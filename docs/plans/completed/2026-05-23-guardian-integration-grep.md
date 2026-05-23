# Guardian Integration for grep Tool

Status: Completed
Completed: 2026-05-23

## Overview
Add guardian policy enforcement to the `grep` tool so that file content search is subject to the guardian's allow/ask/block decisions. The `grep` tool searches file contents — this is a `GuardianActionRead` action.

## Context
- **Tool file**: `grep.go`
- **Test file**: `grep_test.go`
- **Reference pattern**: `weave-bash` extension (`bash.go` lines 68-317)
- **Guardian action**: `sdk.GuardianActionRead`
- The grep tool already has sandbox integration (`sandboxer.AllowRead(absPath)` at line 162); guardian must run *before* sandbox.
- Note: `ask` profile auto-allows reads, but custom profiles may block or log them.

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- Run tests after each change

## Testing Strategy
- **Unit tests**: mock guardian with allow/block/ask/error decisions
- Verify guardian check runs before sandbox check
- Verify guardian block skips sandbox and returns error result
- Verify guardian allow proceeds to sandbox and grep logic

## Implementation Steps

### Task 1: Add guardian infrastructure to grep.go
- [x] Add `guardianMu sync.RWMutex`, `guardian sdk.Guardian` package-level variables
- [x] Add `setGuardian()` / `getGuardian()` helpers
- [x] Add `GuardianRegisteredTopic` listener in `init()` alongside existing `sandbox.registered` listener
- [x] Add `newRequestID()` helper
- [x] Add `guardianRequest(path string) sdk.GuardianRequest` helper with `Action: sdk.GuardianActionRead`
- [x] Add `checkGuardian()` helper (same pattern as bash)
- [x] Add `formatGuardianBlock()` helper (same as bash)
- [x] Call `checkGuardian()` at start of `Execute()`, before file search and sandbox checks
- [x] Pass `guardianReq.ID` into sandbox metadata for linkage
- [x] Run grep tests — must pass before next task

### Task 2: Add guardian tests to grep_test.go
- [x] Write `TestExecuteWithGuardian` with subtests:
  - "allow decision permits grep"
  - "block decision returns guardian error"
  - "missing guardian permits grep"
  - "guardian error returns tool error"
- [x] Write `TestExecuteGuardianSandboxOrdering`:
  - "guardian allow runs before sandbox"
  - "guardian block skips sandbox"
- [x] Add `testGuardian` mock helper
- [x] Run grep tests — must pass

### Task 3: Verify and cleanup
- [x] Run `make lint` in grep extension directory (no Makefile target; ran `golangci-lint run ./...` and `go vet ./...`)
- [x] Run full test suite for grep extension
- [x] Verify no regressions in existing grep functionality

## Technical Details

### guardianRequest for grep
```go
func guardianRequest(path string) sdk.GuardianRequest {
    return sdk.GuardianRequest{
        ID:          newRequestID("grep-guardian"),
        ToolName:    "grep",
        Action:      sdk.GuardianActionRead,
        Path:        path,
        Description: "Search file contents",
        Metadata: map[string]any{
            "operation": "grep",
        },
    }
}
```

### Execute ordering
1. Validate `path` and `pattern` parameters
2. **Guardian check** (`checkGuardian`) — if blocked, return error
3. Resolve absolute path
4. Sandbox check (`sandboxer.AllowRead`)
5. Search file contents, collect matches

## Post-Completion
- Manual verification: test grep tool with `ask` profile — should auto-allow reads
- Test with custom profile that blocks reads — should block
