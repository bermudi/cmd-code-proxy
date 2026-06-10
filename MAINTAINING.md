# MAINTAINING.md

Mechanism-rich reference for working on this proxy. **Read this when you're about to change the client-facing output, debug a request-fidelity question, or cut a release.** For the project's shape, intent, and goals, see [AGENTS.md](AGENTS.md). For the time-bound plan, see [ROADMAP.md](ROADMAP.md).

## CommandCode gateway protocol

Notes on how the real `command-code` binary talks to `api.commandcode.ai/alpha/generate`. Ground truth comes from the captures in `../cmd-recorder/captures/` (and fresh captures — see § Capturing real binary traffic below).

### Capturing real binary traffic

`command-code` does not respect `COMMANDCODE_API_URL` for routing unless `COMMANDCODE_SANDBOX=true` is **also** set. The minified `getApiBaseUrl()` in `dist/index.mjs` is:

```js
function getApiBaseUrl(){
  const sandbox = "true" === process.env.COMMANDCODE_SANDBOX;
  const override = process.env.COMMANDCODE_API_URL;
  if (sandbox && override) return override;
  if (process.argv.includes("--local")) return "http://localhost:9090";
  if (process.argv.includes("--staging")) return "https://staging-api.commandcode.ai";
  return "https://api.commandcode.ai";
}
```

Two ways to point it at a local capture proxy:

1. **`--local` flag** — hardcodes `http://localhost:9090`. Useful when cmd-recorder is on the default port.
2. **`COMMANDCODE_SANDBOX=true COMMANDCODE_API_URL=http://127.0.0.1:<port>`** — works with any port. **Both env vars are required**; `COMMANDCODE_API_URL` alone is silently ignored.

**Procedure for a fresh capture in a project:**

```sh
# Start cmd-recorder (it listens on :9090 by default and captures to ./captures/)
cd /tmp/cmd-recorder-<project> && mkdir -p captures
/home/daniel/build/cmd-recorder/cmd-recorder ./captures :9090 &

# Run command-code pointed at the recorder
cd /home/daniel/<project>
COMMANDCODE_SANDBOX=true COMMANDCODE_API_URL=http://127.0.0.1:9090 \
  command-code --skip-onboarding -p "<prompt>"

# Captures land in /tmp/cmd-recorder-<project>/captures/<ts>_POST_alpha_generate.json
```

Multi-turn: repeat the `command-code` call; each `-p` is a new session (new `threadId`) but the `config` block is sent in full every time. For a true multi-turn session within one `threadId`, interactive mode is needed (requires a TTY).

### System prompt handling

**The gateway builds the system prompt server-side.** Across every capture, `params.messages[]` contains only `user | assistant | tool` — never `system`, never `developer`. The binary sends `config.workingDir` and the gateway reads the project's AGENTS.md, skills, etc. from disk itself.

This means the proxy must **drop** all system/developer messages from the OpenAI request before forwarding. If they leak through (even rewritten to `role: "user"`), the model sees the AGENTS.md content as a user turn and responds to it as an environment announcement — MiniMax-M3 reasonably produced "Working directory: `/home/daniel/build/<project>`. Ready for your next request." acks when the old `normalizeRole` rewrite turned `system → user`. Not a hallucination; the model was correctly interpreting malformed input.

The drop happens in `convert.go:DropSystemMessages`, called before `ConvertMessages` in `BuildCCRequestWithWorkingDir`. If all messages are system, the handler returns a 400 with a useful message.

### Config fields

The binary sends a `config` struct with fields the gateway uses for routing and system-prompt construction. **Captured from a real `command-code` run in `/home/daniel/Documents/NewsWiki` on 2026-06-08:**

| Field | What the real binary sends | What the proxy sends | Gap? |
| --- | --- | --- | --- |
| `workingDir` | `/home/daniel/Documents/NewsWiki` (actual cwd) | From `cc-cwd` extension or `-working-dir` flag | ✓ correct |
| `date` | `2026-06-08` | `time.Now().Format("2006-01-02")` | ✓ correct |
| `environment` | `linux-x64, Node.js v26.2.0` | `"linux-x64, Node.js v26.2.0"` (hardcoded, matches binary) | ✓ fixed |
| `structure` | `["meta", "raw", "scripts", "wiki"]` (top-level subdirs, filtered + sorted) | `populateConfigFromFS(workingDir)` — reads project dir | ✓ stopgap |
| `isGitRepo` | `true` | `populateConfigFromFS(workingDir)` — checks `.git` | ✓ stopgap |
| `currentBranch` | `"main"` | `populateConfigFromFS(workingDir)` — `git branch --show-current` | ✓ stopgap |
| `mainBranch` | `"main"` | `populateConfigFromFS(workingDir)` — `git branch -r` parse | ✓ stopgap |
| `gitStatus` | `"Working tree clean"` or `"M N, A N, D N, ?? N"` | `populateConfigFromFS(workingDir)` — summarized porcelain | ✓ stopgap |
| `recentCommits` | 3 commits with `hash subject` format (`git log --oneline -3`) | `populateConfigFromFS(workingDir)` — `git log --oneline -3` | ✓ stopgap |

**Config resolution:** The proxy's `resolveConfig()` checks for `x_command_code_config` from the pi extension first. If present (all fields populated), it uses it verbatim — the extension runs in the project directory and has the real data. If absent (curl, non-pi clients), it falls back to `populateConfigFromFS` as a local-deployment stopgap that shells out to `git` and reads the project directory using the `workingDir` header. See AGENTS.md § "The proxy has no project context" for the constraint.

**Top-level request fields beyond `config`:** The real binary also sends `memory`, `skills`, `taste`, and `threadId` at the request body level.

| Field | What the real binary sends | What the proxy sends | Source |
| --- | --- | --- | --- |
| `memory` | AGENTS.md content (multi-KB markdown) | AGENTS.md content from project | `x_command_code_memory` from pi extension |
| `skills` | XML from `.commandcode/skills/` or `null` | XML from `.agents/skills/` or `.pi/skills/` or `.commandcode/skills/` | `x_command_code_skills` from pi extension |
| `taste` | `.commandcode/taste/taste.md` content or `null` | `null` (no taste file in most projects) | `x_command_code_taste` from pi extension when present |
| `threadId` | UUID per session (reused across turns in interactive mode) | UUID generated per request (`uuid.New()`) | Generated by the proxy (OpenAI API has no session identifier) |

When the pi extension does not send `x_command_code_memory`/`x_command_code_skills`, the proxy sends `null`/`""` — the gateway falls back to reading these from disk using `config.workingDir`.

**`workingDir` is sent in every request, not just the first.** Verified across 4 separate `command-code` runs in the same project — all carried the full `config` block including `workingDir`. Each `-p` run is a new session (new `threadId`) but sends the same `config`.

**The gateway uses these fields to build the server-side system prompt.** MiniMax-M3 was distracted by stub config values (empty `structure`, `isGitRepo: false`, `"Go proxy"` environment) — the gateway's server-side prompt looked like a generic environment announcement, so the model reasonably responded with "automated environment update" acks. It was not hallucinating, just interpreting the bad input correctly. Populating the fields from the live filesystem fixed the distraction. See § Reverse-engineered binary behavior below for the exact logic the real binary uses.

**Theory (2026-06-08): The missing `threadId` may have been a contributing factor.** The proxy used to omit `threadId` entirely, while the real binary sends a UUID per session. If the gateway constructs the system prompt only on the first message of a session and caches it for subsequent turns, then every proxy request was a *new session* with a fresh system prompt. The model would see the environment context as new each time and respond with "automated environment update" acks. The `threadId` fix (generating one per request) makes the proxy's request shape match the real binary's session model.

### Proxy vs real binary — remaining fidelity gaps

As of 2026-06-08 (reverse-engineered from v0.33.1, verified against captures).

**We're mimicking `callServerAPI()`** (interactive/agent mode, line 5255), which is the path pi exercises. The `callApi()` path (line 4729, `-p`/`--print` mode) is a different code path with a different body shape — captures in `cmd-recorder/captures/` exercise that path because it's easier to capture, but it's not the target.

#### Two API call paths (context)

The real binary has two distinct `POST /alpha/generate` code paths:

| Aspect | `callApi()` (line 4729) | `callServerAPI()` (line 5255) ← target |
| --- | --- | --- |
| Used by | `-p` / `--print` mode | Interactive/agent mode |
| `params.temperature` | `0.3` | absent — correct, proxy doesn't send it either |
| `threadId` at top level | present (UUID) | absent |
| `permissionMode` | absent | `"standard"` / `"default"` / `"auto-accept"` |
| `system` in params | absent | present for agent/system-prompt calls |
| `reasoning_effort` in params | absent | present when model has `reasoningEfforts[]` in registry |

Full JSON body shapes for both paths are in § Reverse-engineered binary behavior below.

#### Gap: `threadId` at top level

The proxy sends `threadId` at the top level (from `newThreadID()`), but `callServerAPI()` does not — it manages sessions separately. The `callApi()` path includes `threadId`, but we're not mimicking that path.

**Current status:** This is harmless — the gateway likely accepts the field and treats it as session identifier. It's also what the previous distraction fix added (see theory in § Config fields). Leaving it in because it's been tested and works; removing it could reintroduce the session-construction problem.

#### Gap: `reasoning_effort` not forwarded

**What the real binary does:** `callServerAPI()` conditionally includes `reasoning_effort` in `params` via `...e.reasoningEffort?{reasoning_effort:e.reasoningEffort}:{}`. The value comes from `prepareServerCall()` which returns `reasoningEffort: o` where `o` is the user's configured reasoning effort from config.

**What the proxy does:** No `reasoning_effort` field in `CCChatParams`. The OpenAI API doesn't have a `reasoning_effort` field either, so the proxy can't forward what it doesn't receive. The real binary sets it from its own config — the proxy would need to derive it from the model selection or client hints.

**Practical impact:** Effectively none for the current model set. The proxy's main use case is MiniMax-M3, and the binary's model registry shows `MiniMax-M3: { reasoning: !0 }` with **no `reasoningEfforts[]` array** — meaning the binary has nothing to set, so `e.reasoningEffort` is `undefined` and the field is absent from the body. The proxy is accidentally correct for the model it actually serves. For models that *do* expose `reasoningEfforts[]` (Claude Opus 4.8, GPT-5.5, DeepSeek V4 Pro, etc.), the binary sets it from the user's `/effort` command; the proxy has no equivalent signal and would default to the gateway's own fallback. Documented for when a user runs the proxy against a reasoning-effort-configurable model.

#### What the proxy already gets right (post-fix verification)

These were the three root causes of the MiniMax-M3 distraction and are **confirmed fixed**:

| Fix | Status | Verification |
| --- | --- | --- |
| Config fields populated (no stubs) | ✓ fixed | `populateConfigFromFS` impersonates binary |
| `threadId` sent (UUID per request) | ✓ fixed | `newThreadID()` generates UUID |
| System messages dropped | ✓ fixed | `DropSystemMessages` + parity test guards |
| `temperature` absent (matches `callServerAPI`) | ✓ correct | — |
| `permissionMode` sent (matches `callServerAPI`) | ✓ correct | — |
| `x-cli-environment`, `x-project-slug`, `x-co-flag` headers | ✓ sent | values match what the binary sends for API-key auth |
| `x-taste-learning: <resolved value>` | ✓ resolved | `XCommandCodeTasteLearning` (per-request) > `-taste-learning` flag > `true` default. See § Taste learning. |
| `x-session-id` | ✓ **sent** | pi extension sends `sess_`+16hex per session; proxy generates fallback if absent |

### Reverse-engineered binary behavior (`dist/index.mjs`, v0.33.1)

Reverse-engineered from the minified 1.4MB ESM bundle at `/home/daniel/.local/share/pnpm/global/v11/3f63-19ea7f0cb15/node_modules/command-code/dist/index.mjs`. Key functions extracted by grepping for named function declarations and tracing the call graph.

The real binary constructs the `config` struct via `getEnvironmentContext()` in `src/utils/environment.ts`. Here's the exact logic, extracted from the minified source:

```
getEnvironmentInfo()        → ${os.platform()}-${os.arch()}, Node.js ${process.version}
getCurrentWorkingDirectory() → global.COMMAND_CODE_CWD || process.cwd()
getCurrentDate()             → new Date().toISOString().split('T')[0]

getRootDirectoryStructure() → readdirSync(cwd, {withFileTypes:true})
                               .filter(isDir)
                               .filter(!startsWith('.'))
                               .filter(!in blocklist: [node_modules, dist, build,
                                 .git, .svn, .hg, coverage, .nyc_output,
                                 .cache, tmp, temp, .next, .nuxt, out])
                               .map(name)
                               .sort()

isGitRepository()           → execSync('git rev-parse --git-dir', {stdio:'ignore'})
getCurrentBranch()          → execSync('git branch --show-current').trim()
getMainBranch()             → execSync('git branch -r')
                               includes('origin/main') → 'main'
                               includes('origin/master') → 'master'
                               fallback → 'main'

getGitStatus()              → execSync('git status --porcelain').trim()
                               if empty → 'Working tree clean'
                               else summarize: M N, A N, D N, ?? N

getRecentCommits()          → execSync('git log --oneline -3').trim().split('\n')

getAdditionalDirectories()  → user-added scope dirs (empty by default)
                               prefixed with 'scope:' in structure
```

#### Memory collection

```
getMemoryContent(lastFileContexts)     → reads AGENTS.md (project + user + enterprise),
                                         resolves @import references, deduplicates.
                                         Returns null on error (gateway falls back to
                                         reading disk using config.workingDir).
```

#### Skills XML generation

```
generateSkillsXML(allSkills)           → deduplicates skills, wraps each in <skill> XML.
                                         Returns null when no skills are found.
                                         Discovers from: .commandcode/skills/,
                                         .agents/skills/, ~/.commandcode/skills/,
                                         and bundled skills.
```

#### Taste collection

```
getTasteContent()                      → reads .commandcode/taste/taste.md.
                                         Strips [cmd] reference link. Returns null
                                         when no taste file exists.
```

#### Two distinct API call paths

The real binary has **two separate code paths** for calling `/alpha/generate`:

**Path A: `callApi()` — `-p` / `--print` mode (line 4729)**
```json
{
  "config": { ...getEnvironmentContext() },
  "memory": "<AGENTS.md content or null>",
  "taste": "<taste.md content or null>",
  "skills": "<skills XML or null>",
  "params": {
    "tools": [...],
    "stream": true,
    "max_tokens": 64000,
    "temperature": 0.3,
    "messages": [...],
    "model": "<modelId>"
  },
  "threadId": "<uuid>"
}
```

**Path B: `callServerAPI()` — interactive/agent mode (line 5255)**
```json
{
  "config": { ...getEnvironmentContext() },
  "memory": "<AGENTS.md content or null>",
  "taste": "<taste.md content or null>",
  "skills": null,
  "permissionMode": "auto-accept" | "default" | "standard",
  "params": {
    "model": "<modelId>",
    "messages": [...],
    "tools": [...],
    "system": "<system prompt (only some calls)>",
    "max_tokens": 64000,
    "stream": true,
    "reasoning_effort": "low" | "medium" | "high" | "xhigh" | "max"  // only when model.reasoningEfforts[] is set
  }
}
```

**Special-cased API calls** (tool description, compact, title-gen, custom-agent, observation) add a `mode` field at the top level (`"tool-desc"`, `"compact"`, `"title-gen"`, `"custom-agent"`) and include `system` inside `params`. These are not the main agent interaction path.

**Session continuity:** `callApi` sends `threadId` (UUID) per session — new UUID per `-p` invocation, reused across turns in interactive mode via `sessionManager.getSessionId()`. `callServerAPI` manages sessions externally (not via `threadId` in the body).

**The proxy's current shape is a hybrid:** it includes `permissionMode` (from path B) AND `threadId` (from path A). See § Proxy vs real binary — remaining fidelity gaps above.

#### Headers

The real binary's `callServerAPI()` (line 5255, the path we mimic) sets:

- `Content-Type: application/json`
- `Authorization: Bearer <token>` (OAuth) — or no auth header if the user is using the CLI with `COMMAND_CODE_API_KEY` instead
- `x-cli-environment`: one of `"production"`, `"staging"`, or `"development"` (determined by telemetry env)
- `x-command-code-version`: CLI version from `package.json`
- `x-project-slug`: project dir name (slugified via `getCurrentProjectDirName()`)
- `x-taste-learning`: `isTasteLearningEnabled().toString()` — `"true"` if user has taste learning enabled, `"false"` if disabled. **See § Taste learning for the full story.**
- `x-co-flag` (misnamed — internally `INTERNAL_TEAM_FLAG_HEADER`): `isOAuthEnforced().toString()`. Despite the name, this is *not* a team flag — it's the OAuth-enforcement signal. For a proxy running with an API key, the value is `"false"`.
- `x-session-id`: the session's ID in `sess_` + 16 hex chars format. The pi extension generates one per pi session and sends it as `x_command_code_session_id`; the proxy generates a per-request fallback if absent.
- `x-oss-primary-provider`: OSS primary provider (when `OSS_PRIMARY_PROVIDER` env var is set)
- `x-oauth-token`: OAuth bearer token (only when using external provider)
- `x-oauth-provider`: OAuth provider name (only when using external provider)
- `x-cmd-zdr`: `"1"` when `CMD_ZDR` env var is set
- `traceparent`: OpenTelemetry tracing header (when span context exists)

The `callApi()` path (-p mode) is the same set minus `x-session-id` (each `-p` is a fresh session).

**Proxy headers** (in `adapter.go:createUpstreamRequest`):
- `Content-Type: application/json` ✓
- `Authorization: Bearer <apiKey>` ✓
- `x-command-code-version`: from NPM version provider ✓
- `x-cli-environment`: hardcoded `"production"` ✓
- `x-project-slug`: slugified from workingDir ✓
- `x-taste-learning`: resolved via 3-tier precedence (per-request > flag > `true` default). See § Taste learning. ✓
- `x-co-flag`: hardcoded `"false"` ✓ — correct for API-key auth.
- `x-session-id`: pi extension sends `sess_`+16hex per session; proxy generates per-request fallback if absent. ✓
- `Accept: text/event-stream` ✓ (proxy-specific)
- `X-Request-Id`: proxy request ID (when available) ✓ (proxy-specific)

#### Taste learning

`isTasteLearningEnabled()` (in `src/utils/user-config.ts`):
```js
async function isTasteLearningEnabled() {
  return (await loadUserConfig()).tasteLearning ?? !0  // default true
}
```

The user toggles this in the TUI: `/taste` in interactive mode, or the "Taste Learning" configure dialog (which sets `userConfig.tasteLearning` to `false`). The flag is persisted in the user's config file.

**What the toggle controls** (and what it doesn't):

| Field | Gated by `isTasteLearningEnabled()`? | Notes |
| --- | --- | --- |
| `x-taste-learning` header | **Yes** | `"true"` or `"false"` per the toggle. This is the user-config signal to the gateway. |
| `taste` body field | No | `getTasteContent()` runs unconditionally; the body field is sent (with content or `null`) regardless of the toggle. The toggle does not prevent the model from *seeing* taste content — it only signals the user's preference to the gateway. |
| Internal taste-learning agent | Yes (separately) | `getTasteLearningAgentModel()` returns a model only when taste learning is active; the separate `/alpha/learn` endpoint is used for the learning agent itself. |

**Practical impact of the proxy's hardcoded `"true"`:** The proxy lies about the user's preference. If the user's `command-code` has taste learning **off**, the gateway still sees `x-taste-learning: "true"`. We don't know if the gateway uses this for routing/model selection (the captures don't disambiguate), but it's a real wire-format divergence that contradicts what a fresh `command-code` install with the same user config would send.

**Resolution (2026-06-08):** The header is now resolved with explicit precedence:
1. `x_command_code_taste_learning` field on the OpenAI request (per-request, set by the pi extension)
2. `-taste-learning` CLI flag on the proxy (proxy-wide default, default `true` for backward compat)
3. `true` (the binary's default for unset config)

The pi `cc-cwd` extension reads `~/.commandcode/config.json` `tasteLearning` field and forwards it. `ResolveTasteLearning(perRequest, proxyDefault)` in `proxy.go` picks the right value. The `Upstream` interface gained a `tasteLearning bool` parameter on `Generate` so the resolved value reaches the adapter per-request.

#### `x-co-flag` is `isOAuthEnforced()`, not a "team flag"

The constant name `INTERNAL_TEAM_FLAG_HEADER` is a vestigial from when this header carried a CommandCode-team-internal flag. In v0.33.1, `callServerAPI()` (and `callApi()`) set it to `isOAuthEnforced().toString()`. The proxy's hardcoded `"false"` is correct for an API-key auth deployment; it'd need to become `"true"` only if the proxy were ever driven via OAuth.

### Message roles

Only three roles are valid in `params.messages[]`: `user`, `assistant`, `tool`. The proxy's `normalizeRole` function maps OpenAI roles to these; any role that doesn't map returns `""` and `ConvertMessages` drops the message. This is a programmer-error safety net — `DropSystemMessages` is the real gatekeeper.

### Ground truth

The `cmd-recorder` captures in `../cmd-recorder/captures/` are the canonical reference for what the real binary sends. When debugging request-fidelity questions, diff the proxy's output against these captures. The request-shape parity test in `paritytest/cmdcode_shape_test.go` automates this.

### Thread ID and session continuity

The real binary sends a `threadId` (UUID) at the top level of the request body. `-p` mode generates a new `threadId` per invocation. Interactive mode reuses the same `threadId` across turns within a session. The proxy should pass `threadId` through verbatim from any client that provides one (the OpenAI API doesn't have an equivalent — the proxy can generate a UUID per request and reuse it for the duration of an OpenAI `n` choice or a streaming session).

## Parity test mechanics

The client-facing output is the hard contract of this proxy. The parity test in `internal/proxy/paritytest/` is the safety net. It feeds the same NDJSON through a vendored copy of the pre-refactor dispatcher and the current code, then diffs the bytes. Streaming is asserted byte-identical; non-streaming is asserted class-equivalent via per-fixture `expected` maps.

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

What the proxy accepts from clients and translates into upstream requests. **Until this lands as a comment in `main.go` (see ROADMAP Phase 2.3), this file is the source of truth.**

| Field | Status |
| --- | --- |
| `model` | ✓ mapped via alias table, forwarded |
| `messages` | ✓ converted via `ConvertMessages` (system/developer **dropped** by `DropSystemMessages`, tool_calls/tool_result content parts, image URLs, thinking/reasoning) |
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

## Request-shape parity test

In addition to the client-facing parity test, `paritytest/cmdcode_shape_test.go` asserts the proxy's **request** shape matches the real `command-code` binary's shape. Four tests:

- `TestCommandCodeShape_NoSystemRoleInMessages` — reads the most recent capture from `../cmd-recorder/captures/` and asserts no `system`/`developer` role appears in `params.messages[]`. Skip-not-fail if the recorder dir is absent.
- `TestProxyRequestShape_DropsSystemMessages` — feeds a pi-shaped OpenAI request (5 messages, 2 system) into `BuildCCRequest`; asserts 3 messages survive and none are system/developer.
- `TestProxyRequestShape_NoAgentsMDLeakage` — no message body opens with `# AGENTS.md` or `# Project-specific instructions`. This is the visible symptom guard — it catches the specific failure mode that caused MiniMax-M3 to interpret malformed input as an environment announcement.
- `TestProxyRequestShape_ConfigWorkingDirSet` — `Config.WorkingDir` is non-empty.

These tests were verified to catch the regression: temporarily reverting `DropSystemMessages` causes the leakage tests to fail with the exact symptoms (`len(messages) = 5, want 3` and a user turn containing the AGENTS.md header).

## Go HTTP middleware gotcha: `statusWriter` and interface assertions

`RequestLoggingMiddleware` wraps `http.ResponseWriter` in a `statusWriter` to capture the response status code. This breaks `w.(http.Flusher)` type assertions in downstream handlers unless `statusWriter` transparently delegates `Flush()` to the underlying writer.

**What would break:** Every streaming request would return 500 "Streaming not supported" because `HandleChatCompletions` does `w.(http.Flusher)` and `statusWriter` didn't implement `Flush()`. Unit tests with `httptest.ResponseRecorder` wouldn't catch this because `ResponseRecorder` already implements `Flusher`.

**The fix:** `statusWriter` delegates `Flush()`, `Hijack()`, and `WriteHeader()` transparently to the underlying `ResponseWriter`. When adding new middleware that wraps `ResponseWriter`, always check whether downstream handlers assert on `http.Flusher` or `http.Hijacker`.

**Request capture ordering:** The request body is written to `CaptureDir` **before** the upstream call, not after. This ensures the request file exists even when upstream returns a 401/500/timeout — exactly when you need to debug what was sent. The response tee is created only when upstream returns a body.

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
- [ ] If you changed the client-facing output, the parity test caught the diff and the diff is classified (see Parity test mechanics above).
- [ ] AGENTS.md and ROADMAP.md are updated if scope, goals, or the plan changed.

That's it. No tag, no version bump, no rebuild of all platforms.
