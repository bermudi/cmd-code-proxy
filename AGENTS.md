# AGENTS.md

## Project
Local HTTP proxy that exposes an OpenAI-compatible API and translates requests to CommandCode's proprietary `/alpha/generate` endpoint. Lets any OpenAI client use CommandCode as a backend.

## Stack
Go 1.26 · stdlib + `google/uuid` · no framework

## Architecture
```
OpenAI client → localhost:55990/v1/chat/completions (OpenAI format)
                      ↕  convert.go
              api.commandcode.ai/alpha/generate (NDJSON, content-parts format)
```

- **internal/proxy/convert.go** — OpenAI ↔ CommandCode message/tool format translation
- **internal/proxy/proxy.go** — request handling, streaming NDJSON→SSE, non-streaming assembly
- **internal/proxy/model.go** — short alias → full model ID mapping
- **internal/api/** — type definitions for both wire formats
- **internal/version/** — polls npm registry for latest command-code version (spoofs `x-command-code-version`)

CommandCode uses Anthropic-style content-parts (`[{type:"text",...},{type:"tool-call",...}]`), not OpenAI's flat structure. Three parallel tool streaming protocols exist (`tool-use`/`tool-delta`, `tool-input-start`/`tool-input-delta`, `tool-call`) — all must be handled.

## Conventions
- All upstream requests use `stream: true` — non-streaming mode buffers NDJSON and assembles a final response
- System messages are hoisted to a top-level `system` field, not left in the messages array
- Tool results with content starting with `"Error:"` get `type: "error-text"`

## Workflow
```sh
go build -o bin/command-code-proxy .
go test ./...

# start
./bin/command-code-proxy -api-key "$(cat ~/.pi/agent/CMD_API_KEY)"
# flags: -port (default 55990), -host (default 127.0.0.1), -api-key, -version

# verify
curl http://127.0.0.1:55990/health
curl http://127.0.0.1:55990/v1/models
```

## Constraints
- Default binds to localhost only — do not expose to network without auth
- API key can come from `-api-key` flag or `Authorization: Bearer` header per-request
- Model list at `/v1/models` is hardcoded, not fetched from upstream — needs manual updates when CommandCode adds models
