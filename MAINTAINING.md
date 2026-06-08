# MAINTAINING.md

Mechanism-rich reference for working on this proxy. **Read this when you're about to change the response side, debug a request-fidelity question, or cut a release.** For the project's shape, intent, and goals, see [AGENTS.md](AGENTS.md). For the time-bound plan, see [ROADMAP.md](ROADMAP.md).

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

This means the proxy must **drop** all system/developer messages from the OpenAI request before forwarding. If they leak through (even rewritten to `role: "user"`), the model sees the AGENTS.md content as a user turn and gets confused — MiniMax-M3 produced "Working directory: `/home/daniel/build/<project>`. Ready for your next request." hallucinations when the old `normalizeRole` rewrite turned `system → user`.

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

**All "stopgap" fields are populated by `populateConfigFromFS` in `proxy.go`, which shells out to `git` and reads the project directory using the `workingDir` header. This works on local deployment but is architecturally wrong — the proxy has no business reading the project's filesystem. See AGENTS.md § "The proxy has no project context" for the constraint and ROADMAP.md § 2.8 for the fix (pi extension sends this data).**

**`workingDir` is sent in every request, not just the first.** Verified across 4 separate `command-code` runs in the same project — all carried the full `config` block including `workingDir`. Each `-p` run is a new session (new `threadId`) but sends the same `config`.

**The gateway uses these fields to build the server-side system prompt.** MiniMax-M3 hallucinated "automated environment update" responses when the proxy sent stub values (empty `structure`, `isGitRepo: false`, `"Go proxy"` environment). Populating the fields from the live filesystem fixed the hallucination. See § Reverse-engineered binary behavior below for the exact logic the real binary uses.

### Reverse-engineered binary behavior (`dist/index.mjs`, v0.32.2)

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

The full request body for the main agent call is built in `prepareServerCall()`:
```
{
  config:     getEnvironmentContext(),
  memory:     await getMemoryContent(lastFileContexts),  // reads AGENTS.md
  taste:      await getTasteContent(),                    // reads .commandcode/taste/taste.md
  skills:     generateSkillsXML(allSkills) || null,       // XML from .commandcode/skills/
  permissionMode: <mode>,                                 // from CLI flags
  params: { model, messages, tools, max_tokens, stream }
  threadId:   sessionId,
}
```

**Headers** the real binary sends:
- `x-cli-environment`: `${os.platform()}-${os.arch()}, Node.js ${process.version}`
- `x-command-code-version`: CLI version string
- `x-project-slug`: project dir name
- `Authorization: Bearer <token>` (from OAuth or API key)
- `Content-Type: application/json`

### Message roles

Only three roles are valid in `params.messages[]`: `user`, `assistant`, `tool`. The proxy's `normalizeRole` function maps OpenAI roles to these; any role that doesn't map returns `""` and `ConvertMessages` drops the message. This is a programmer-error safety net — `DropSystemMessages` is the real gatekeeper.

### Ground truth

The `cmd-recorder` captures in `../cmd-recorder/captures/` are the canonical reference for what the real binary sends. When debugging request-fidelity questions, diff the proxy's output against these captures. The request-shape parity test in `paritytest/cmdcode_shape_test.go` automates this.

### Thread ID and session continuity

The real binary sends a `threadId` (UUID) at the top level of the request body. `-p` mode generates a new `threadId` per invocation. Interactive mode reuses the same `threadId` across turns within a session. The proxy should pass `threadId` through verbatim from any client that provides one (the OpenAI API doesn't have an equivalent — the proxy can generate a UUID per request and reuse it for the duration of an OpenAI `n` choice or a streaming session).

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

In addition to the response-side parity test, `paritytest/cmdcode_shape_test.go` asserts the proxy's **request** shape matches the real `command-code` binary's shape. Four tests:

- `TestCommandCodeShape_NoSystemRoleInMessages` — reads the most recent capture from `../cmd-recorder/captures/` and asserts no `system`/`developer` role appears in `params.messages[]`. Skip-not-fail if the recorder dir is absent.
- `TestProxyRequestShape_DropsSystemMessages` — feeds a pi-shaped OpenAI request (5 messages, 2 system) into `BuildCCRequest`; asserts 3 messages survive and none are system/developer.
- `TestProxyRequestShape_NoAgentsMDLeakage` — no message body opens with `# AGENTS.md` or `# Project-specific instructions`. This is the visible symptom guard — it catches the specific failure mode that caused MiniMax-M3 hallucinations.
- `TestProxyRequestShape_ConfigWorkingDirSet` — `Config.WorkingDir` is non-empty.

These tests were verified to catch the regression: temporarily reverting `DropSystemMessages` causes the leakage tests to fail with the exact symptoms (`len(messages) = 5, want 3` and a user turn containing the AGENTS.md header).

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
