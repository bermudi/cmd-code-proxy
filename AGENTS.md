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

### Response pipeline (assembler.go + translator.go)

The response side is a three-stage pipeline:

1. **EventTranslator** (`translator.go`) — scans upstream NDJSON, yields typed `Event` structs. Normalizes `finish-step`/`finish` deduplication and usage fields.
2. **ResponseAssembler** (`assembler.go`) — drives the translator loop, owns the `toolIndexRegistry`, dispatches each event to a `sink`.
3. **sink** — either `streamSink` (writes SSE chunks as they arrive) or `finalSink` (buffers everything, emits one JSON body at the end).

The dispatcher has zero `if streaming` branches. The only mode-specific policy is `promoteOrphans` (streaming promotes orphan `tool-input-delta` events to fresh slots; final drops them).

### Request pipeline (proxy.go + convert.go + adapter.go)

- `HandleChatCompletions` → `BuildCCRequestWithWorkingDir` → `DropSystemMessages` → `ConvertMessages` → `ConvertTools`
- `Upstream` interface (`adapter.go`) — real `ccAdapter` calls CommandCode with retries; tests inject `fakeUpstream`.

### The proxy has no project context
The proxy process runs in its own checkout directory
(`/home/daniel/build/command-code-proxy-server`). It does **not** run in
the user's project. The only project identifier it receives is the
`x_command_code_working_dir` header from the pi cc-cwd extension.

Without that header, the proxy would only know its own CWD — it has no
way to discover which project pi is working in. This means the proxy
**cannot** gather project-environment data (git branch, recent commits,
directory structure, etc.) on its own. That information must come from
the client (the pi extension) alongside the request.

Currently `populateConfigFromFS` shells out to `git` and calls
`os.ReadDir` using the `workingDir` header value as a **local-deployment
stopgap** — it works only because both processes are on the same machine
and the proxy can technically read `/home/daniel/Documents/NewsWiki` if
someone tells it to. The correct architecture is for the pi extension to
collect this data and send it; see ROADMAP.md § 2.8.

Three non-obvious decisions:

- **CommandCode uses Anthropic-style content parts** (`[{type:"text"},{type:"tool-call"}]`), not OpenAI's flat structure. `convert.go` translates.
- **Three parallel tool streaming protocols must all be handled**: legacy `tool-use` + `tool-delta`, modern `tool-input-start` + `tool-input-delta`, and inline `tool-call`. Adding a fourth is a real protocol change — see the Process section.
- **CommandCode's gateway builds the system prompt server-side.** The real `command-code` binary never sends system content in `params.messages[]` — only `user | assistant | tool`. The gateway reads the project's AGENTS.md, skills, etc. from disk using `config.workingDir`. The proxy must drop all system/developer messages before forwarding; the gateway reconstructs them. See MAINTAINING.md § CommandCode gateway protocol for the full protocol notes.

## Conventions

- **MiniMax-M3 distraction pattern (two factors identified):** 
  1. **(Fixed) Stub config values:** Sending `isGitRepo: false`, empty `structure`, `"Go proxy"` environment caused the gateway's server-side system prompt to look like a generic environment announcement. MiniMax-M3 reasonably interpreted the malformed input as system state and responded with "automated environment update" acks — not a hallucination, just the model correctly reading bad input. Fix: populate `config` with real project data (from the pi extension via `x_command_code_config`, with `populateConfigFromFS` as fallback).
  2. **(Fixed 2026-06-08) Missing `threadId`:** The proxy used to omit `threadId` entirely, while the real binary sends a UUID per session. If the gateway constructs the system prompt only on the first message of a session and caches it for subsequent turns, every proxy request was a new session with a fresh system prompt. The model would see the environment context as new each time and respond to it. Fix: generate `threadId` per request via `uuid.New()`. See MAINTAINING.md § CommandCode gateway protocol for the full theory.
- **The proxy impersonates the command-code binary, not itself.** Every field in `CCConfig` must match what the real `command-code` binary would send — this is not "the proxy's best guess about its own environment." The `Environment` field is a hardcoded string (`"linux-x64, Node.js v26.2.0"`) that matches the real CLI's output; it is **intentionally a lie** about the proxy's actual OS/runtime. When in doubt, capture traffic from the real binary (see MAINTAINING.md) and match that.
- **All upstream requests use `stream: true`.** Non-streaming client mode buffers NDJSON and assembles a final response. Do not "optimize" by setting stream=false on non-streaming requests.
- **System/developer messages are dropped before forwarding.** The gateway builds the system prompt server-side from `config.workingDir`. If every input message is system, the proxy 400s instead of forwarding an empty message list. See MAINTAINING.md § CommandCode gateway protocol for the full protocol notes.
- **`x-taste-learning` should reflect the user's actual `command-code` preference, not be hardcoded to `"true"`.** The real binary reads `userConfig.tasteLearning` (default `true`) and sends `"true"`/`"false"` accordingly. The pi extension (cc-cwd) hardcodes `x_command_code_taste_learning: false` per-request to match the user's local preference; a `-taste-learning` CLI flag sets the proxy-wide default. See MAINTAINING.md § Taste learning for the full wire-format analysis. If the user toggles their command-code taste preference, the extension value needs to be edited (no automatic coupling — by design, the extension doesn't read `~/.commandcode/config.json`).
- **`x-co-flag` is misnamed.** Despite the constant `INTERNAL_TEAM_FLAG_HEADER`, the value the binary sets is `isOAuthEnforced().toString()`. The proxy's hardcoded `"false"` is correct for an API-key auth deployment.
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

## Capturing real `command-code` traffic

When debugging request-fidelity questions, ground truth is captures from the real `command-code` binary. The procedure lives in [MAINTAINING.md](MAINTAINING.md) § Capturing real binary traffic. Short version:

```sh
# Start cmd-recorder (listens on :9090, captures to ./captures/)
/home/daniel/build/cmd-recorder/cmd-recorder ./captures :9090 &

# Point command-code at the recorder (BOTH env vars required; --local also works)
cd /path/to/project
COMMANDCODE_SANDBOX=true COMMANDCODE_API_URL=http://127.0.0.1:9090 \
  command-code --skip-onboarding -p "<prompt>"
```

The `COMMANDCODE_API_URL` env var is silently ignored unless `COMMANDCODE_SANDBOX=true` is also set — the minified `getApiBaseUrl()` in `dist/index.mjs` is the source of truth for this.

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
