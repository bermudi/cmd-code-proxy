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
| `messages` | ✓ converted via `ConvertMessages` (system hoisted, tool_calls/tooll_result content parts, image URLs, thinking/reasoning) |
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

Improvements that are *not* part of the current goals but would be reasonable next steps. Listed in rough order of effort/payoff.

1. **Forward more OpenAI request fields.** `tool_choice`, `parallel_tool_calls`, `response_format`, `stop`, `top_p` — implement CommandCode-side equivalents for each and add parity coverage.
2. **Surface upstream streaming errors to the client.** The assembler currently logs `EventError` and continues; clients should get an OpenAI-shaped error chunk + `[DONE]` instead of a clean finish.
3. **Dynamic `/v1/models` from upstream.** Replace the hand-curated `fallbackModels` with a fetch-on-startup or fetch-on-first-request approach.
4. **Real upstream parity.** Capture an actual CommandCode NDJSON stream (or several) and add it as a fixture file. The current parity fixtures are hand-written; real upstream data is a stronger signal.
5. **`-working-dir` flag.** The proxy uses process cwd for `config.workingDir` / `x-project-slug`. A service-mode user has no good way to override this.
6. **Fuller `/v1/responses` endpoint.** `truncation`, `metadata`, `previous_response_id`, `store`, `user` are dropped at the shim layer.
7. **Request-body fidelity parity test.** There's no parity coverage for the request side. The current request-side tests are per-feature unit tests, not byte-equivalence against an old baseline.
8. **A documented public-package layout.** Today the proxy package internals are exposed to `paritytest` for convenience. If the proxy is ever imported by another module, the API surface will need a proper boundary.

## Morning handoff notes
- Before release, rebuild all tracked binaries together (`bin/command-code-proxy`, `bin/command-code-proxy-arm64`, `bin/command-code-proxy.exe`) or intentionally drop binary changes from the commit. Current drift is easy to miss.
- If cutting a release, bump `appVersion` in `main.go`, update README's version string, tag the commit, and make sure the binaries are built from the clean tagged tree.
- Decide whether process cwd is acceptable for `config.workingDir`/`x-project-slug`. If users run this as a service, add a `-working-dir` flag or another explicit override instead of silently using the service directory.
- After those fixes, run `go test ./...`, `go vet ./...`, and smoke `/health` plus `/v1/models` against the built binary.

## Constraints
- Default binds to localhost only — do not expose to network without auth
- API key can come from `-api-key` flag or `Authorization: Bearer` header per-request
- Model list at `/v1/models` is hardcoded, not fetched from upstream — needs manual updates when CommandCode adds models
