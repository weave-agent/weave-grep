# CLAUDE.md — weave-grep

## Guardian Policy Pattern

The grep tool registers a guardian from `sdk.GuardianRegisteredTopic` and checks it before sandbox reads. `guardianRequest` uses `sdk.GuardianActionRead`, `ToolName: "grep"`, and metadata `operation: "grep"`.

Execution order is: validate args, check guardian, resolve the absolute path, check sandbox, then search. Guardian blocks and guardian errors return tool errors before sandbox checks run. Sandbox reads use `AllowReadWithMetadata` when available and include `guardian_request_id` so sandbox decisions can be correlated with guardian decisions.

## Streaming Progress Pattern

The grep tool emits `tool.progress` bus events while searching. This is implemented via:

1. **`progressCollector`** — accumulates matches with mutex protection, tracks the most recently seen file
2. **`sdk.Throttle`** — throttles publish calls to 200ms intervals to avoid flooding the bus
3. **`onMatch` callback** — threaded from `Execute` down to leaf search functions so progress events fire as matches arrive, not just at the end

The collector is nil-safe: `newProgressCollector` accepts a potentially-nil bus and gracefully no-ops when nil.

## Context Cancellation Conventions

All search paths respect `context.Context` cancellation:

- **Pre-flight check**: `searchWithStdlib`, `searchFile`, and `searchDir` return immediately if `ctx.Err() != nil` before starting
- **Per-line check in `searchFile`**: `ctx.Err()` is checked every line during scan; on cancellation, partial lines are processed and returned
- **Per-file check in `searchDir`**: `ctx.Err()` is checked between files during `filepath.WalkDir`; on cancellation, partial matches are returned
- **Ripgrep streaming**: `searchWithRipgrep` checks `ctx.Err()` every scanner line and kills the subprocess via `cmd.Process.Kill()` on cancellation, returning partial matches

All cancellation paths return **partial results** rather than discarding them.

## Ripgrep Streaming Parse Pattern

Ripgrep output is consumed line-by-line via `cmd.StdoutPipe()` + `bufio.Scanner` rather than `cmd.Output()`. This enables:

1. Calling the `onMatch` callback as each match line is parsed
2. Responsive cancellation without waiting for the full rg process to complete
3. Memory efficiency for large search results

`parseRgLine` handles JSON unmarshaling, sandbox filtering, include pattern matching, and VCS path skipping per line.

## Testing Async Cancellation

Mid-operation cancellation is tested by:

1. Creating large files or many files to ensure the operation takes longer than the cancel delay
2. Firing cancellation from a goroutine after a short sleep (100ms)
3. Asserting the function returns within a timeout (5s) rather than asserting exact results

This approach avoids hanging tests while acknowledging the nondeterministic timing of goroutine scheduling.
