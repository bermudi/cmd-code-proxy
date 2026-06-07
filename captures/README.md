# Captures

Real NDJSON traffic captured from CommandCode's `/alpha/generate` endpoint (MiniMax M3, 2026-06-06). These are ground-truth records of what the upstream gateway actually sends — not hand-written fixtures.

## Files

| File | Scenario | Events |
|------|----------|--------|
| `chatcmpl-772cee0e…` | Simple text response | `start` → `start-step` → reasoning → text → `finish-step` + `finish` |
| `chatcmpl-beb53500…` | Text response (continuation) | `start` → `start-step` → reasoning → text → `finish-step` + `finish` |
| `chatcmpl-b11cf821…` | Tool call (`bash pwd`) | `start` → `start-step` → reasoning → tool-input-* → tool-call → `finish-step` + `finish` |
| `chatcmpl-df244d08…` | Text response, no reasoning | `start` → `start-step` → text → `finish-step` + `finish` |

All captures include `finish-step`, `finish`, and `provider-metadata`.

## Key observations from real traffic

### 1. `finish-step` vs `finish` — two separate events, different schemas

The gateway emits **both** `finish-step` and `finish`. They carry the same `finishReason` but different usage shapes:

| Field | `finish-step` | `finish` |
|-------|---------------|----------|
| Usage key | `usage` | `totalUsage` |
| `inputTokenDetails` | ✓ present | ✓ present |
| `providerMetadata` | ✓ (gateway routing, cost) | ✗ |
| `response` | ✓ (upstream response headers, model ID) | ✗ |

The proxy currently only handles `"finish"` (translator line 133). It does **not** parse `"finish-step"`. This means:
- The proxy reads usage from `finish.totalUsage` — this works because `finish` fires after `finish-step` with the same numbers.
- If a stream ever sends only `finish-step` without `finish`, usage would be lost.
- `finish-step` also carries `providerMetadata` with real cost data (`gateway.cost`, `gateway.inferenceCost`) and the resolved model — the proxy ignores all of this.

### 2. `inputTokens` includes cached tokens

Confirmed from real data. Example from `chatcmpl-b11cf821…`:

```
inputTokens:    5320   ← total (cached + uncached)
cacheReadTokens: 5234
noCacheTokens:    86
```

`5234 + 86 = 5320`. The proxy's `usageFromEvent()` passes `InputTokens` straight through as `prompt_tokens` without subtracting cache reads. OpenAI's convention is that `prompt_tokens` and `prompt_tokens_details.cached_tokens` are **disjoint**. If a client computes cost from `prompt_tokens` alone, it over-counts by the cache-read amount.

### 3. `rawFinishReason` values observed

| `finishReason` | `rawFinishReason` | Meaning |
|----------------|-------------------|---------|
| `stop` | `end_turn` | Normal completion |
| `tool-calls` | `tool_use` | Model wants to call tools |

Both are already handled by `normalizeFinishReason()` in the assembler.

### 4. `tool-input-start` has a `dynamic` field

```
{"type":"tool-input-start","id":"call_function_llns1sagnafb_1","toolName":"bash","dynamic":false}
```

The proxy's `CCStreamEvent` struct doesn't have a `Dynamic` field. Non-load-bearing (it's a boolean flag, probably from the Vercel AI Gateway), but noted for completeness.

### 5. `reasoning-delta` can carry `providerMetadata`

The Anthropic reasoning-delta events include a signature:

```json
{"type":"reasoning-delta","id":"0","text":"","providerMetadata":{"anthropic":{"signature":"8a5595…"}}}
```

Empty-text reasoning deltas with signatures are a thing. The proxy ignores them (no harm), but they're present in the wild.

### 6. Event ID scheme

IDs are simple string integers for text/reasoning blocks (`"0"`, `"1"`) and `call_function_*` strings for tool calls. The proxy's `toolIndexRegistry` already handles both via the `register()` / `lookup()` pattern.

### 7. `providerMetadata` carries real cost and routing data

Every `finish-step` includes:

```
gateway.cost:           "0.00036684"   ← real USD
gateway.marketCost:     "0.00036684"
gateway.inferenceCost:  "0.00036684"
gateway.routing.resolvedProvider: "minimax"
gateway.routing.canonicalSlug:    "minimax/minimax-m3"
```

This data is currently discarded. If the proxy ever wants to expose per-request cost to the client (e.g. in a custom header or response field), this is where to find it.

### 8. Model used: MiniMax M3 via Anthropic-protocol gateway

All four captures routed to `minimax/minimax-m3` (MiniMax-M3), resolved through Vercel AI Gateway with Anthropic-protocol adapter. The upstream provider returned Anthropic-native fields (`input_tokens`, `cache_read_input_tokens`, `end_turn`, `tool_use`) which the gateway translated into the Vercel AI SDK event shape.

## Known bugs surfaced by these captures

1. **`finish-step` not parsed by translator.** ✅ *Fixed.* The translator now handles both `finish-step` and `finish`, deduplicating so only the first is emitted (upstream sends both with identical data). The `finish-step`'s `usage` key is preferred over `finish`'s `totalUsage` — it arrives first with richer data.

2. **`prompt_tokens` over-counts by `cacheReadInputTokens`.** ✅ *Fixed.* `usageFromEvent()` now subtracts cache reads/writes from `InputTokens` to produce disjoint `prompt_tokens` and `cache_read_tokens`, matching OpenAI convention.

3. **Wrong JSON key for cache tokens in `CCStreamEvent.TotalUsage`.** ✅ *Fixed.* The struct used `json:"cacheReadInputTokens"` but real upstream sends `json:"cachedInputTokens"`. The field silently deserialized as 0 for all real traffic. Both `TotalUsage` and the new `Usage` now use the correct `cachedInputTokens` key.

## Usage in tests

These files are suitable for:
- **ROADMAP Phase 1.1** (real upstream parity fixtures) — load a capture, pipe it through the assembler, assert the OpenAI output matches expectations.
- **Regression tests** — if upstream changes the event schema, diff against these captures.

To add one as a parity fixture, see `MAINTAINING.md` § Parity test mechanics and the existing fixtures in `internal/proxy/paritytest/`.
