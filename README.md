# CommandCode Proxy Server

OpenAI-compatible proxy server for the CommandCode API. It exposes `/v1/chat/completions`, `/chat/completions`, `/v1/responses`, and `/v1/models` endpoints so OpenAI-compatible clients can call CommandCode models through a local HTTP server.

Repository: https://github.com/bermudi/cmd-code-proxy

Version: `v1.0.8`

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

### Responses

```http
POST /v1/responses
```

Accepts a subset of OpenAI Responses API requests and rewrites them internally to chat completions. `input` can be a string or an array of role/content items; `instructions` are converted to a system message.

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
├── go.mod
├── go.sum
├── main.go
└── internal
    ├── api
    │   ├── commandcode.go
    │   └── openai.go
    ├── proxy
    │   ├── convert.go
    │   ├── convert_test.go
    │   ├── model.go
    │   ├── model_test.go
    │   ├── proxy.go
    │   └── proxy_test.go
    ├── server
    │   └── server.go
    └── version
        └── version.go
```

## How it works

1. Client sends an OpenAI-compatible request to the local proxy.
2. The proxy extracts system messages, maps the model name, and converts messages to CommandCode format.
3. The proxy sends the request to `https://api.commandcode.ai/alpha/generate`.
4. CommandCode streaming NDJSON events are converted back to OpenAI-compatible SSE chunks or collected into a single JSON response.

Every upstream request is sent with `stream: true`. For non-streaming clients, the proxy buffers the NDJSON stream and assembles the final JSON response.

## CommandCode request context

The upstream request includes CLI-compatible context fields and headers:

- `config.workingDir` is the proxy process working directory.
- `config.environment` is `cli`.
- `memory`, `taste`, and `skills` are sent as JSON `null`.
- `x-cli-environment: production`
- `x-project-slug: <slugified working directory>`
- `x-taste-learning: true`
- `x-co-flag: false`

If you run the proxy as a long-lived service, start it from the project directory you want CommandCode to see.

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
