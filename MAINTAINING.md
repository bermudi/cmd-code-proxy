# MAINTAINING.md

Mechanism-rich reference for working on this proxy. **Read this when you're about to change the response side, debug a request-fidelity question, or cut a release.** For the project's shape, intent, and goals, see [AGENTS.md](AGENTS.md). For the time-bound plan, see [ROADMAP.md](ROADMAP.md).

## Parity test mechanics

The response side is the hard contract of this proxy. The parity test in `internal/proxy/paritytest/` is the safety net. It feeds the same NDJSON through a vendored copy of the pre-refactor dispatcher and the current code, then diffs the bytes. Streaming is asserted byte-identical; non-streaming is asserted class-equivalent via per-fixture `expected` maps.

### The harness in one paragraph

`paritytest/parity_test.go` contains:

- A verbatim copy of the pre-refactor `StreamResponse` / `NonStreamResponse` / `WriteSSE` / `EventTranslator` (renamed to `old*` to avoid name collisions when this package imports the current `proxy` package).
- A `parityFixtures` table — 17 NDJSON streams exercising every event class and combination.
- `TestParity_Stream` and `TestParity_Final` — for each fixture, run old and new code, normalize JSON, and assert.
- A `constraint` predicate per fixture: `valueIs(v)`, `valueAbsent()`, `valuePresent()`. Suffix-matched against the JSON dot-paths that differ.
- A `diffJSON` walker that reports the set of paths that differ between old and new output.

### Adding a new event class

Walked example for a hypothetical `EventRefusalDelta` (upstream signals a content refusal mid-stream):

1. Add `EventRefusalDelta EventType` to `translator.go` and a decoder case for `raw.Refusal`.
2. Add the case to `assembler.go`'s `handle` switch, dispatching to a new `onRefusalDelta(text)` strategy method.
3. Add a parity fixture in `paritytest/` named `refusal_delta` with the new NDJSON shape. The `expected` map names where in the response the refusal should appear (e.g. `choices.0.delta.refusal` or wherever the OpenAI spec lands it).
4. Run `go test ./...`. The new fixture must pass; existing fixtures must be unchanged.
5. If an existing fixture's diff is unintentional, fix the assembler. If a new fixture fails because the expected value is wrong, fix the fixture's value to match the real OpenAI spec — not the assembler's current output.

### When the parity test reports a diff

Three cases:

- **Unintentional regression** — the new code changed a wire-format detail that wasn't supposed to change. Fix the new code, do not touch the fixture. If the fixture is wrong, fix the fixture; if both look right, the old code's behavior was a bug and the new code is a fix, and the fixture is *also* wrong — update both with a comment.
- **Intentional improvement** — the new code fixes a real wire-format bug (e.g. propagating `finish_reason: length` instead of silently downgrading to `stop`), or omits a degenerate field (e.g. the always-zero `usage` object when no usage data arrived). Add the changed path to the fixture's `expected` map with a `valueIs(...)` / `valueAbsent()` / `valuePresent()` constraint that names the new value, and a comment explaining why.
- **Unclear** — stop and ask. Do not paper over an unexpected diff by adjusting the fixture to match whatever the new code happened to produce.

### Testing the test

The parity harness was built once. Its continued ability to catch the failure modes it was designed for is unverified. **Periodically verify it still fires.** To do this:

1. Pick a known-wrong behavior (e.g. `finish_reason = "WRONG"`, or reverse the args concatenation).
2. Apply the change locally.
3. Run `go test ./...`. Confirm the test fails with a useful diff.
4. Revert the bait.
5. Record the verification date in a comment in `paritytest/parity_test.go` so future maintainers know the safety net was last verified.

If the bait doesn't fail the test, the test is broken. Stop and fix it.

## Request-side fidelity

What the proxy accepts on the request side. **Until this lands as a comment in `main.go` (see ROADMAP Phase 2.3), this file is the source of truth.**

| Field | Status |
| --- | --- |
| `model` | ✓ mapped via alias table, forwarded |
| `messages` | ✓ converted via `ConvertMessages` (system hoisted, tool_calls/tool_result content parts, image URLs, thinking/reasoning) |
| `tools` | ✓ converted via `ConvertTools` (function schema → CommandCode input_schema) |
| `temperature` | ✓ forwarded |
| `max_tokens` / `max_completion_tokens` | ✓ forwarded |
| `stream` | ✓ forwarded (always `true` upstream) |
| `tool_choice` | ✗ parsed, dropped |
| `parallel_tool_calls` | ✗ parsed, dropped |
| `response_format` | ✗ parsed, dropped |
| `stop` | ✗ parsed, dropped |
| `top_p` | ✗ parsed, dropped |
| `presence_penalty` / `frequency_penalty` | ✗ parsed, dropped |
| `logprobs` / `logit_bias` / `metadata` / `audio` / `modalities` | ✗ parsed, dropped |

The dropped fields aren't used by the personal-use scenarios this proxy was built for. The roadmap says: don't add them speculatively; add one when a real request needs it (Phase 2.4), with a parity fixture that proves it round-trips, and update the table.

## Release checklist

Before tagging a release:

- [ ] Bump `appVersion` in `main.go`.
- [ ] Update the `Version:` line in `README.md`.
- [ ] Build the binaries from a clean tree: `bin/command-code-proxy`, `bin/command-code-proxy-arm64`, `bin/command-code-proxy.exe`. (Or intentionally drop binary changes from the commit — current drift is easy to miss.)
- [ ] `go test ./...` — all green including the parity test.
- [ ] `go vet ./...` — clean.
- [ ] Smoke `/health` and `/v1/models` against the built binary.
- [ ] Tag the commit. Push the tag.

The `-working-dir` flag (ROADMAP Phase 1.4) is **not** a release blocker. Cut the release from a tag without it; ship the flag in a follow-up commit.

## When a release is *not* happening

For everyday commits, the bar is lower:

- [ ] `go test ./...` — all green.
- [ ] `go vet ./...` — clean.
- [ ] If you changed the response side, the parity test caught the diff and the diff is classified (see Parity test mechanics above).
- [ ] AGENTS.md and ROADMAP.md are updated if scope, goals, or the plan changed.

That's it. No tag, no version bump, no rebuild of all platforms.
