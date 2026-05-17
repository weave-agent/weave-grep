package grep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weave-agent/weave/sdk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegister(t *testing.T) {
	tool, err := sdk.GetTool("grep", nil)
	require.NoError(t, err)
	assert.Equal(t, "grep", tool.Name())
}

func TestDefinition(t *testing.T) {
	tool := &tool{}
	def := tool.Definition()
	assert.Equal(t, "grep", def.Name)
	assert.NotNil(t, def.Parameters)
}

func TestDefinitionHasInclude(t *testing.T) {
	tool := &tool{}
	def := tool.Definition()
	params := def.Parameters.(map[string]any)
	props := params["properties"].(map[string]any)
	_, hasInclude := props["include"]
	assert.True(t, hasInclude, "definition should have 'include' parameter")
}

func createTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	return path
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) string // returns path
		args      map[string]any
		wantError bool
		check     func(t *testing.T, result sdk.ToolResult)
	}{
		{
			name:      "missing pattern",
			setup:     func(t *testing.T) string { return "." },
			args:      map[string]any{},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "pattern is required")
			},
		},
		{
			name:  "simple match",
			setup: func(t *testing.T) string { return createTempFile(t, "hello world\nfoo bar\nhello again") },
			args:  map[string]any{"pattern": "hello"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "hello world")
				assert.Contains(t, result.Content, "hello again")
				assert.NotContains(t, result.Content, "foo bar")
			},
		},
		{
			name:  "no match",
			setup: func(t *testing.T) string { return createTempFile(t, "hello world\nfoo bar") },
			args:  map[string]any{"pattern": "notfound"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "no matches found")
			},
		},
		{
			name:  "case-insensitive",
			setup: func(t *testing.T) string { return createTempFile(t, "Hello World\nfoo bar") },
			args:  map[string]any{"pattern": "hello", "ignoreCase": true},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "Hello World")
			},
		},
		{
			name:  "literal mode",
			setup: func(t *testing.T) string { return createTempFile(t, "test (foo)\ntest bar") },
			args:  map[string]any{"pattern": "(foo)", "literal": true},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "test (foo)")
				assert.NotContains(t, result.Content, "test bar")
			},
		},
		{
			name: "context lines",
			setup: func(t *testing.T) string {
				return createTempFile(t, "line1\nline2\nMATCH\nline4\nline5")
			},
			args: map[string]any{"pattern": "MATCH", "context": float64(1)},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "line2")
				assert.Contains(t, result.Content, "MATCH")
				assert.Contains(t, result.Content, "line4")
				assert.NotContains(t, result.Content, "line1")
				assert.NotContains(t, result.Content, "line5")
			},
		},
		{
			name:      "invalid regex",
			setup:     func(t *testing.T) string { return createTempFile(t, "hello") },
			args:      map[string]any{"pattern": "[invalid"},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "invalid pattern")
			},
		},
		{
			name:      "nonexistent path",
			setup:     func(t *testing.T) string { return "/nonexistent/path/xyz" },
			args:      map[string]any{"pattern": "test", "path": "/nonexistent/path/xyz"},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "error:")
			},
		},
		{
			name: "directory search",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("findme in a"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("no match here"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("findme in c"), 0o644))

				return dir
			},
			args: map[string]any{"pattern": "findme"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "findme in a")
				assert.Contains(t, result.Content, "findme in c")
				assert.NotContains(t, result.Content, "no match here")
			},
		},
		{
			name: "skips ignored directories",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("findme git"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.txt"), []byte("findme main"), 0o644))

				return dir
			},
			args: map[string]any{"pattern": "findme"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "findme main")
				assert.NotContains(t, result.Content, "findme git")
			},
		},
		{
			name: "long line is truncated",
			setup: func(t *testing.T) string {
				longLine := strings.Repeat("x", 2*1024*1024)
				return createTempFile(t, "before\nTARGET"+longLine+"\nafter")
			},
			args: map[string]any{"pattern": "TARGET"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.NotContains(t, result.Content, "no matches found")
				assert.Less(t, len(result.Content), 2*1024*1024, "output should be truncated, not full 2MB")
			},
		},
		{
			name: "include glob filter",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "code.go"), []byte("findme go"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("findme txt"), 0o644))

				return dir
			},
			args: map[string]any{"pattern": "findme", "include": "*.go"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "findme go")
				assert.NotContains(t, result.Content, "findme txt")
			},
		},
		{
			name: "include brace pattern",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "a.ts"), []byte("findme ts"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "b.tsx"), []byte("findme tsx"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "c.go"), []byte("findme go"), 0o644))

				return dir
			},
			args: map[string]any{"pattern": "findme", "include": "*.{ts,tsx}"},
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "findme ts")
				assert.Contains(t, result.Content, "findme tsx")
				assert.NotContains(t, result.Content, "findme go")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)

			args := tt.args
			if _, ok := args["path"]; !ok {
				args["path"] = path
			}

			result, err := (&tool{}).Execute(context.Background(), args)
			require.NoError(t, err)
			assert.Equal(t, tt.wantError, result.IsError)

			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

func TestExecuteSandboxDenied(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("findme"), 0o644))

	sb := &testSandboxer{allowReadFn: func(p string) bool { return false }}
	setSandboxer(sb)

	t.Cleanup(func() { setSandboxer(nil) })

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "sandbox: read denied")
}

func TestExecuteSandboxAllowed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readable.txt"), []byte("findme here"), 0o644))

	sb := &testSandboxer{allowReadFn: func(p string) bool { return true }}
	setSandboxer(sb)

	t.Cleanup(func() { setSandboxer(nil) })

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "findme here")
}

func TestExecuteSandboxNil(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "normal.txt"), []byte("findme normal"), 0o644))

	setSandboxer(nil)

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "findme normal")
}

func TestLineTruncation(t *testing.T) {
	longContent := "prefix " + strings.Repeat("x", 1000) + " suffix"
	line := "file.txt:42:" + longContent
	truncated := truncateLine(line)

	assert.Less(t, len(truncated), len(line), "truncated line should be shorter")
	assert.Contains(t, truncated, "chars truncated")
	assert.True(t, strings.HasPrefix(truncated, "file.txt:42:prefix "))
}

func TestLineTruncationShort(t *testing.T) {
	line := "file.txt:1:short content"
	assert.Equal(t, line, truncateLine(line), "short lines should not be truncated")
}

func TestBinaryFileSkipped(t *testing.T) {
	dir := t.TempDir()

	// Write a binary file (PNG header)
	binaryContent := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00}
	binPath := filepath.Join(dir, "image.png")
	require.NoError(t, os.WriteFile(binPath, binaryContent, 0o644))

	txtPath := filepath.Join(dir, "readme.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("findme text"), 0o644))

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.NotContains(t, result.Content, "image.png")
	// Text file should still be found (via rg or fallback)
}

func TestRgPathWithRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not in PATH")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("hello go"), 0o644))

	// Test rg path works with include filter
	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "hello",
		"path":    dir,
		"include": "*.go",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "hello go")
	assert.NotContains(t, result.Content, "hello world")
}

func TestRespectGitignore(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not in PATH")
	}

	dir := t.TempDir()

	// Initialize a git repo so rg respects .gitignore
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("findme ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("findme visible"), 0o644))

	// Initialize git repo so rg finds the .gitignore
	gitCmd := exec.Command("git", "init")
	gitCmd.Dir = dir
	require.NoError(t, gitCmd.Run())

	// With respect_gitignore = true (default), ignored files should be skipped
	cfg := &testConfig{respectGitignore: true}
	result, err := (&tool{cfg: cfg}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.NotContains(t, result.Content, "findme ignored")
	assert.Contains(t, result.Content, "findme visible")
}

func TestNoRespectGitignore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("findme ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("findme visible"), 0o644))

	// With respect_gitignore = false, ignored files should be found
	cfg := &testConfig{respectGitignore: false}
	result, err := (&tool{cfg: cfg}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "findme visible")
	// Both rg (--no-ignore) and stdlib fallback (no .gitignore parsing) find the ignored file
	assert.Contains(t, result.Content, "findme ignored")
}

type testSandboxer struct {
	allowReadFn  func(string) bool
	allowWriteFn func(string) bool
	wrapFn       func(cmd, dir string) (string, error)
}

func (ts *testSandboxer) WrapCommand(cmd, dir string) (string, error) {
	if ts.wrapFn != nil {
		return ts.wrapFn(cmd, dir)
	}

	return cmd, nil
}

func (ts *testSandboxer) AllowWrite(path string) bool {
	if ts.allowWriteFn != nil {
		return ts.allowWriteFn(path)
	}

	return true
}

func (ts *testSandboxer) AllowRead(path string) bool {
	if ts.allowReadFn != nil {
		return ts.allowReadFn(path)
	}

	return true
}

func (ts *testSandboxer) Mode() string   { return "auto" }
func (ts *testSandboxer) SetMode(string) {}

func TestRgWithSandboxerFiltersDeniedPaths(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not in PATH")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.txt"), []byte("findme public"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("findme secret"), 0o644))

	sb := &testSandboxer{allowReadFn: func(p string) bool {
		return !strings.Contains(p, "secret")
	}}
	setSandboxer(sb)

	t.Cleanup(func() { setSandboxer(nil) })

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "findme public")
	assert.NotContains(t, result.Content, "findme secret")
}

type testConfig struct {
	respectGitignore bool
}

func (testConfig) FilePath() string                         { return "" }
func (testConfig) ProjectDir() string                       { return "" }
func (testConfig) ExtensionConfig(_, _ string, _ any) error { return nil }
func (testConfig) IsHeadless() bool                         { return false }
func (testConfig) Preferences(any) error                    { return nil }
func (testConfig) SavePreferences(any) error                { return nil }
func (testConfig) SaveProviderKey(_, _ string) error        { return nil }
func (tc testConfig) RespectGitignore() bool                { return tc.respectGitignore }
