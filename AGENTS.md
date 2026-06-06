# AGENTS.md

## Project
Local HTTP proxy that exposes an OpenAI-compatible API and translates requests to CommandCode's proprietary `/alpha/generate` endpoint. Lets any OpenAI-shaped client use CommandCode as a backend.

## Scope — what this proxy is, and what it isn't

**Is:** a personal-use adapter that turns OpenAI-shaped HTTP traffic into CommandCode-shaped traffic, and turns CommandCode's NDJSON responses back into OpenAI-shaped SSE / JSON. The response side is a faithful translation — the output is byte-identical to the verified pre-refactor behavior of this proxy for every event class we test. The request side supports the OpenAI features actually used in practice: tools, tool calls, images, system messages, thinking/reasoning content parts, and the basics (`model`, `messages`, `temperature`, `max_tokens`/`max_completion_tokens`, `stream`).

**Is not:** a complete OpenAI-API-shape-preserving proxy. Some OpenAI request fields are accepted by the parser and silently dropped before reaching CommandCode (see [Request-side fidelity](#request-side-fidelity) below). Streaming errors from upstream are logged but not surfaced to the client. The `/v1/responses` endpoint is a partial shim, not a full implementation. The `/v1/models` list is hand-curated, not dynamically fetched from upstream.

The asymmetry is deliberate. For personal use, the response side has to look like the real `command-code` binary output — that's what the parity test enforces. The request side only has to carry the features the user actually exercises.

## Stack
Go 1.26 · stdlib + `google/uuid` · no framework

## Architecture
```
OpenAI client → localhost:55990/v1/chat/completions (OpenAI format)
                      ↕  convert.go
              api.commandcode.ai/alpha/generate (NDJSON, content-parts format)
```

- **internal/proxy/convert.go** — OpenAI ↔ CommandCode message/tool format translation
- **internal/proxy/assembler.go** — single per-event dispatcher for the response side (one switch, one tool-index registry, one finish-reason policy). Owns both streaming-SSE and non-streaming-JSON output.
- **internal/proxy/proxy.go** — request handling, HTTP routes, wire format glue
- **internal/proxy/model.go** — short alias → full model ID mapping
- **internal/api/** — type definitions for both wire formats
- **internal/version/** — polls npm registry for latest command-code version (spoofs `x-command-code-version`)
- **internal/proxy/paritytest/** — vendored copy of the pre-refactor dispatcher + fixture-driven harness. Feeds the same NDJSON through old and new code and asserts byte-level equivalence for streaming, plus classified equivalence for non-streaming. This is the safety net that keeps the response side honest.

CommandCode uses Anthropic-style content-parts (`[{type:"text",...},{type:"tool-call",...}]`), not OpenAI's flat structure. Three parallel tool streaming protocols exist (`tool-use`/`tool-delta`, `tool-input-start`/`tool-input-delta`, `tool-call`) — all must be handled.

## Conventions
- All upstream requests use `stream: true` — non-streaming mode buffers NDJSON and assembles a final response
- System messages are hoisted to a top-level `system` field, not left in the messages array
- Tool results with content starting with `"Error:"` get `type: "error-text"`
- The response side exposes `reasoning_content` on both `Message` and `Delta`. This is *de facto* standard among reasoning-model gateways (DeepSeek, Qwen, Anthropic-protocol bridges) and is what the upstream `command-code` binary does. Strict OpenAI SDKs ignore the field; reasoning-aware clients consume it directly.
- Finish reason policy is unified: whatever upstream said (normalized to `stop` / `length` / `tool_calls` / `content_filter`) is what the client gets. The pre-refactor non-streaming path hard-coded `stop` or `tool_calls` and dropped upstream's real reason — that was a bug, the new code fixes it.

## Process — how to change the response side

The response side is the hard contract of this proxy (see [Goals](#goals) above). Changes to `internal/proxy/assembler.go` and `internal/proxy/translator.go` are how CommandCode's protocol becomes OpenAI's, and they will silently regress if treated casually. The parity test in `internal/proxy/paritytest/` is the safety net. These rules exist because "all unit tests pass" was once used to declare this proxy done, and it missed a real wire-format bug (`finish_reason: length` was being silently downgraded to `stop`).

### The rules

1. **The parity test is the proof of behavior preservation on the response side, not the unit tests.** Per-event unit tests in `assembler_test.go` verify the contract; the parity test in `paritytest/` verifies the wire format against a vendored copy of the pre-refactor dispatcher. If only the unit tests pass, the change is suspect.
2. **When CommandCode adds a new event type, add a new parity fixture *first*.** Add an `EventType` constant, decoder case, and `handle` case in the assembler; add a fixture that exercises it; run the parity test; then merge. The fixture name documents what changed. No "we'll add the test later."
3. **When the parity test reports a diff, classify it before merging.** Either:
   - The diff is an unintentional regression — fix the code, do not modify the fixture.
   - The diff is an intentional improvement (e.g. fixing a wire-format bug, omitting a degenerate field) — add the changed path to the fixture's `expected` map with a `valueIs(...)` / `valueAbsent()` / `valuePresent()` constraint that names the new value, and a comment explaining why.
   - The diff is unclear — stop and ask, do not paper over it.
4. **Never edit the vendored old code in `paritytest/` to "make the test pass."** That code is the baseline. If the test can't be made to pass against the baseline, the new code is wrong, not the baseline.
5. **Periodically verify the test catches regressions.** Deliberately introduce a known wrong behavior in the assembler (e.g. emit `finish_reason: "WRONG"`, or reverse the args concatenation), run the parity test, confirm it fails with a useful message, then revert. A safety net that doesn't fire is worse than no safety net because it gives false confidence. Do this whenever the test infrastructure changes.

### A worked example

Adding a hypothetical `EventRefusalDelta` event (upstream signals a content refusal mid-stream):

1. Add `EventRefusalDelta EventType` to `translator.go`, decode it from `raw.Refusal`.
2. Add the case to `assembler.go`'s `handle` switch, dispatching to a new `onRefusalDelta(text)` strategy method.
3. Add a parity fixture in `paritytest/` named `refusal_delta` with the new NDJSON shape and an `expected` map that says the response should carry the refusal in `choices[0].delta.refusal` (or wherever the spec lands it).
4. Run `go test ./...`. The new fixture must pass; existing fixtures must be unchanged.
5. If an existing fixture's diff is unintentional, fix the assembler. If a new fixture fails because the expected value is wrong, fix the fixture's value to match the real OpenAI spec — not the assembler's current output.

### Anti-patterns

- Adding code to the assembler, running only `assembler_test.go`, and declaring the change done.
- Marking a parity test failure as "known issue" and merging anyway.
- Extending `fallbackModels` (or any hand-maintained list) without a corresponding fixture showing the new model round-trips.
- "I read the code and it looks right" as a substitute for measurement.

## Workflow
```sh
go build -o bin/command-code-proxy .
go test ./...

# start
./bin/command-code-proxy -api-key "$(cat ~/.pi/agent/CMD_API_KEY)"
# flags: -port (default 55990), -host (default 127.0.0.1), -api-key, -list-closed-models, -version

# verify
curl http://127.0.0.1:55990/health
curl http://127.0.0.1:55990/v1/models
```

The parity test runs as part of `go test ./...`. It enforces that any change to the response-side dispatcher is byte-equivalent (streaming) or classified-equivalent (non-streaming) to the vendored pre-refactor code.

## Request-side fidelity

The following OpenAI request fields are parsed and forwarded:

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

The dropped fields aren't used by the personal-use scenarios this proxy was built for. If you need one, see [Goals](#goals) and [Nice-to-haves](#nice-to-haves) below.

## Goals

What this proxy should always do well:

1. **Response-side fidelity to upstream `command-code` binary output.** The response side (streaming SSE and non-streaming JSON) is byte-equivalent to a verified pre-refactor baseline for every event combination the parity test exercises. Any new event class added by CommandCode must be covered by a new parity fixture.
2. **Faithful streaming tool-call protocol translation.** Three upstream tool-call shapes (legacy `tool-use`/`tool-delta`, modern `tool-input-start`/`tool-input-delta`, and inline `tool-call`) all translate to the OpenAI single-shape protocol without dropping data or splitting a single logical call into multiple client-visible calls.
3. **Faithful finish-reason semantics.** `length` / `content_filter` / `tool_calls` / `stop` from upstream are preserved through the proxy to the client. No more "everything becomes `stop`."
4. **Reasoning-content passthrough.** `reasoning_content` is exposed on both `Message` and `Delta` so reasoning-aware clients can see what the model thought.
5. **Tools, images, and thinking on the request side.** These are the features actually used; they must round-trip cleanly.
6. **No regression in `/v1/models`, `/health`, `/v1/responses` for the supported subset.**

## Nice-to-haves

Improvements that are *not* part of the current goals but would be reasonable next steps. Roughly ordered by payoff-per-effort, with the personal-use bias in mind — speculative features are pushed down, reliability and debuggability are pulled up.

### Near term (next 1–2 sessions)

1. **Real upstream parity fixtures.** Capture 2–3 real NDJSON streams from the live `command-code` binary (one short text, one with tool calls, one with reasoning) and add them as fixtures in `paritytest/`. Half a session. The current 17 fixtures are hand-written and prove the new code matches the old code; they do not prove the proxy matches real upstream behavior.
2. **Request ID end-to-end.** Generate or honor a `X-Request-Id` on inbound, thread it through `BuildCCRequest` → upstream → assembler → log lines. 2–3 hours. Prerequisite for every future debugging session. The OpenAI `client_request_id` field can be reused.
3. **Surface upstream streaming errors to the client.** The assembler currently logs `EventError` and continues; clients should get an OpenAI-shaped error chunk + `[DONE]` + a recognizable `finish_reason` instead of a clean finish. ~1 hour. Prevents the "client hangs in tool loop" failure mode.
4. **`-working-dir` flag.** One CLI flag, one if-statement in `BuildCCRequest`. 15 minutes. Has been a TODO; the morning handoff notes call it out.

### Medium term (next month or two)

5. **Capture a regression-bait protocol change and verify the test catches it.** Intentionally add a fake new event to a fixture, verify the parity test fails with a useful diff, then revert. Half a session. Tests the test, not the proxy.
6. **Structured logging (slog).** Slog is in stdlib. ~1 day. Prerequisite for any future observability work.
7. **Make the personal-use boundary visible.** A comment in `main.go` listing the dropped OpenAI fields and saying "this proxy is for personal use by the maintainer; the following are intentionally not implemented." Turns a deferred TODO into a deliberate design choice. 30 minutes.
8. **Forward more OpenAI request fields** (`tool_choice`, `parallel_tool_calls`, `response_format`, `stop`, `top_p`) — wait for a real use case. The work is small when the need is concrete; premature now.

### Long term / don't bother

- **Dynamic `/v1/models` from upstream.** The hand-curated list works for personal use.
- **Full OpenAI Responses coverage.** The shim is partial by design; the chat completions endpoint is the primary surface.
- **Public-package API boundary.** Don't bother until someone tries to import this as a library.
- **Request-body fidelity parity test.** Useful, but only worth doing if/when a real protocol change forces the request side to evolve.

## Morning handoff notes
- Before release, rebuild all tracked binaries together (`bin/command-code-proxy`, `bin/command-code-proxy-arm64`, `bin/command-code-proxy.exe`) or intentionally drop binary changes from the commit. Current drift is easy to miss.
- If cutting a release, bump `appVersion` in `main.go`, update README's version string, tag the commit, and make sure the binaries are built from the clean tagged tree.
- Decide whether process cwd is acceptable for `config.workingDir`/`x-project-slug`. If users run this as a service, add a `-working-dir` flag or another explicit override instead of silently using the service directory.
- After those fixes, run `go test ./...`, `go vet ./...`, and smoke `/health` plus `/v1/models` against the built binary.

## Constraints
- Default binds to localhost only — do not expose to network without auth
- API key can come from `-api-key` flag or `Authorization: Bearer` header per-request
- Model list at `/v1/models` is hardcoded, not fetched from upstream — needs manual updates when CommandCode adds models
