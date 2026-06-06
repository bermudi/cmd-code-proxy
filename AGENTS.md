# AGENTS.md

## Project
Personal-use HTTP proxy that exposes an OpenAI-shaped API and translates to CommandCode's proprietary `/alpha/generate` endpoint. Lets any OpenAI-shaped client use CommandCode as a backend. Built for the maintainer's own use; the priority is making the response side look like the real `command-code` binary output.

## Scope — what this proxy is, and what it isn't
**Is:** an OpenAI-shape adapter for personal use. Response side is a faithful translation; request side supports the OpenAI features actually used in practice (tools, tool calls, images, system messages, thinking/reasoning content parts, plus the basics).

**Is not:** a complete OpenAI-API-shape-preserving proxy. The dropped-fields list lives in [MAINTAINING.md](MAINTAINING.md); the deliberate-feature-gaps are in [ROADMAP.md](ROADMAP.md) § Phase 3.

The asymmetry is deliberate: the response side is the hard contract (anything wrong there is a bug a real client will hit); the request side only needs to carry the features actually exercised.

## Stack
Go 1.26, stdlib + `google/uuid`. No web framework, no ORM, no DI container — `proxy.go` wires the routes by hand.

## Architecture
```
OpenAI client → localhost:55990/v1/chat/completions (OpenAI format)
                      ↕  convert.go
              api.commandcode.ai/alpha/generate (NDJSON, content-parts format)
```
Two non-obvious decisions:

- **CommandCode uses Anthropic-style content parts** (`[{type:"text"},{type:"tool-call"}]`), not OpenAI's flat structure. `convert.go` translates.
- **Three parallel tool streaming protocols must all be handled**: legacy `tool-use` + `tool-delta`, modern `tool-input-start` + `tool-input-delta`, and inline `tool-call`. Adding a fourth is a real protocol change — see the Process section.

## Conventions
- **All upstream requests use `stream: true`.** Non-streaming client mode buffers NDJSON and assembles a final response. Do not "optimize" by setting stream=false on non-streaming requests.
- **System messages hoist to a top-level `system` field.** They are not left in the messages array. CommandCode rejects them otherwise.
- **Tool results whose content starts with `Error:` get `type: "error-text"`.** Stable data contract; do not "fix" this.
- **The response side exposes `reasoning_content` on both `Message` and `Delta`.** De facto standard among reasoning-model gateways (DeepSeek, Qwen, Anthropic-protocol bridges) and matches the upstream `command-code` binary. Strict OpenAI SDKs ignore it; reasoning-aware clients consume it.

## Process — how to change the response side
The response side is the hard contract. The unit tests in `*_test.go` verify the per-event contract; the **parity test is the proof of behavior preservation on the wire format**. If only the unit tests pass, the change is suspect.

The rules:

1. **New event type → new parity fixture first.** Fixture first, then implementation, then run the parity test, then merge. No "we'll add the test later."
2. **When the parity test reports a diff, classify it before merging.** Unintentional regression → fix the code. Intentional improvement → update the fixture's expected value with a comment explaining why. Unclear → stop and ask.
3. **Never edit the vendored baseline to make the test pass.** That code is the ground truth. If the test can't be made to pass, the new code is wrong.
4. **Periodically verify the test catches regressions.** Deliberately break the assembler, confirm the test fails with a useful message, then revert. A safety net that doesn't fire is worse than no safety net.

Worked example, helper-map names, and the full classification workflow live in [MAINTAINING.md](MAINTAINING.md) § Parity test mechanics. Update there when the harness changes.

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
The parity test runs as part of `go test ./...`. It enforces byte-equivalence (streaming) or class-equivalence (non-streaming) against the vendored pre-refactor code.

## Goals
What this proxy should always do well:

1. **Response-side fidelity to the verified pre-refactor baseline.** Streaming SSE and non-streaming JSON must be byte-equivalent (or class-equivalent for known fixes) for every event combination the parity test exercises.
2. **Faithful streaming tool-call protocol translation.** Three upstream tool-call shapes → one OpenAI protocol. No data loss, no splitting one logical call into multiple client-visible calls.
3. **Faithful finish-reason semantics.** `length` / `content_filter` / `tool_calls` / `stop` from upstream are preserved. The pre-refactor non-streaming path silently downgraded length and content_filter to `stop`; that was a bug, do not reintroduce it.
4. **Reasoning-content passthrough.** `reasoning_content` on both `Message` and `Delta`.
5. **Tools, images, and thinking on the request side.** Round-trip cleanly.
6. **No regression in `/v1/models`, `/health`, `/v1/responses` for the supported subset.**

## Constraints
- **Default binds to localhost only.** Do not expose to a network without auth in front.
- **API key comes from `-api-key` flag or `Authorization: Bearer` header per-request.** Never log it; never include it in error responses.
- **Parity test baseline is immutable.** Never edit code under `paritytest/` that is part of the vendored baseline. See [MAINTAINING.md](MAINTAINING.md) for the exception workflow.

## Pointers
- Time-bound plan: [ROADMAP.md](ROADMAP.md)
- Mechanism-rich reference: [MAINTAINING.md](MAINTAINING.md) (parity test mechanics, request-fidelity table, release checklist)
- User-facing doc: [README.md](README.md)
