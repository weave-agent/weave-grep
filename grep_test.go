package grep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestGuardianRequest(t *testing.T) {
	req := guardianRequest("/tmp/project")

	assert.NotEmpty(t, req.ID)
	assert.True(t, strings.HasPrefix(req.ID, "grep-guardian-"))
	assert.Equal(t, "grep", req.ToolName)
	assert.Equal(t, sdk.GuardianActionRead, req.Action)
	assert.Equal(t, "/tmp/project", req.Path)
	assert.Equal(t, "Search file contents", req.Description)
	assert.Equal(t, "grep", req.Metadata["operation"])
}

func TestCheckGuardianBlock(t *testing.T) {
	origGuardian := getGuardian()

	g := &testGuardian{
		decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
			assert.Equal(t, "/tmp/secret", req.Path)
			assert.Equal(t, sdk.GuardianActionRead, req.Action)

			return sdk.GuardianDecision{
				ID:      "decision-1",
				Action:  sdk.GuardianDecisionBlock,
				Profile: "custom",
				Reason:  "blocked by test policy",
			}, nil
		},
	}
	setGuardian(g)

	t.Cleanup(func() { setGuardian(origGuardian) })

	req, result := checkGuardian(context.Background(), "/tmp/secret")

	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "guardian: blocked")
	assert.Contains(t, result.Content, "action: read")
	assert.Contains(t, result.Content, "rule: custom")
	assert.Contains(t, result.Content, "reason: blocked by test policy")
	assert.Equal(t, "/tmp/secret", req.Path)
}

func TestAllowSandboxReadPassesGuardianMetadata(t *testing.T) {
	sb := &testSandboxer{}

	allowed, reason := allowSandboxRead(context.Background(), sb, "/tmp/file", "guardian-1")
	assert.True(t, allowed)
	assert.Empty(t, reason)
	require.Len(t, sb.requests, 1)
	assert.Equal(t, "/tmp/file", sb.requests[0].Filesystem[0].Path)
	assert.Equal(t, sdk.SandboxFilesystemRead, sb.requests[0].Filesystem[0].Access)
	assert.Equal(t, "grep", sb.requests[0].Metadata["operation"])
	assert.Equal(t, "guardian-1", sb.requests[0].Metadata["guardian_request_id"])
}

func TestGuardianAndSandboxRegistration(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	bus := newRegistrationBus()
	sdk.InvokeBusSubscribers(bus)

	g := &registrationGuardian{}
	s := &testSandboxer{}

	bus.Publish(sdk.NewEvent(sdk.GuardianRegisteredTopic, g))
	bus.Publish(sdk.NewEvent(sdk.SandboxRegisteredTopic, s))

	assert.Same(t, g, getGuardian())
	assert.Same(t, s, getSandboxer())

	bus.Publish(sdk.NewEvent(sdk.GuardianRegisteredTopic, "not a guardian"))
	bus.Publish(sdk.NewEvent(sdk.SandboxRegisteredTopic, "not a sandboxer"))

	assert.Same(t, g, getGuardian())
	assert.Same(t, s, getSandboxer())
}

func TestExecuteWithGuardian(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	t.Run("allow decision permits grep", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "readable.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme here\nskip"), 0o644))

		var gotReq sdk.GuardianRequest

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				gotReq = req

				return sdk.GuardianDecision{
					ID:        "decision-allow",
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionAllow,
				}, nil
			},
		})
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "findme here")

		assert.NotEmpty(t, gotReq.ID)
		assert.Equal(t, "grep", gotReq.ToolName)
		assert.Equal(t, sdk.GuardianActionRead, gotReq.Action)
		assert.Equal(t, path, gotReq.Path)
		assert.Equal(t, "Search file contents", gotReq.Description)
		assert.Equal(t, "grep", gotReq.Metadata["operation"])
	})

	t.Run("guardian receives absolute path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "relative.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme relative"), 0o644))

		cwd, err := os.Getwd()
		require.NoError(t, err)

		relPath, err := filepath.Rel(cwd, path)
		require.NoError(t, err)
		require.NotEqual(t, path, relPath)

		var gotPath string

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				gotPath = req.Path

				return sdk.GuardianDecision{
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionAllow,
				}, nil
			},
		})
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    relPath,
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "findme relative")
		assert.Equal(t, path, gotPath)
	})

	t.Run("block decision returns guardian error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secret.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme secret"), 0o644))

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				return sdk.GuardianDecision{
					ID:        "decision-block",
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionBlock,
					Profile:   "strict",
					Reason:    "blocked by policy",
				}, nil
			},
		})
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "guardian: blocked")
		assert.Contains(t, result.Content, "action: read")
		assert.Contains(t, result.Content, "rule: strict")
		assert.Contains(t, result.Content, "reason: blocked by policy")
	})

	t.Run("ask decision returns guardian error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ask.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme ask"), 0o644))

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				return sdk.GuardianDecision{
					ID:        "decision-ask",
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionAsk,
				}, nil
			},
		})
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "guardian: blocked")
		assert.Contains(t, result.Content, "action: read")
		assert.Contains(t, result.Content, "rule: decision-ask")
		assert.Contains(t, result.Content, "reason: guardian returned unresolved approval decision")
		assert.NotContains(t, result.Content, "findme ask")
	})

	t.Run("missing guardian permits grep", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "normal.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme normal"), 0o644))

		setGuardian(nil)
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "findme normal")
	})

	t.Run("guardian error returns tool error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "blocked.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme blocked"), 0o644))

		setGuardian(&testGuardian{
			decideFn: func(context.Context, sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				return sdk.GuardianDecision{}, errors.New("policy engine unavailable")
			},
		})
		setSandboxer(nil)

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "guardian: policy engine unavailable")
	})
}

func TestExecutePassesGuardianMetadataToSandbox(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.txt"), []byte("findme one"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.txt"), []byte("findme two"), 0o644))

	var rootReq sdk.GuardianRequest

	setGuardian(&testGuardian{
		decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
			if req.Path == dir {
				rootReq = req
			}

			return sdk.GuardianDecision{
				ID:        "decision-allow",
				RequestID: req.ID,
				Action:    sdk.GuardianDecisionAllow,
			}, nil
		},
	})

	sb := &testSandboxer{}
	setSandboxer(sb)

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "findme")

	require.NotEmpty(t, rootReq.ID)
	require.NotEmpty(t, sb.requests)

	for _, req := range sb.requests {
		require.Len(t, req.Filesystem, 1)
		assert.NotEmpty(t, req.Filesystem[0].Path)
		assert.Equal(t, sdk.SandboxFilesystemRead, req.Filesystem[0].Access)
		assert.Equal(t, "grep", req.Metadata["operation"])
		assert.NotEmpty(t, req.Metadata["guardian_request_id"])
	}

	assert.Equal(t, rootReq.ID, sb.requests[0].Metadata["guardian_request_id"])
}

func TestExecuteGuardianBlocksDirectoryChildBeforeRead(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	dir := t.TempDir()
	allowedPath := filepath.Join(dir, "allowed.txt")
	blockedPath := filepath.Join(dir, "secret.txt")

	require.NoError(t, os.WriteFile(allowedPath, []byte("findme allowed"), 0o644))
	require.NoError(t, os.WriteFile(blockedPath, []byte("findme secret"), 0o644))

	var checkedPaths []string

	setGuardian(&testGuardian{
		decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
			checkedPaths = append(checkedPaths, req.Path)
			if req.Path == blockedPath {
				return sdk.GuardianDecision{
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionBlock,
					Reason:    "blocked child",
				}, nil
			}

			return sdk.GuardianDecision{
				RequestID: req.ID,
				Action:    sdk.GuardianDecisionAllow,
			}, nil
		},
	})

	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "guardian: blocked")
	assert.Contains(t, result.Content, "reason: blocked child")
	assert.NotContains(t, result.Content, "findme secret")
	assert.Contains(t, checkedPaths, dir)
	assert.Contains(t, checkedPaths, allowedPath)
	assert.Contains(t, checkedPaths, blockedPath)
}

func TestExecuteGuardianSandboxOrdering(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	t.Run("guardian allow runs before sandbox", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ordered.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme ordered"), 0o644))

		var order []string

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				order = append(order, "guardian")

				return sdk.GuardianDecision{
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionAllow,
				}, nil
			},
		})
		setSandboxer(&testSandboxer{
			allowReadFn: func(string) bool {
				order = append(order, "sandbox")

				return true
			},
		})

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "findme ordered")
		require.GreaterOrEqual(t, len(order), 2)
		assert.Equal(t, []string{"guardian", "sandbox"}, order[:2])
	})

	t.Run("guardian block skips sandbox", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "blocked.txt")
		require.NoError(t, os.WriteFile(path, []byte("findme blocked"), 0o644))

		var order []string

		setGuardian(&testGuardian{
			decideFn: func(_ context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
				order = append(order, "guardian")

				return sdk.GuardianDecision{
					RequestID: req.ID,
					Action:    sdk.GuardianDecisionBlock,
					Reason:    "blocked before sandbox",
				}, nil
			},
		})
		setSandboxer(&testSandboxer{
			allowReadFn: func(string) bool {
				order = append(order, "sandbox")

				return true
			},
		})

		result, err := (&tool{}).Execute(context.Background(), map[string]any{
			"pattern": "findme",
			"path":    path,
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "guardian: blocked")
		assert.Equal(t, []string{"guardian"}, order)
	})
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
	assert.Contains(t, result.Content, "findme text")
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

func TestRespectGitignoreWithGuardian(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not in PATH")
	}

	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(&testGuardian{})
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("findme ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("findme visible"), 0o644))

	gitCmd := exec.Command("git", "init")
	gitCmd.Dir = dir
	require.NoError(t, gitCmd.Run())

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

func TestCleanRgFilePathSkipsVCSDependencyDirs(t *testing.T) {
	tests := []string{
		".git/config",
		"node_modules/pkg/index.js",
		"src/.hg/cache",
		"src/.svn/entries",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			_, ok := cleanRgFilePath(path, "", true)
			assert.False(t, ok)
		})
	}

	relPath, ok := cleanRgFilePath(".git/config", "", false)
	assert.True(t, ok)
	assert.Equal(t, filepath.Clean(".git/config"), relPath)
}

type testSandboxer struct {
	allowReadFn        func(string) bool
	requestExpansionFn func(context.Context, sdk.SandboxExpansionRequest) (sdk.SandboxExpansion, error)
	requests           []sdk.SandboxExpansionRequest
}

func (ts *testSandboxer) WrapCommand(context.Context, sdk.SandboxCommandRequest) (sdk.SandboxCommand, error) {
	return sdk.SandboxCommand{}, nil
}

func (ts *testSandboxer) Status(context.Context) (sdk.SandboxStatus, error) {
	return sdk.SandboxStatus{}, nil
}

func (ts *testSandboxer) RequestExpansion(ctx context.Context, req sdk.SandboxExpansionRequest) (sdk.SandboxExpansion, error) {
	ts.requests = append(ts.requests, req)

	if ts.requestExpansionFn != nil {
		return ts.requestExpansionFn(ctx, req)
	}

	allowed := true

	if ts.allowReadFn != nil {
		for _, fs := range req.Filesystem {
			if fs.Access == sdk.SandboxFilesystemRead && !ts.allowReadFn(fs.Path) {
				allowed = false
				break
			}
		}
	}

	if !allowed {
		return sdk.SandboxExpansion{RequestID: req.ID, State: sdk.SandboxExpansionDenied, Reason: "path is protected"}, nil
	}

	return sdk.SandboxExpansion{RequestID: req.ID, State: sdk.SandboxExpansionAllowed}, nil
}

func (ts *testSandboxer) ResolveExpansion(context.Context, string, sdk.SandboxExpansionResolution) error {
	return nil
}

type registrationGuardian struct{}

func (g *registrationGuardian) Decide(context.Context, sdk.GuardianRequest) (sdk.GuardianDecision, error) {
	return sdk.GuardianDecision{Action: sdk.GuardianDecisionAllow}, nil
}

func (g *registrationGuardian) Resolve(context.Context, string, sdk.GuardianResolution) error {
	return nil
}

func (g *registrationGuardian) Snapshot(context.Context) (sdk.GuardianSnapshot, error) {
	return sdk.GuardianSnapshot{}, nil
}

type registrationBus struct {
	handlers map[string][]sdk.Handler
}

func newRegistrationBus() *registrationBus {
	return &registrationBus{handlers: make(map[string][]sdk.Handler)}
}

func (r *registrationBus) Publish(ev sdk.Event) {
	for _, h := range r.handlers[ev.Topic] {
		_ = h(ev)
	}
}

func (r *registrationBus) On(topic string, h sdk.Handler) {
	r.handlers[topic] = append(r.handlers[topic], h)
}

func (r *registrationBus) OnAll(sdk.Handler) {}

func (r *registrationBus) Off(sdk.Handler) {}

func (r *registrationBus) Close() error { return nil }

type testGuardian struct {
	decideFn func(context.Context, sdk.GuardianRequest) (sdk.GuardianDecision, error)
}

func (g *testGuardian) Decide(ctx context.Context, req sdk.GuardianRequest) (sdk.GuardianDecision, error) {
	if g.decideFn != nil {
		return g.decideFn(ctx, req)
	}

	return sdk.GuardianDecision{Action: sdk.GuardianDecisionAllow}, nil
}

func (g *testGuardian) Resolve(context.Context, string, sdk.GuardianResolution) error {
	return nil
}

func (g *testGuardian) Snapshot(context.Context) (sdk.GuardianSnapshot, error) {
	return sdk.GuardianSnapshot{}, nil
}

func TestSearchWithStdlibContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("findme"), 0o644))

	matches, err := searchWithStdlib(ctx, dir, true, "findme", "", false, false, 0, true, nil)
	require.NoError(t, err)
	assert.Nil(t, matches)
}

func TestSearchDirContextCanceled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("findme"), 0o644))

	re := regexp.MustCompile("findme")

	// Cancel before starting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	matches, err := searchDir(ctx, dir, re, 0, "", true, nil)
	require.NoError(t, err)
	assert.Nil(t, matches)
}

func TestSearchFileContextCanceled(t *testing.T) {
	content := "line1\nline2\nline3\n"
	path := createTempFile(t, content)

	re := regexp.MustCompile("line")

	// Cancel before starting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	matches := searchFile(ctx, "test.txt", path, re, 0, nil)
	assert.Nil(t, matches)
}

func TestSearchFileContextCanceledMidScan(t *testing.T) {
	// Create a large file (just under 10MB limit) so scanning takes long enough for cancel to fire.
	lines := make([]string, 0, 150000)
	for i := range 150000 {
		lines = append(lines, fmt.Sprintf("line %d with findme target content to make lines longer", i))
	}

	content := strings.Join(lines, "\n")
	path := createTempFile(t, content)

	re := regexp.MustCompile("findme")

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan []string, 1)

	go func() {
		done <- searchFile(ctx, "test.txt", path, re, 0, nil)
	}()

	select {
	case <-done:
		// Function returns promptly on cancellation; partial result count is
		// timing-dependent so we only assert it doesn't hang.
	case <-time.After(5 * time.Second):
		t.Fatal("searchFile did not return after context cancellation")
	}
}

func TestSearchDirContextCanceledMidWalk(t *testing.T) {
	dir := t.TempDir()
	// Create many files so walk takes time
	for i := range 5000 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.txt", i)), []byte("findme content here"), 0o644))
	}

	re := regexp.MustCompile("findme")

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct {
		matches []string
		err     error
	}, 1)

	go func() {
		m, e := searchDir(ctx, dir, re, 0, "", true, nil)
		done <- struct {
			matches []string
			err     error
		}{m, e}
	}()

	select {
	case <-done:
		// Function returns promptly on cancellation; partial result count is
		// timing-dependent so we only assert it doesn't hang.
	case <-time.After(5 * time.Second):
		t.Fatal("searchDir did not return after context cancellation")
	}
}

func TestSearchWithRipgrepContextCanceledMidOperation(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not in PATH")
	}

	dir := t.TempDir()
	// Create many files so ripgrep takes time
	for i := range 10000 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%05d.txt", i)), []byte("findme content here"), 0o644))
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct {
		matches []string
		err     error
	}, 1)

	go func() {
		m, e := searchWithRipgrep(ctx, "rg", dir, true, "findme", "", false, false, 0, true, "", nil)
		done <- struct {
			matches []string
			err     error
		}{m, e}
	}()

	select {
	case result := <-done:
		// Key assertion: function returns promptly on cancellation with partial results.
		_ = result
	case <-time.After(5 * time.Second):
		t.Fatal("searchWithRipgrep did not return after context cancellation")
	}
}

type mockBus struct {
	mu     sync.Mutex
	events []sdk.Event
}

func (m *mockBus) Publish(e sdk.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.events = append(m.events, e)
}

func (m *mockBus) On(topic string, h sdk.Handler) {}
func (m *mockBus) OnAll(h sdk.Handler)            {}
func (m *mockBus) Off(h sdk.Handler)              {}
func (m *mockBus) Close() error                   { return nil }

func (m *mockBus) progressEvents() []sdk.Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []sdk.Event

	for _, e := range m.events {
		if e.Topic == sdk.TopicToolProgress {
			out = append(out, e)
		}
	}

	return out
}

func TestExecutePublishesProgressEvents(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("findme in a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("findme in b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("findme in c"), 0o644))

	bus := &mockBus{}
	ctx := sdk.WithBus(context.Background(), bus)

	result, err := (&tool{}).Execute(ctx, map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	progressEvents := bus.progressEvents()
	assert.NotEmpty(t, progressEvents, "should have published progress events")

	// Verify match counts are non-decreasing across events.
	var prevCount int

	for i, e := range progressEvents {
		payload, ok := e.Payload.(sdk.ToolProgress)
		require.True(t, ok)

		var count int

		_, _ = fmt.Sscanf(payload.Content, "Found %d matches", &count)
		assert.GreaterOrEqual(t, count, prevCount, "event %d should have non-decreasing count", i)
		prevCount = count
	}

	lastEvent := progressEvents[len(progressEvents)-1]
	payload, ok := lastEvent.Payload.(sdk.ToolProgress)
	require.True(t, ok)
	assert.Contains(t, payload.Content, "3")
	assert.Equal(t, "grep", payload.ToolName)
}

func TestExecuteThrottlesProgressEvents(t *testing.T) {
	dir := t.TempDir()
	for i := range 500 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%03d.txt", i)), []byte("findme content here"), 0o644))
	}

	bus := &mockBus{}
	ctx := sdk.WithBus(context.Background(), bus)

	_, err := (&tool{}).Execute(ctx, map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)

	progressEvents := bus.progressEvents()

	assert.GreaterOrEqual(t, len(progressEvents), 1, "should publish at least one progress event")
	// With 500 matches and 200ms throttle, events should be far fewer than match count.
	assert.Less(t, len(progressEvents), 50, "events should be throttled to far fewer than match count")
}

func TestExecuteProgressEventContent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.go"), []byte("findme in go"), 0o644))

	bus := &mockBus{}
	ctx := sdk.WithBus(context.Background(), bus)

	_, err := (&tool{}).Execute(ctx, map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)

	progressEvents := bus.progressEvents()
	require.NotEmpty(t, progressEvents)

	for _, e := range progressEvents {
		payload, ok := e.Payload.(sdk.ToolProgress)
		require.True(t, ok)
		assert.Equal(t, "grep", payload.ToolName)
		assert.Contains(t, payload.Content, "Found")
		assert.Contains(t, payload.Content, "matches")
		assert.Contains(t, payload.Content, "hello.go")
	}
}

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

func TestProgressCollector(t *testing.T) {
	bus := &mockBus{}
	// Use direct struct to avoid SDK throttle firing asynchronously.
	pc := &progressCollector{bus: bus}

	pc.add("file.go")
	pc.add("file.go")
	pc.add("other.go")

	pc.publish(false)

	events := bus.progressEvents()
	require.Len(t, events, 1)
	payload := events[0].Payload.(sdk.ToolProgress)
	assert.Equal(t, "grep", payload.ToolName)
	assert.Contains(t, payload.Content, "3")
	assert.Contains(t, payload.Content, "other.go")
}

func TestProgressCollectorCurrentFileEdgeCases(t *testing.T) {
	bus := &mockBus{}
	// Use direct struct to avoid SDK throttle firing asynchronously.
	pc := &progressCollector{bus: bus}

	// Normal case: file path is set correctly
	pc.add("file.go")
	pc.publish(false)

	events := bus.progressEvents()
	require.Len(t, events, 1)
	payload := events[0].Payload.(sdk.ToolProgress)
	assert.Contains(t, payload.Content, "file.go")

	// Reset
	bus.events = nil

	// Empty file path: currentFile should remain empty
	pc = &progressCollector{bus: bus}
	pc.add("")
	pc.publish(false)

	events = bus.progressEvents()
	require.Len(t, events, 1)
	payload = events[0].Payload.(sdk.ToolProgress)
	assert.NotContains(t, payload.Content, " in ")
}

func TestProgressCollectorPublishZeroMatches(t *testing.T) {
	bus := &mockBus{}
	ctx := sdk.WithBus(context.Background(), bus)
	pc := newProgressCollector(ctx, bus)

	pc.publish(false)

	assert.Empty(t, bus.progressEvents(), "should not publish event with zero matches")
}

func TestNewProgressCollectorNilBus(t *testing.T) {
	// Should not panic when bus is nil.
	pc := newProgressCollector(context.Background(), nil)
	require.NotNil(t, pc)

	// add and publish should be no-ops without panicking.
	pc.add("test.go")
	pc.publish(false)
}

func TestSearchWithStdlibOnMatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("findme in a\nother line"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("findme in b"), 0o644))

	var callbackMatches []string

	onMatch := func(_, m string) {
		callbackMatches = append(callbackMatches, m)
	}

	matches, err := searchWithStdlib(context.Background(), dir, true, "findme", "", false, false, 0, true, onMatch)
	require.NoError(t, err)
	require.NotNil(t, matches)
	assert.Len(t, matches, 2)
	assert.Equal(t, matches, callbackMatches)
}

func TestSearchFileOnMatch(t *testing.T) {
	content := "line one\nfindme here\nline three\n"
	path := createTempFile(t, content)
	re := regexp.MustCompile("findme")

	var callbackMatches []string

	onMatch := func(_, m string) {
		callbackMatches = append(callbackMatches, m)
	}

	matches := searchFile(context.Background(), "test.txt", path, re, 0, onMatch)
	require.Len(t, matches, 1)
	assert.Equal(t, matches, callbackMatches)
	assert.Contains(t, callbackMatches[0], "findme here")
}

func TestSearchFileOnMatchWithContextLines(t *testing.T) {
	content := "line1\nline2\nMATCH\nline4\nline5"
	path := createTempFile(t, content)
	re := regexp.MustCompile("MATCH")

	var callbackMatches []string

	onMatch := func(_, m string) {
		callbackMatches = append(callbackMatches, m)
	}

	matches := searchFile(context.Background(), "test.txt", path, re, 1, onMatch)
	require.Len(t, matches, 3)
	// onMatch is only called for actual matches, not context lines.
	require.Len(t, callbackMatches, 1)
	assert.Contains(t, callbackMatches[0], "MATCH")
}

func TestParseRgLine(t *testing.T) {
	baseDir := "/tmp"

	tests := []struct {
		name             string
		line             string
		include          string
		respectGitignore bool
		want             string
	}{
		{
			name: "valid match",
			line: `{"type":"match","data":{"path":{"text":"file.go"},"line_number":42,"lines":{"text":"hello world"}}}`,
			want: "file.go:42:hello world",
		},
		{
			name: "context type is included",
			line: `{"type":"context","data":{"path":{"text":"file.go"},"line_number":41,"lines":{"text":"prev line"}}}`,
			want: "file.go:41:prev line",
		},
		{
			name: "non-match type returns empty",
			line: `{"type":"begin","data":{"path":{"text":"file.go"}}}`,
			want: "",
		},
		{
			name: "malformed JSON returns empty",
			line: `not json at all`,
			want: "",
		},
		{
			name:    "include filter mismatch",
			line:    `{"type":"match","data":{"path":{"text":"file.go"},"line_number":1,"lines":{"text":"hello"}}}`,
			include: "*.txt",
			want:    "",
		},
		{
			name:             "skip VCS path",
			line:             `{"type":"match","data":{"path":{"text":".git/config"},"line_number":1,"lines":{"text":"secret"}}}`,
			respectGitignore: true,
			want:             "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, _ := parseRgLine(context.Background(), []byte(tt.line), baseDir, tt.include, tt.respectGitignore, "")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseRgLineSandboxDenied(t *testing.T) {
	dir := t.TempDir()

	sb := &testSandboxer{allowReadFn: func(p string) bool {
		return !strings.Contains(p, "secret")
	}}
	setSandboxer(sb)
	t.Cleanup(func() { setSandboxer(nil) })

	line := `{"type":"match","data":{"path":{"text":"secret.txt"},"line_number":1,"lines":{"text":"findme secret"}}}`
	_, got, _ := parseRgLine(context.Background(), []byte(line), dir, "", true, "")
	assert.Empty(t, got)
}

func TestSearchWithStdlibInvalidRegex(t *testing.T) {
	matches, err := searchWithStdlib(context.Background(), ".", true, "[invalid", "", false, false, 0, true, nil)
	require.Error(t, err)
	assert.Nil(t, matches)
}

func TestExecuteNilBus(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("findme here"), 0o644))

	// Execute with no bus in context should not panic and should return results.
	result, err := (&tool{}).Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    dir,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "findme here")
}
