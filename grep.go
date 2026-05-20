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
	"time"

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

//nolint:gochecknoinits // SDK requires init() for tool registration
func init() {
	sdk.OnBusReady(func(bus sdk.Bus) {
		bus.On("sandbox.registered", func(ev sdk.Event) error {
			if s, ok := ev.Payload.(sdk.Sandboxer); ok {
				setSandboxer(s)
			}

			return nil
		})
	})

	sdk.RegisterTool("grep", func(cfg sdk.Config, _ sdk.PreferenceReader, _ struct{}) (sdk.Tool, error) {
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

type searchParams struct {
	pattern          string
	absPath          string
	isDir            bool
	include          string
	ignoreCase       bool
	literal          bool
	contextLines     int
	respectGitignore bool
}

func (t *tool) parseSearchParams(args map[string]any) (searchParams, sdk.ToolResult) {
	pattern, _ := args[ParamPattern].(string)
	if pattern == "" {
		return searchParams{}, sdk.ToolResult{Content: "error: pattern is required", IsError: true}
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
			return searchParams{}, sdk.ToolResult{Content: fmt.Sprintf("error: invalid include pattern: %s", matchErr), IsError: true}
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
		return searchParams{}, sdk.ToolResult{Content: fmt.Sprintf("error: %s", err), IsError: true}
	}

	if s := getSandboxer(); s != nil && !s.AllowRead(absPath) {
		return searchParams{}, sdk.ToolResult{Content: "sandbox: read denied — path is protected", IsError: true}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return searchParams{}, sdk.ToolResult{Content: fmt.Sprintf("error: %s", err), IsError: true}
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
		return searchParams{}, sdk.ToolResult{Content: fmt.Sprintf("error: invalid pattern: %s", compileErr), IsError: true}
	}

	respectGitignore := true
	if t.cfg != nil {
		respectGitignore = t.cfg.RespectGitignore()
	}

	return searchParams{
		pattern:          pattern,
		absPath:          absPath,
		isDir:            info.IsDir(),
		include:          include,
		ignoreCase:       ignoreCase,
		literal:          literal,
		contextLines:     contextLines,
		respectGitignore: respectGitignore,
	}, sdk.ToolResult{}
}

func (t *tool) Execute(ctx context.Context, args map[string]any) (sdk.ToolResult, error) {
	params, errResult := t.parseSearchParams(args)
	if errResult.IsError {
		return errResult, nil
	}

	// Set up streaming progress reporter
	bus := sdk.BusFromContext(ctx)

	var collector *progressCollector
	if bus != nil {
		collector = newProgressCollector(ctx, bus)
	}

	matches, searchErr := t.search(ctx, params.absPath, params.isDir, params.pattern, params.include, params.ignoreCase, params.literal, params.contextLines, params.respectGitignore, func(file, _ string) {
		if collector != nil {
			collector.add(file)
		}
	})

	// Publish final progress event
	if collector != nil {
		if collector.cancel != nil {
			collector.cancel()
		}

		collector.publish()
	}

	if searchErr != nil {
		return sdk.ToolResult{Content: fmt.Sprintf("error: %v", searchErr), IsError: true}, nil
	}

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

// progressCollector accumulates match count and publishes throttled progress events.
type progressCollector struct {
	mu          sync.Mutex
	matchCount  int
	currentFile string
	bus         sdk.Bus
	throttle    func()
	cancel      func()
}

func newProgressCollector(ctx context.Context, bus sdk.Bus) *progressCollector {
	pc := &progressCollector{bus: bus}
	if bus != nil {
		throttleCtx, cancel := context.WithCancel(ctx)
		pc.cancel = cancel
		pc.throttle = sdk.Throttle(throttleCtx, func() {
			pc.publish()
		}, 200*time.Millisecond)
	}

	return pc
}

func (pc *progressCollector) add(file string) {
	pc.mu.Lock()
	pc.matchCount++

	if file != "" {
		pc.currentFile = file
	}
	pc.mu.Unlock()

	if pc.throttle != nil {
		pc.throttle()
	}
}

func (pc *progressCollector) publish() {
	pc.mu.Lock()
	count := pc.matchCount
	file := pc.currentFile
	pc.mu.Unlock()

	if pc.bus == nil || count == 0 {
		return
	}

	content := fmt.Sprintf("Found %d matches", count)
	if file != "" {
		content += " in " + file
	}

	content += "..."

	pc.bus.Publish(sdk.NewEvent(sdk.TopicToolProgress, sdk.ToolProgress{
		ToolName: "grep",
		Content:  content,
	}))
}

// search tries rg first, then falls back to stdlib.
func (t *tool) search(ctx context.Context, absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool, onMatch func(file, match string)) ([]string, error) {
	// Use rg when available. Matches from denied paths are filtered by
	// AllowRead checks in parseRgLine.
	if rgPath := ripgrep.Find(); rgPath != "" {
		matches, err := searchWithRipgrep(ctx, rgPath, absPath, isDir, pattern, include, ignoreCase, literal, contextLines, respectGitignore, onMatch)
		if err == nil {
			return matches, nil
		}
	}

	return searchWithStdlib(ctx, absPath, isDir, pattern, include, ignoreCase, literal, contextLines, respectGitignore, onMatch)
}

func searchWithStdlib(ctx context.Context, absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool, onMatch func(file, match string)) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search canceled: %w", err)
	}

	expr := pattern
	if literal {
		expr = regexp.QuoteMeta(pattern)
	}

	if ignoreCase {
		expr = "(?i)" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("compile regex: %w", err)
	}

	if isDir {
		return searchDir(ctx, absPath, re, contextLines, include, respectGitignore, onMatch)
	}

	if !fileMatchesInclude(include, filepath.Base(absPath)) {
		return nil, nil
	}

	return searchFile(ctx, filepath.Base(absPath), absPath, re, contextLines, onMatch), nil
}

func searchWithRipgrep(ctx context.Context, rgPath, absPath string, isDir bool, pattern, include string, ignoreCase, literal bool, contextLines int, respectGitignore bool, onMatch func(file, match string)) ([]string, error) {
	args, searchPath := buildRgArgs(absPath, isDir, pattern, ignoreCase, literal, contextLines, respectGitignore)

	cmd := exec.CommandContext(ctx, rgPath, args...)
	cmd.Dir = searchPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rg stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()

		return nil, fmt.Errorf("rg start: %w", err)
	}

	var matches []string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()

			return matches, nil //nolint:nilerr // return partial matches on cancellation
		}

		file, match, isMatch := parseRgLine(scanner.Bytes(), searchPath, include, respectGitignore)
		if match != "" {
			matches = append(matches, match)
			if onMatch != nil && isMatch {
				onMatch(file, match)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return handleRgScannerErr(ctx, cmd, err, matches)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return matches, nil //nolint:nilerr // return partial matches on cancellation
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return matches, nil
		}

		return nil, fmt.Errorf("rg: %w", err)
	}

	return matches, nil
}

func buildRgArgs(absPath string, isDir bool, pattern string, ignoreCase, literal bool, contextLines int, respectGitignore bool) ([]string, string) {
	args := []string{"--json", "-H", "-n", "--hidden"}

	if ignoreCase {
		args = append(args, "-i")
	}

	if literal {
		args = append(args, "-F")
	}

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

	return args, searchPath
}

func handleRgScannerErr(ctx context.Context, cmd *exec.Cmd, scanErr error, matches []string) ([]string, error) {
	// Kill the process to prevent deadlock if rg is blocked writing to stdout
	// after the scanner stopped reading (e.g. token exceeded buffer limit).
	_ = cmd.Process.Kill()

	if werr := cmd.Wait(); werr != nil {
		if ctx.Err() != nil {
			return matches, nil //nolint:nilerr // return partial matches on cancellation
		}

		var exitErr *exec.ExitError
		if errors.As(werr, &exitErr) && exitErr.ExitCode() == 1 {
			return matches, nil
		}
	}

	if ctx.Err() != nil {
		return matches, nil //nolint:nilerr // return partial matches on cancellation
	}

	return nil, fmt.Errorf("parsing rg output: %w", scanErr)
}

func parseRgLine(line []byte, baseDir, include string, respectGitignore bool) (string, string, bool) {
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
		return "", "", false
	}

	if entry.Type != "match" && entry.Type != "context" {
		return "", "", false
	}

	relPath := entry.Data.Path.Text

	// rg outputs paths relative to its CWD (baseDir) using "/" separators
	// regardless of OS. Ripgrep paths are already clean (no .. or . segments).
	if filepath.IsAbs(relPath) {
		var err error

		relPath, err = filepath.Rel(baseDir, relPath)
		if err != nil {
			return "", "", false
		}
	}

	// Skip VCS and dependency directories (defense-in-depth for --no-ignore)
	if respectGitignore && isSkipPath(relPath) {
		return "", "", false
	}

	if s := getSandboxer(); s != nil && !s.AllowRead(filepath.Join(baseDir, relPath)) {
		return "", "", false
	}

	if !fileMatchesInclude(include, filepath.Base(relPath)) {
		return "", "", false
	}

	content := strings.TrimRight(entry.Data.Lines.Text, "\n\r")

	return relPath, fmt.Sprintf("%s:%d:%s", relPath, entry.Data.LineNumber, content), entry.Type == "match"
}

func searchDir(ctx context.Context, root string, re *regexp.Regexp, contextLines int, include string, respectGitignore bool, onMatch func(file, match string)) ([]string, error) {
	var matches []string

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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
		fileMatches := searchFile(ctx, relPath, walkPath, re, contextLines, onMatch)
		matches = append(matches, fileMatches...)

		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return matches, nil //nolint:nilerr // return partial matches on cancellation
		}

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

func searchFile(ctx context.Context, displayPath, filePath string, re *regexp.Regexp, contextLines int, onMatch func(file, match string)) []string {
	if ctx.Err() != nil {
		return nil
	}

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

		if ctx.Err() != nil {
			break
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
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
			result := fmt.Sprintf("%s:%d:%s", displayPath, i+1, lines[i])
			results = append(results, result)

			if onMatch != nil && re.MatchString(lines[i]) {
				onMatch(displayPath, result)
			}
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
// ripgrep JSON always uses "/" separators regardless of OS.
func isSkipPath(rel string) bool {
	return slices.ContainsFunc(strings.Split(rel, "/"), isSkipDir)
}
