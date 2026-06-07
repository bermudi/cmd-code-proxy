# Roadmap

> **Status:** living document. Last reviewed: 2026-06-05.
> **Supersedes:** the ordering previously implied by AGENTS.md § Nice-to-haves. The Nice-to-haves section remains a permanent catalog of ideas; this document is the time-bound plan that picks which of them to do and in what order.

## Thesis

The next phase of work is about making the existing proxy more trustworthy in production, not about expanding its surface area. The parity test just landed; the first job is to make sure it carries weight, not just to add more features on top of it. Features follow reliability.

Three things justify this prioritization:

1. **The proxy is in personal use, and the current feature set is sufficient for the workflows it serves.** Adding `tool_choice` or `response_format` support is speculative until a real request hits the proxy that needs it.
2. **The biggest recent wins were about *noticing* things the existing tests missed.** The `finish_reason: length` → `stop` bug survived for some time because the unit tests verified the per-event contract but never verified the wire format end-to-end. The parity test fixes that. The next job is to extend the same discipline to the request side and to the upstream protocol drift.
3. **The proxy is observability-poor.** When something breaks, the only signal is `log.Printf` lines. That's fine for a personal tool that the maintainer runs interactively; it gets worse fast as soon as anything else changes (an upstream protocol change, a model switch, a different client). A request ID flowing end-to-end is the cheapest single improvement that unlocks every future debugging session.

## Phase 1 — reliability (next 1–2 sessions)

The goal of this phase is to make the proxy's behavior *measurable* and *debuggable* without adding new features. Each item ends the phase with a stronger signal about what the proxy is doing.

### 1.1 Real upstream parity fixtures

- **What:** Capture 2–3 real NDJSON streams from the live `command-code` binary (one short text, one with tool calls, one with reasoning). Add them as fixture files in `internal/proxy/paritytest/`.
- **Why:** The current 17 parity fixtures are hand-written. They prove the new code matches the old code. They do not prove the proxy matches real upstream behavior. The "old code" baseline is itself a translation layer; the actual contract is "what the upstream binary produces."
- **Effort:** half a session.
- **Success criteria:** Three real-stream fixtures exist, each asserts the response renders correctly for an OpenAI client. The parity harness accepts fixture files (currently fixtures are inline strings; this is also a small harness improvement).
- **Depends on:** the user actually running a few real upstream calls and saving the streams. The harness is ready for this; the inputs are not.

### 1.2 Request ID end-to-end

- **What:** Generate a `X-Request-Id` on the inbound HTTP request (or honor one from the client, including the OpenAI `client_request_id` field), thread it through `BuildCCRequest` → upstream call → assembler → log lines.
- **Why:** The current logs are timestamped but not request-correlated. When the proxy returns a wrong answer or hangs, the only way to find the matching upstream call is to read timestamps. A single ID threaded through the whole lifecycle turns "what happened to this request" from a 5-minute grep into a 5-second grep.
- **Effort:** 2–3 hours.
- **Success criteria:** Every log line in the request lifecycle carries the same request ID. The ID appears in the upstream `x-request-id` (or equivalent) header. `TestHandleChatCompletions_*` tests assert the ID propagates.
- **Depends on:** nothing.

### 1.3 Surface upstream streaming errors to the client

- **What:** When the assembler sees `EventError`, emit a final OpenAI-shaped error chunk, set a recognizable `finish_reason`, and exit cleanly. Currently the error is logged and the stream pretends nothing happened.
- **Why:** A client in the middle of a tool loop has no way to know the stream failed; it just gets a clean `[DONE]` with whatever content was streamed before the error. The next time that happens it will look like a hang. This is the kind of bug that's invisible until it isn't.
- **Effort:** ~1 hour.
- **Success criteria:** A new parity fixture (`upstream_stream_error`) proves the client receives an error chunk + `[DONE]` and no further chunks. `assembler_test.go` covers the non-streaming path.
- **Depends on:** the new event handling in the assembler already has a place for this — just a new case in `handle()`.

### 1.4 `-working-dir` flag ✅

- **What:** Add a CLI flag that overrides `config.workingDir` and `x-project-slug` in the upstream request body.
- **Why:** The morning handoff notes call this out as a TODO. Anyone running the proxy as a service (systemd, Docker, supervisord) has no good way to set these without running from a specific directory, which is fragile.
- **Effort:** 15 minutes.
- **Success criteria:** `cmd-code-proxy -working-dir /srv/proxy` overrides both fields. The default behavior is unchanged.
- **Depends on:** nothing.

## Phase 2 — operational maturity (next month or two)

The goal of this phase is to make the proxy pleasant to *operate* (deploy, debug, evolve) over years, not just functional in the moment.

### 2.1 Test the test (deliberate regression bait)

- **What:** Intentionally introduce a known-wrong behavior in the assembler (e.g. `finish_reason: "WRONG"`, or reverse the args concatenation), run the parity test, confirm it fails with a useful message, then revert.
- **Why:** The parity test was built once. Its continued ability to catch the failure modes it was designed for is unverified. A safety net that doesn't fire is worse than no safety net because it gives false confidence. This is "tests the test" work.
- **Effort:** half a session.
- **Success criteria:** A regression-bait PR is run locally (not merged), the parity test fails loudly with a clear diff, and the bait is reverted. Document the result in a comment in `paritytest/parity_test.go` so future maintainers know the test was last verified on 2026-06-05 or later.
- **Depends on:** the parity harness being mature enough that the failure mode is recognizable. (It is — I verified it during the refactor.)

### 2.2 Structured logging (slog)

- **What:** Replace `log.Printf` calls with `slog` (stdlib, Go 1.21+). Move from plain strings to key-value pairs.
- **Why:** The current logs are human-readable but machine-hostile. `slog` + `slog.SetDefault(slog.NewJSONHandler(...))` is one line away from being `jq`-able. This is the prerequisite for any meaningful observability work (request IDs become useful only if the surrounding log lines are structured).
- **Effort:** ~1 day.
- **Success criteria:** Every existing `log.Printf` call has been replaced. The default handler can be switched between text and JSON via an env var. `TestHandleChatCompletions_*` tests assert on log output.
- **Depends on:** 1.2 (request IDs) is more useful with structured logging around them.

### 2.3 Make the personal-use boundary visible

- **What:** Add a comment in `main.go` (or a `LIMITATIONS.md` at the repo root) listing the dropped OpenAI request fields, the partial Responses shim, the hand-curated model list, and stating explicitly: "this proxy is for personal use by the maintainer; the following are intentionally not implemented."
- **Why:** Today, the gap between "what's implemented" and "what's documented as not implemented" is one document (AGENTS.md's request-side fidelity table). A reader skimming `main.go` has no signal that this is a personal-use tool with deliberate scope. Making the boundary a comment next to the actual code means anyone (including future bermudi) who lands in the file is told what the project is *not* trying to be.
- **Effort:** 30 minutes.
- **Success criteria:** A comment in `main.go` (or a sibling file) lists the scope decisions. AGENTS.md links to it from the Scope section.
- **Depends on:** nothing.

### 2.4 Forward more OpenAI request fields — only when there's a real need

- **What:** `tool_choice`, `parallel_tool_calls`, `response_format`, `stop`, `top_p`. Implement CommandCode-side equivalents for each as the need arises.
- **Why:** These are real gaps. They are not gaps that matter for current usage. Doing them speculatively produces code that isn't tested against a real use case, and code paths that aren't used are often subtly wrong.
- **Effort:** small per field, ~1–2 hours each.
- **Success criteria:** A request comes in that needs a specific field, the implementation lands with a parity fixture showing it round-trips, the AGENTS.md request-side fidelity table updates to ✓.
- **Depends on:** the need. Until then, this is a placeholder.

## Phase 3 — explicitly deferred

Items I considered and chose *not* to do, with reasons. This section is here so a future contributor doesn't reopen the same debates.

- **Dynamic `/v1/models` from upstream.** The hand-curated list (`fallbackModels` in `proxy.go`) is fine for personal use. The cost of dynamic fetching is a runtime dependency on upstream at startup, plus a cache-invalidation problem. The user knows what models to add when CommandCode ships them. **Don't do this unless multiple users / a containerized deployment materializes.**
- **Full OpenAI Responses coverage.** The shim is partial by design; the chat completions endpoint is the primary surface. The fields I'd need to support (`truncation`, `metadata`, `previous_response_id`, `store`, `user`, the `reasoning` parameter) are non-trivial and tied to the Responses-specific state model. **Don't do this without a real consumer.**
- **Public-package API boundary.** The proxy's internals are exposed to `paritytest` for convenience (the vendored old code calls into the same package). If the proxy were ever imported as a library, this would need cleanup. The work is speculative until that happens. **Don't do this.**
- **Request-body fidelity parity test.** The current request-side tests are per-feature unit tests in `convert_test.go`. A byte-equivalence parity test would require vendoring an old request-builder, which doesn't exist as a separate function today. This is the same discipline as the response-side parity test, applied to a different surface. **Worth doing only if the request side starts evolving (i.e. if Phase 2.4 actually happens).** The pattern from `paritytest/` is reusable.
- **All-models version table at `/v1/models`.** There's no actual consumer for it inside the proxy. The CLI's `model.go` already maps short aliases; that's what callers use. **Don't do this.**

## What this roadmap is not

- **Not a feature backlog.** Items appear here because they pay back in reliability, debuggability, or scope discipline, not because they're features someone might want. The full list of "things one could do" lives in AGENTS.md § Nice-to-haves.
- **Not a contract.** Dates are deliberately absent from Phase 1 items because personal-use work doesn't have deadlines. Phases are ordered by dependency and payoff, not by calendar.
- **Not exhaustive.** If a real bug shows up tomorrow, it jumps the queue regardless of which phase it's in.

## How to revise this

When the next phase starts, copy the next phase's items into the active section and update the date. When a phase completes, move its items to a `## Completed` section at the bottom with the commit hash that finished them. When something is rejected, move it to Phase 3 with the reason.
