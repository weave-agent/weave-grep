package grep

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/weave-agent/weave/sdk"
	"github.com/weave-agent/weave/utils/ripgrep"
	"github.com/weave-agent/weave/utils/truncate"
)

const (
	ParamPattern = "pattern"
	paramPath    = "path"
	paramInclude = "include"
	jsonType     = "type"
	maxLineLen   = 500
)

type tool struct {
	cfg sdk.Config
}

var (
	sandboxerMu sync.RWMutex
	sandboxer   sdk.Sandboxer
)

func setSandboxer(s sdk.Sandboxer) {
	sandboxerMu.Lock()
	sandboxer = s
	sandboxerMu.Unlock()
}

func getSandboxer() sdk.Sandboxer {
	sandboxerMu.RLock()

	s := sandboxer

	sandboxerMu.RUnlock()

	return s
}

func init() {
	sdk.OnBusReady(func(bus sdk.Bus) {
		bus.On("sandbox.registered", func(ev sdk.Event) error {
			if s, ok := ev.Payload.(sdk.Sandboxer); ok {
				setSandboxer(s)
			}

			return nil
		})
	})

	sdk.RegisterTool[struct{}]("grep", func(cfg sdk.Config, _ sdk.PreferenceReader, _ struct{}) (sdk.Tool, error) {
		return &tool{cfg: cfg}, nil
	})
}

func (t *tool) Name() string { return "grep" }

func (t *tool) Definition() sdk.ToolDef {
	return sdk.ToolDef{
		Name:        "grep",
		Description: "Search files for a pattern using regular expressions. Uses ripgrep when available for .gitignore support and faster searches; falls back to pure Go when rg is absent. Returns matching file:line:content entries.",
		Parameters: map[string]any{
			jsonType: "object",
			"properties": map[string]any{
				ParamPattern: map[string]any{
					jsonType:      "string",
					"description": "The regular expression pattern to search for.",
				},
				paramPath: map[string]any{
					jsonType:      "string",
					"description": "File or directory to search. Defaults to current directory.",
				},
				paramInclude: map[string]any{
					jsonType:      "string",
					"description": "Glob filter to limit search to matching files (e.g. \"*.go\", \"*.{ts,tsx}\").",
				},
				"ignoreCase": map[string]any{
					jsonType:      "boolean",
					"description": "Case-insensitive matching. Defaults to false.",
				},
				"literal": map[string]any{
					jsonType:      "boolean",
					"description": "Treat pattern as a literal string instead of regex. Defaults to false.",
				},
				"context": map[string]any{
					jsonType:      "number",
					"description": "Number of context lines before and after each match. Defaults to 0.",
				},
			},
			"required":             []string{ParamPattern},
			"additionalProperties": false,
		},
	}
}

func (t *tool) Execute(ctx context.Context, args map[string]any) (sdk.ToolResult, error) {
	pattern, _ := args[ParamPattern].(string)
	if pattern == "" {
		return sdk.ToolResult{Content: "error: pattern is required", IsError: true}, nil
	}

	path, _ := args[paramPath].(string)
	if path == "" {
		path = "."
	}

	include, _ := args[paramInclude].(string)
	ignoreCase, _ := args["ignoreCase"].(bool)
	literal, _ := args["literal"].(bool)

	if include != "" {
		if _, matchErr := filepath.Match(include, ""); matchErr != nil {
			return sdk.ToolResult{Content: fmt.Sprintf("error: invalid include pattern: %s", matchErr), IsError: true}, nil
		}
	}

	var contextLines int

	if v, ok := args["context"]; ok {
		if f, ok := v.(float64); ok && f >= 0 {
			contextLines = min(int(f), 50)
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return sdk.ToolResult{Content: fmt.Sprintf("error: %s", err), IsError: true}, nil
	}

	if s := getSandboxer(); s != nil && !s.AllowRead(absPath) {
		return sdk.ToolResult{Content: "sandbox: read denied — path is protected", IsError: true}, nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return sdk.ToolResult{Content: fmt.Sprintf("error: %s", err), IsError: true}, nil
	}

	// Validate regex early so both rg and stdlib paths get consistent error handling
	expr := pattern
	if literal {
		expr = regexp.QuoteMeta(pattern)
	}

	if ignoreCase {
		expr = "(?i)" + expr
	}

	if _, compileErr := regexp.Compile(expr); compileErr != nil {
		return sdk.ToolResult{Content: fmt.Sprintf("error: invalid pattern: %s", compileErr), IsError: true}, nil
	}

	respectGitignore := true
	if t.cfg != nil {
		respectGitignore = t.cfg.RespectGitignore()
	}

	matches := t.search(ctx, absPath, info.IsDir(), pattern, include, ignoreCase, literal, contextLines, respectGitignore)
	if len(matches) == 0 {
		return sdk.ToolResult{Content: "no matches found", IsError: false}, nil
	}

	for i, m := range matches {
		matches[i] = truncateLine(m)
	}

	output := strings.Join(matches, "\n")
	result := truncate.Truncate(output, truncate.DefaultMaxLines, truncate.DefaultMaxBytes)

	return sdk.ToolResult{Content: result.Format(), IsError: false}, nil
}

// search tries rg first, then falls back to stdlib.
func (t *tool) search(ctx context.Context, absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool) []string {
	// Use rg when available. Matches from denied paths are filtered by
	// AllowRead checks in parseRgJSON.
	if rgPath := ripgrep.Find(); rgPath != "" {
		matches, err := searchWithRipgrep(ctx, rgPath, absPath, isDir, pattern, include, ignoreCase, literal, contextLines, respectGitignore)
		if err == nil {
			return matches
		}
	}

	return searchWithStdlib(absPath, isDir, pattern, include, ignoreCase, literal, contextLines, respectGitignore)
}

func searchWithStdlib(absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool) []string {
	expr := pattern
	if literal {
		expr = regexp.QuoteMeta(pattern)
	}

	if ignoreCase {
		expr = "(?i)" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return nil
	}

	if isDir {
		matches, dirErr := searchDir(absPath, re, contextLines, include, respectGitignore)
		if dirErr != nil {
			return nil
		}

		return matches
	}

	if !fileMatchesInclude(include, filepath.Base(absPath)) {
		return nil
	}

	return searchFile(filepath.Base(absPath), absPath, re, contextLines)
}

func searchWithRipgrep(ctx context.Context, rgPath, absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool) ([]string, error) {
	args := []string{"--json", "-H", "-n", "--hidden"}

	if ignoreCase {
		args = append(args, "-i")
	}

	if literal {
		args = append(args, "-F")
	}

	// Do not pass --glob to rg — it overrides gitignore logic.
	// Instead, filter include in Go after parsing rg output.

	if !respectGitignore {
		args = append(args, "--no-ignore")
	}

	if contextLines > 0 {
		args = append(args, "-C", strconv.Itoa(contextLines))
	}

	args = append(args, "--", pattern)

	searchPath := absPath
	if !isDir {
		searchPath = filepath.Dir(absPath)
		args = append(args, filepath.Base(absPath))
	} else {
		args = append(args, ".")
	}

	cmd := exec.CommandContext(ctx, rgPath, args...)
	cmd.Dir = searchPath

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return []string{}, nil
		}

		return nil, fmt.Errorf("rg: %w", err)
	}

	return parseRgJSON(out, searchPath, include, respectGitignore)
}

func parseRgJSON(data []byte, baseDir, include string, respectGitignore bool) ([]string, error) {
	var matches []string

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		var entry struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}

		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		if entry.Type != "match" && entry.Type != "context" {
			continue
		}

		relPath := entry.Data.Path.Text

		// rg outputs paths relative to its CWD (baseDir), so clean directly
		if !filepath.IsAbs(relPath) {
			relPath = filepath.Clean(relPath)
		}

		// Skip VCS and dependency directories (matches stdlib isSkipDir behavior)
		if respectGitignore && isSkipPath(relPath) {
			continue
		}

		if s := getSandboxer(); s != nil && !s.AllowRead(filepath.Join(baseDir, relPath)) {
			continue
		}

		if !fileMatchesInclude(include, filepath.Base(relPath)) {
			continue
		}

		content := strings.TrimRight(entry.Data.Lines.Text, "\n\r")
		matches = append(matches, fmt.Sprintf("%s:%d:%s", relPath, entry.Data.LineNumber, content))
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return nil, fmt.Errorf("parsing rg output: %w", scanErr)
	}

	return matches, nil
}

func searchDir(root string, re *regexp.Regexp, contextLines int, include string, respectGitignore bool) ([]string, error) {
	var matches []string

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // walkErr intentionally swallowed to skip inaccessible entries
		}

		if d.IsDir() {
			name := d.Name()
			if respectGitignore && isSkipDir(name) {
				return filepath.SkipDir
			}

			return nil
		}

		if !fileMatchesInclude(include, d.Name()) {
			return nil
		}

		if s := getSandboxer(); s != nil && !s.AllowRead(walkPath) {
			return nil
		}

		relPath, _ := filepath.Rel(root, walkPath)
		fileMatches := searchFile(relPath, walkPath, re, contextLines)
		matches = append(matches, fileMatches...)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("grep: walk directory: %w", err)
	}

	return matches, nil
}

func fileMatchesInclude(include, name string) bool {
	if include == "" {
		return true
	}

	// Try direct match first
	if matched, _ := filepath.Match(include, name); matched {
		return true
	}

	// Handle brace patterns like *.{ts,tsx} by expanding alternatives
	if idx := strings.Index(include, "{"); idx != -1 {
		closeIdx := strings.Index(include, "}")
		if closeIdx > idx {
			prefix := include[:idx]
			suffix := include[closeIdx+1:]

			for alt := range strings.SplitSeq(include[idx+1:closeIdx], ",") {
				expanded := prefix + strings.TrimSpace(alt) + suffix
				if matched, _ := filepath.Match(expanded, name); matched {
					return true
				}
			}
		}
	}

	return false
}

func searchFile(displayPath, filePath string, re *regexp.Regexp, contextLines int) []string {
	fi, err := os.Stat(filePath)
	if err != nil || fi.Size() > 10*1024*1024 {
		return nil
	}

	if isBinaryFile(filePath) {
		return nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil
	}

	var results []string

	matched := make(map[int]bool)

	for i, line := range lines {
		if re.MatchString(line) {
			for j := max(0, i-contextLines); j <= min(len(lines)-1, i+contextLines); j++ {
				matched[j] = true
			}
		}
	}

	for i := range lines {
		if matched[i] {
			results = append(results, fmt.Sprintf("%s:%d:%s", displayPath, i+1, lines[i]))
		}
	}

	return results
}

func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()

	buf := make([]byte, 512)

	n, err := f.Read(buf)
	if err != nil {
		return true
	}

	contentType := http.DetectContentType(buf[:n])

	return !strings.HasPrefix(contentType, "text/")
}

func truncateLine(line string) string {
	runes := []rune(line)
	if len(runes) <= maxLineLen {
		return line
	}

	return string(runes[:maxLineLen]) + fmt.Sprintf("... [%d chars truncated]", len(runes)-maxLineLen)
}

func isSkipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == ".hg" || name == ".svn"
}

// isSkipPath returns true if the relative path is under a VCS or dependency directory.
func isSkipPath(rel string) bool {
	return slices.ContainsFunc(strings.Split(rel, string(filepath.Separator)), isSkipDir)
}
