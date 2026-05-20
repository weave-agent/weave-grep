# Tool Streaming & Interruption — Grep Extension

## Overview
Add streaming progress to the grep tool so the TUI shows partial results while searching. Also fix the stdlib fallback to respect context cancellation so ESC can interrupt it.

## Context
- `grep.go` — `searchWithRipgrep` already takes `ctx` and uses `exec.CommandContext`
- `grep.go` — `searchWithStdlib` does NOT take `ctx`; no cancellation checks
- `grep.go` — no bus events published during execution

## Development Approach
- Regular approach
- Every task includes tests before moving to next

## Implementation Steps

### Task 1: Add context to stdlib search path
- [x] Change `searchWithStdlib` signature to accept `ctx context.Context`
- [x] Add `ctx.Err()` check in the file walk loop (between files)
- [x] Add `ctx.Err()` check in the line scan loop (periodic, every N lines)
- [x] Return early with empty results if context is canceled
- [x] Write tests: stdlib search cancels mid-execution
- [x] Run extension tests — must pass

### Task 2: Add streaming progress via bus events
- [ ] Collect matches in a slice with mutex protection
- [ ] Use `sdk.Throttle` (from core SDK) to publish `tool.progress` events
- [ ] Event content: "Found N matches in <path>..." or truncated preview
- [ ] Throttle interval: 200ms
- [ ] Write tests: verify bus events are published, throttled, and contain correct match counts
- [ ] Run extension tests — must pass

### Task 3: Verify integration
- [ ] Run `go test ./...` in grep extension dir
- [ ] Run `make lint` if available

## Technical Details

```go
func (t *tool) search(ctx context.Context, absPath string, isDir bool, ...) []string {
    if rgPath := ripgrep.Find(); rgPath != "" {
        return searchWithRipgrep(ctx, rgPath, absPath, ...)
    }
    return searchWithStdlib(ctx, absPath, ...)
}
```

## Post-Completion
- Depends on core SDK `Throttle` helper
- Manual verification: run `grep` on a large directory and watch TUI show "Found N matches..." updating live
