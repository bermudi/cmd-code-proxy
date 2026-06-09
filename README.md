# CommandCode Proxy Server

OpenAI-compatible proxy server for the CommandCode API. Exposes `/v1/chat/completions`, `/chat/completions`, `/v1/responses`, and `/v1/models` so OpenAI-shaped clients can call CommandCode models through a local HTTP server.

Repository: https://github.com/bermudi/cmd-code-proxy

Version: `v1.0.8`

For project conventions, scope, goals, and the request-side fidelity table, see [AGENTS.md](AGENTS.md) (the table itself lives in [MAINTAINING.md](MAINTAINING.md) for now). For the time-bound development plan, see [ROADMAP.md](ROADMAP.md).

## Features

- OpenAI-compatible chat completions endpoint and alias
- OpenAI Responses-compatible endpoint shim
- Streaming and non-streaming responses
- OpenAI-compatible model list endpoint
- Optional closed/premium model listing
- Short model name mapping
- Optional default API key from CLI
- Per-request API key via `Authorization` header
- Configurable host and port
- Tools, tool calls, images, and thinking/reasoning content round-trip cleanly
- `reasoning_content` exposed on both message and delta (DeepSeek/Qwen/Anthropic-protocol style)

## What it is and isn't

This is a personal-use adapter, not a complete OpenAI-API shape-preserving proxy.

**Supported on the request side:** `model`, `messages` (including system hoisting, content parts, images, tool calls, tool results, thinking blocks), `tools`, `temperature`, `max_tokens`/`max_completion_tokens`, `stream`. Everything a Claude-Code-style client actually exercises.

**Not supported on the request side:** `tool_choice`, `parallel_tool_calls`, `response_format`, `stop`, `top_p`, `presence_penalty`, `frequency_penalty`. These are accepted by the JSON parser and dropped before reaching CommandCode. If you need one, file an issue.

**Response side:** byte-equivalent to this proxy's verified pre-refactor behavior for every event class covered by the parity test (17 fixtures, all event types and combinations). Streaming errors from upstream are logged but not surfaced to the client.

## Requirements

- Go 1.26.2 or newer

## Run

```bash
go run main.go
```

Default server address:

```text
http://127.0.0.1:55990
```

## CLI options

```bash
go run main.go [options]
```

| Option | Default | Description |
| --- | --- | --- |
| `-host` | `127.0.0.1` | Host to bind the server to |
| `-port` | `55990` | Port to run the server on |
| `-api-key` | empty | Optional default CommandCode API key |
| `-list-closed-models` | `false` | Include closed/premium models, such as Claude and GPT, in `/v1/models` |
| `-working-dir` | proxy process working directory | Working directory/project context to send to CommandCode |
| `-taste-learning` | `true` | Default value for the upstream `x-taste-learning` header. Override per-request with `x_command_code_taste_learning` in the request body. |
| `-capture-dir` | empty | Directory to save raw upstream request and response NDJSON for debugging |
| `-debug` | `false` | Enable debug-level logging (verbose per-event NDJSON) |
| `-version` | `false` | Print version and exit |

Examples:

```bash
# Run on default host and port
go run main.go

# Run on a custom port
go run main.go -port 8080

# Expose on all interfaces
go run main.go -host 0.0.0.0

# Use a default API key for all requests that do not include Authorization
go run main.go -api-key your-commandcode-api-key

# Include closed/premium models in /v1/models
go run main.go -list-closed-models

# Capture upstream request/response NDJSON for debugging
go run main.go -capture-dir ./captures

# Print version
go run main.go -version
```

## Build

Build for the current platform:

```bash
go build -o bin/command-code-proxy
```

Cross-compile for Windows and Linux:

```bash
GOOS=linux GOARCH=amd64 go build -o bin/command-code-proxy
GOOS=linux GOARCH=arm64 go build -o bin/command-code-proxy-arm64
GOOS=windows GOARCH=amd64 go build -o bin/command-code-proxy.exe
```

## API key behavior

The proxy uses the API key in this order:

1. `Authorization` header from the incoming client request
2. `-api-key` CLI value
3. If neither exists, the request returns `401 Unauthorized`

Header format:

```http
Authorization: Bearer your-commandcode-api-key
```

## Endpoints

### Health check

```http
GET /health
```

Response:

```json
{"status":"ok"}
```

### List models

```http
GET /v1/models
```

Returns an OpenAI-compatible model list. By default, closed/premium models are filtered out; start the proxy with `-list-closed-models` to include them.

The list is hand-curated (see `internal/proxy/proxy.go` — `fallbackModels`); new CommandCode models need a code update to appear.

### Chat completions

```http
POST /v1/chat/completions
```

`POST /chat/completions` is also registered as an OpenAI-compatible alias.

Example non-streaming request:

```bash
curl http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-commandcode-api-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "system", "content": "You are helpful."},
      {"role": "user", "content": "Hello"}
    ],
    "stream": false
  }'
```

Example streaming request:

```bash
curl -N http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-commandcode-api-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "Write a short poem."}
    ],
    "stream": true
  }'
```

Reasoning content, if any, is exposed in `choices[0].message.reasoning_content` (non-streaming) and in `choices[0].delta.reasoning_content` (streaming).

### Responses

```http
POST /v1/responses
```

Partial OpenAI Responses API shim. `input` can be a string or an array of role/content items; `instructions` is converted to a system message; `max_output_tokens` / `max_completion_tokens` are forwarded. `truncation`, `metadata`, `previous_response_id`, `store`, `user`, and the `reasoning` parameter are not supported.

Example request:

```bash
curl http://127.0.0.1:55990/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-commandcode-api-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "instructions": "You are concise.",
    "input": "Explain what this proxy does in one sentence.",
    "max_output_tokens": 200
  }'
```

## Supported model aliases

The proxy accepts full model IDs and these short aliases:

| Alias | Maps to |
| --- | --- |
| `deepseek-v4-pro`, `deepseek-v4`, `deepseek-pro` | `deepseek/deepseek-v4-pro` |
| `deepseek-v4-flash`, `deepseek-flash` | `deepseek/deepseek-v4-flash` |
| `minimax-m2.7`, `minimax2.7` | `MiniMaxAI/MiniMax-M2.7` |
| `minimax-m2.5`, `minimax2.5`, `minimax` | `MiniMaxAI/MiniMax-M2.5` |
| `glm-5.1` | `zai-org/GLM-5.1` |
| `glm-5` | `zai-org/GLM-5` |
| `kimi-k2.6`, `kimi2.6` | `moonshotai/Kimi-K2.6` |
| `kimi-k2.5`, `kimi2.5` | `moonshotai/Kimi-K2.5` |
| `qwen-3.6-max-preview`, `qwen3.6-max` | `Qwen/Qwen3.6-Max-Preview` |
| `qwen-3.6-plus`, `qwen3.6-plus`, `qwen3.6` | `Qwen/Qwen3.6-Plus` |
| `step-3.5-flash`, `step3.5` | `stepfun/Step-3.5-Flash` |
| `gemini-3.1-flash-lite`, `gemini-flash-lite` | `google/gemini-3.1-flash-lite` |
| `minimax-m3`, `minimax3` | `MiniMaxAI/MiniMax-M3` |
| `qwen-3.7-max-free`, `qwen3.7-max-free` | `Qwen/Qwen3.7-Max-Free` |
| `qwen-3.7-max`, `qwen3.7-max` | `Qwen/Qwen3.7-Max` |
| `step-3.7-flash`, `step3.7` | `stepfun/Step-3.7-Flash` |
| `mimo-v2.5-pro`, `mimo-pro` | `xiaomi/mimo-v2.5-pro` |
| `mimo-v2.5`, `mimo` | `xiaomi/mimo-v2.5` |

Unknown model names are passed through unchanged.

## Project structure

```text
.
├── README.md
├── AGENTS.md            # goals, scope, process discipline
├── MAINTAINING.md        # parity-test mechanics, request-fidelity table, release checklist
├── ROADMAP.md            # time-bound development plan
├── go.mod
├── go.sum
├── main.go
├── scripts
│   └── diff-captures.sh  # diff proxy captures against real binary captures
└── internal
    ├── api
    │   ├── commandcode.go
    │   └── openai.go
    ├── proxy
    │   ├── adapter.go         # real CommandCode API client
    │   ├── assembler.go       # response-side dispatcher (stream + non-stream)
    │   ├── assembler_test.go  # per-event-class unit tests
    │   ├── config.go          # config FS population + porcelain parser
    │   ├── convert.go         # OpenAI ↔ CommandCode message/tool format
    │   ├── convert_test.go
    │   ├── handler.go         # per-endpoint HTTP handlers
    │   ├── handler_test.go    # end-to-end HTTP tests
    │   ├── logging.go         # slog + request-scoped logging middleware
    │   ├── model.go
    │   ├── model_test.go
    │   ├── proxy.go           # Proxy struct + request body builder
    │   ├── router.go          # route registration + middleware
    │   ├── translator.go      # NDJSON event decoder
    │   ├── config_test.go     # config FS + porcelain tests
    │   ├── adapter_test.go    # adapter retry + backoff tests
    │   └── paritytest/        # vendored pre-refactor code + parity harness
    └── version
        └── version.go
```

## How it works

1. Client sends an OpenAI-shaped request to the local proxy.
2. The proxy extracts system messages, maps the model name, and converts messages to CommandCode format.
3. The proxy sends the request to `https://api.commandcode.ai/alpha/generate`.
4. CommandCode streaming NDJSON events are converted back to OpenAI-shaped SSE chunks or collected into a single JSON response.
5. The response side has a parity test (in `internal/proxy/paritytest/`) that pins the wire format byte-for-byte against a vendored copy of the pre-refactor dispatcher, so any future change to streaming or finish-reason semantics must either match the old behavior or be explicitly classified as an intentional improvement.

Every upstream request is sent with `stream: true`. For non-streaming clients, the proxy buffers the NDJSON stream and assembles the final JSON response.

## Request ID and observability

Every request gets a unique `X-Request-Id` (returned in the response header) and the same ID is forwarded to CommandCode as `x-request-id`.

All log lines are structured (Go `slog`). Default is human-readable text; set `PROXY_LOG_JSON=1` for JSON output suitable for `jq`.

**Request capture** — `-capture-dir` writes two files per request:
```
chatcmpl-xxx.request.json   ← full request body sent to CommandCode
chatcmpl-xxx-*.ndjson       ← raw upstream response (if upstream responds)
```
The request is captured before the upstream call, so it exists even on 401s or transport errors.

**Diff against real binary** — `scripts/diff-captures.sh` compares proxy captures against real `command-code` binary captures from `cmd-recorder`:
```bash
# Capture from the proxy
./bin/command-code-proxy -capture-dir ./proxy-captures

# Capture from the real binary (see MAINTAINING.md)
cd /path/to/project
COMMANDCODE_SANDBOX=true COMMANDCODE_API_URL=http://127.0.0.1:9090 \
  command-code --skip-onboarding -p "hello"

# Diff
./scripts/diff-captures.sh ./proxy-captures ../cmd-recorder/captures
```
The script normalizes both captures with `jq -S` and runs unified diff. Known expected differences (memory, taste, skills, threadId) are called out in the summary.

## CommandCode request context

The proxy impersonates the real `command-code` binary. The upstream request includes:

**Config fields** (sent in `config`):
- `workingDir` — from the pi cc-cwd extension or `-working-dir` flag (not the proxy's own checkout dir)
- `environment` — hardcoded `"linux-x64, Node.js v26.2.0"` (matches the real CLI, not the proxy's actual runtime)
- `date` — current date (`YYYY-MM-DD`)
- `structure` — top-level subdirectory names from `workingDir` (filtered and sorted)
- `isGitRepo` — `true` if `.git` exists in `workingDir`
- `currentBranch` — `git branch --show-current`
- `mainBranch` — `main` or `master` from `git branch -r`
- `gitStatus` — `git status --porcelain` summary (`"M N, A N, D N, ?? N"` or `"Working tree clean"`)
- `recentCommits` — `git log --oneline -3`

These fields are read from the live filesystem if the pi extension does not send a pre-populated `x_command_code_config`. The proxy's own checkout dir is intentionally not used — using it would leak the proxy's `go.mod` and `internal/` into the gateway's system prompt.

**Other request fields:**
- `permissionMode` — `"auto-accept"`
- `params.stream` — always `true` upstream
- `threadId` — UUID generated per request (session continuity with CommandCode)
- `memory` — AGENTS.md content from the project (sent by the pi extension)
- `skills` — XML from `.agents/skills/` or `.pi/skills/` or `.commandcode/skills/` (sent by the pi extension)
- `taste` — `.commandcode/taste/taste.md` if it exists (sent by the pi extension, usually null)

## CommandCode version header

The upstream request includes:

```http
x-command-code-version: <latest npm command-code version>
```

The value is fetched from:

```text
https://registry.npmjs.org/command-code/latest
```

The fetched version is cached for 30 minutes. If the registry request fails, the proxy uses the last cached version, or `unknown` if no version has been fetched yet.
