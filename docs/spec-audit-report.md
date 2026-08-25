# Conflux Functional Spec — Brutal Audit Report

> **Target:** `docs/functional-spec.md:1-608` (Functional Specification, rev as of 2026-08-25)
> **Stance:** Faulty-by-assumption. `src/` not inspected per instruction.
> **Verdict:** **FAIL — Not implementable as conformance contract.** Internally contradictory, ambiguous where it must be exact, and exact where it should be vague. Any two conforming implementations built from this spec would diverge on status codes, key burn, proxy selection, retry budgets, and stream semantics.
> **Severity scale:** **Critical** = two implementers will disagree on observable behavior / security hole. **Major** = untestable or unspecified contract. **Minor** = drift, typo, style that costs time.

---

## 0. TL;DR for leadership

Spec claims "pure functional description. No implementation detail" (`docs/functional-spec.md:3`) then leaks `ReadHeaderTimeout`, `failed to fetch`, `ECONNREFUSED`, `15s` keepalive, `sha256:` hashes, and `replaceAll` semantics, while leaving the actual conformance surface (classification string matching, retry budgets, proxy math under exhaustion, streaming peek) undefined.

Result: 12 Critical, 18 Major, ~15 Minor issues. Compliance checklist in `docs/functional-spec.md:582-598` is untestable against the prose that precedes it. Fix the spec before fixing code — otherwise you are testing folklore.

Top 5 must-fix before any engineering work:

1. **Config schema is three-way contradictory** — `docs/functional-spec.md:99-176` vs `docs/functional-spec.md:332` phantom field vs `docs/functional-spec.md:546` env DSL.
2. **`active_window` vs `> len(keys)`** — error vs cap contradiction (`docs/functional-spec.md:172` vs `docs/functional-spec.md:232`).
3. **Retry budget does not exist in schema** — `retry.max_attempts_per_request` is referenced but never defined (`docs/functional-spec.md:332`).
4. **Header `replaceAll` is a substring bomb + leak** — `docs/functional-spec.md:283`.
5. **Streaming peek + proxy rotation math collapse under exhaustion/sticky** — `docs/functional-spec.md:339-398`.

---

## 1. Methodology

- Read `docs/functional-spec.md:1-608` line-by-line. No `src/` reads.
- For each section: ask "could two competent engineers produce the same wire behavior?" If no, file issue.
- Cross-check: Canonical Schema (`docs/functional-spec.md:98-147`) vs Field Defaults Summary (`docs/functional-spec.md:149-166`) vs Validation Rules (`docs/functional-spec.md:168-176`) vs Prose (`docs/functional-spec.md:224-608`) vs Examples (`docs/functional-spec.md:526-578`).
- Naming consistency pass: `config` → `status` (`docs/functional-spec.md:430-439`) → `persistence` (`docs/functional-spec.md:469-479`).

---

## 2. Severity Histogram

| Severity | Count | Examples |
|----------|-------|----------|
| **Critical** | 12 | phantom retry config, header substring replacement, proxy-all-tripped fallback defeats breaker, `plausible` undefined, classification string grep as contract |
| **Major** | 18 | `port` required vs default, `active_window` cap vs error, timeout reuse, keepalive vs idle watchdog, status JSON name drift, state file path missing, reload matching undefined |
| **Minor** | 15 | ISO local time, `host` only hop-by-hop removal, `[DONE]` handling, pruning sort, metric HELP gaps, example inline syntax |

---

## 3. Cross-Cutting Failures

### 3.1 "Strict, single canonical name" violated by spec itself
`docs/functional-spec.md:87` promises strictness. Then:
- Global `proxies.urls` (`docs/functional-spec.md:109`) vs provider `proxies: [url]` (`docs/functional-spec.md:135`). Same concept, two shapes. Implementer must branch.
- `server.port` required-comment vs defaults table default (`docs/functional-spec.md:100` vs `docs/functional-spec.md:152`). Is missing port a startup error or `4010`?
- `config` uses `snake_case` (`max_errors`, `cooldown`, `active_window`), `status` uses `camelCase` (`maxConsecutiveErrors`, `cooldownMs`, `baseUrl`, `keyStrategy`, `retireKeys`, `activeKeys`) in `docs/functional-spec.md:438`. Persistence uses `consecutiveErrors`/`exhaustedAt`/`retiredAt`. Three dialects for one domain — breaks "compatibility set" promise `docs/functional-spec.md:517`.

### 3.2 Environment shadowing contradiction
`docs/functional-spec.md:89`: "Environment is not a config source... except `CONFIG_PATH`" and `docs/functional-spec.md:169` unknown keys → startup error.
`docs/functional-spec.md:546`: `admin_token: env:ADMIN_TOKEN  # resolve env at load`. This is either a literal string `env:ADMIN_TOKEN` (fails unknown-key? No, but wrong value) or a DSL meaning "resolve env". If DSL, it is env shadowing. If literal, example is wrong and insecure (token in file). Spec cannot be both strict and magical.

### 3.3 Time, units, and defaults incoherence
`docs/functional-spec.md:90`: "Explicit units. Durations are strings with units (`500ms`, `30s`, `5m`, `2h`). Bare numbers are rejected." Good.
Then `docs/functional-spec.md:112` `rotate_interval: 20` is a bare int (cycles) — consistent but not a duration, so unit rule doesn't apply but reader must infer.
`docs/functional-spec.md:438` `cooldownMs: 3600000` in status — milliseconds as int, contradicting duration-string contract. Which layer converts? Not defined.
Defaults scattered: `proxies.cooldown` default `30s` in `docs/functional-spec.md:114` and `159`, but `defaults.cooldown` default `5h` vs `1h` vs `30m` per provider — easy to miswire and test expects wrong default.

### 3.4 Underspecified string matching as contract
Classification (`docs/functional-spec.md:315-324`) and proxy error detection (`docs/functional-spec.md:324`, `396`) rely on substring lists (`temporarily rate-limited upstream`, `upstream provider`, `ECONNREFUSED/ETIMEDOUT/ENOTFOUND/connection refused/socket hung up/timeout/SOCKS…/failed to fetch`). No case, no JSON path, no regex, no locale. Two implementers will classify the same 429 differently — key burn diverges.

---

## 4. Section-by-Section Teardown

### §1 Purpose `docs/functional-spec.md:7-26`
**Minor.** Non-Goals list is fine but "No prompt or message transformation, except optional `model` rewriting via `fallback_models`" underplays that `fallback_models` *is* prompt-adjacent transformation. Should state "only `model` field" explicitly here, not buried in `docs/functional-spec.md:286`.

### §2 Glossary `docs/functional-spec.md:29-41`
**Major.** `Active Window: Top-N healthy provider keys eligible` (`docs/functional-spec.md:37`) says "Top-N healthy" implying health-ranked. But `docs/functional-spec.md:230` says "first N healthy keys (by declaration order)". Is "top" declaration order or error-rank? Matters for promotion.
`Cycle: One sweep through entire active window in round-robin order. Used only for proxy rotation` (`docs/functional-spec.md:40`) — so `sticky` has no cycles. Then `docs/functional-spec.md:385` shift formula `floor(cycleCount/rotate_interval)` never advances for sticky — proxy never rotates. Intended? Not stated. Glossary must define cycle for sticky or declare no rotation.

### §3 System Overview `docs/functional-spec.md:45-80`
**Major.** Components list (`docs/functional-spec.md:49-55`) says single `/` listener on `server.port` with `/*` proxied beyond reserved paths — but never defines evaluation order: does `GET /metrics` with valid client key still count as provider request? Diagram (`docs/functional-spec.md:59-77`) shows `extract client key → match Provider → rate limit → select key` with no reserved-path bypass. Implementer will rate-limit metrics.
Retry bounded by `retry.max_attempts_per_request` and `max_stream_retries` (`docs/functional-spec.md:79`) — the former doesn't exist in schema. Forward reference to phantom.

### §4 Configuration Model `docs/functional-spec.md:82-184`
**Critical cluster.**

- **Port defaults lie** `docs/functional-spec.md:100` comment `port: 24118 # int, required` vs `docs/functional-spec.md:152` default `4010`. Example minimal `docs/functional-spec.md:530` uses `24118`. So is `24118` the example or the required? Test in `docs/functional-spec.md:585` will fail either way.
- **`request_timeout` required vs default** `docs/functional-spec.md:101` says `# Required, default 60s` — cannot be both. `docs/functional-spec.md:153` says default `60s`. Which is conformance?
- **`proxies` shape split** `docs/functional-spec.md:108-114` (`proxies.urls`, `rotate_interval`, `max_errors`, `cooldown`) vs `docs/functional-spec.md:134-135` (`proxies: [url]`). Strict unknown-key validation (`docs/functional-spec.md:87`) cannot handle both without aliasing, but aliasing is forbidden.
- **Inline proxy type lie** `docs/functional-spec.md:132` `proxy: http://inline:8080 # optional inline egress override (object form)` — value is string URL inside object with `key:`. "Object form" suggests alternative string form not defined.
- **`active_window` validation vs semantics** `docs/functional-spec.md:172` "`active_window` > `len(keys)` → error (capping not allowed)" vs `docs/functional-spec.md:232` "`If unset or >= len(keys) → all healthy keys participate`". At `> len`, one says error, other implies cap. Checklist `docs/functional-spec.md:589` says `active_window ≤ len(keys)` — aligns with validation, contradicting semantics.
- **`requests_per_key` with sticky → error** `docs/functional-spec.md:173` is correct but never tested in checklist beyond "→ error". What about `defaults.requests_per_key=2` with provider `mode: sticky` override — error at merge or at provider?
- **Missing validations**: `rotate_interval >=1`, `cooldown` duration parse, `proxies.urls` entry must be `http(s)://` or `socks5://`, `fallback_models` keys/values non-empty, `rate_limit_rpm` LRU bound 10k mentioned in `docs/functional-spec.md:213` but not in validation. All must be explicit for strict mode.
- **Inheritance incomplete**: `defaults` may override any field per `docs/functional-spec.md:116` but schema only lists `key_selection`, `active_window`, `max_errors`, `cooldown`, `retire_on_exhaustion`, `max_stream_retries`. Per-provider `request_timeout` (`docs/functional-spec.md:143`) and `rate_limit_rpm`, `fallback_models` have no defaults entry. Is inheritance allowed? Example shows per-provider `request_timeout` — so yes, but not in baseline.

### §5 Routing `docs/functional-spec.md:186-221`
**Critical.**

- **Bearer parsing weasel word** `docs/functional-spec.md:193`: "Raw value without `Bearer` prefix is taken as-is if plausible." `plausible` undefined. Length? charset? `Bearer` case-insensitive trimmed, but `Bearer` with no token → missing (`docs/functional-spec.md:193`). What about `Authorization: Bearer    ` (whitespace) or `Authorization: bearer` lower? Must be exact spec, not "plausible".
- **Header case-insensitivity** not stated. `x-api-key` vs `X-Api-Key` — HTTP says case-insensitive, but spec lists lower. Implementers will mismatch.
- **Multiple headers** — if `Authorization: Bearer A` and `x-api-key: B` both present, precedence says Bearer wins. Fine. But header substitution later (`docs/functional-spec.md:283`) says `replaceAll(clientKey, providerKey)` for each header value. Which `clientKey`? The extracted one. So `x-api-key: B` header still forwarded with client key `B` even though Bearer `A` was used for routing — leak of second credential.
- **Rate limiting** `docs/functional-spec.md:208-213`: sliding window 60s per exact client key value, `in-memory only; not distributed. Recommend LRU bound (e.g. 10k keys)` — "recommend" is not a contract. Is 10k required for conformance? Checklist `docs/functional-spec.md:582-598` doesn't cover it, so implementations diverge on memory blow-up vs eviction.
- **Reserved paths** `docs/functional-spec.md:216-220`: "All other paths `/*` are proxied to matched Provider's `base_url` (see §7)" — does `/*` include query? Does reserved check happen before or after client-key extraction? If before, unauthenticated `GET /metrics` leaks metrics. If after, authenticated metrics counted toward rate limit. Must define order and auth for admin/metrics/status.

### §6 Key Pool & Selection `docs/functional-spec.md:224-264`
**Critical.**

- **Health vs declaration order** `docs/functional-spec.md:230`: "first N healthy keys (by declaration order) are eligible" — good, but `docs/functional-spec.md:37` glossary said "Top-N healthy" which could be read as health-ranked. Keep declaration-order definition and fix glossary.
- **`requests_per_key` counter semantics** `docs/functional-spec.md:240-241`: "same key reused K times before advancing. Cursor only advances after K uses, wrapping within active window. If cursor's key is no longer healthy, it jumps to next healthy slot and resets counter." Does the jump consume one of K or reset to 0? If key `A` used once of `K=3` then becomes exhausted, next request uses `B` — is `B` count 1 or 0? Determines rotation sequence. Untestable as written; checklist `docs/functional-spec.md:590` says sequences must match expected rotation but no expected sequence table given.
- **Sticky advance condition** `docs/functional-spec.md:244-245`: "Sticks ... until a penalized error occurs ... On error, it advances to next healthy key past current index". But table `docs/functional-spec.md:318-322` says `SHARED_POOL_RATE_LIMITED` (429, non-penalized) → `retryWithNextKey: yes` and `UPSTREAM_OUTAGE` (500-599, non-penalized) → `retryWithNextKey: yes`. So retry says next key, sticky says don't advance. Both cannot be true. Real bug: sticky will hammer same key on 500 while retry budget tries next key — spec incoherent.
- **Lazy cooldown vs strict** `docs/functional-spec.md:252` lazy clear vs `docs/functional-spec.md:608` strictly after cooldown. Also `docs/functional-spec.md:233` promotion FIFO on exhaustion but recomputation after cooldown: "window recomputes and that key re-enters if it falls within first N healthy slots." What if re-entering key is beyond N due to FIFO order? Does it evict a later healthy standby? Not defined.
- **Per-provider isolation** `docs/functional-spec.md:255` — good, but then `docs/functional-spec.md:605` proxy health global — cross-provider proxy health contradicts isolation expectation; must clarify.
- **`markExhausted` immediate** `docs/functional-spec.md:261` — used for fatal auth paths but no table row says which category triggers `markExhausted` vs `recordError` increment. Only text.

### §7 Forwarding `docs/functional-spec.md:266-306`
**Critical/Major cluster.**

- **URL normalization** `docs/functional-spec.md:273`: "if `base_url` ends with a first path segment equal to first segment of `clientPath`, that duplicate segment is removed ... generalizes to any first segment." Fails on edge cases:
  - `base_url https://api.example.com/v1` + `/v1alpha/foo` → first segment `v1` vs `v1alpha` — not equal, no strip (correct). But spec says "first segment equal" — does it compare raw string before `?search`? What about trailing slash stripped `docs/functional-spec.md:129` — `/v1/` vs `/v1`? Must define segment comparison (split on `/`, exact token, ignore empty).
  - `base_url https://example.com/api/v1` + `/v1/chat` → first segments `api` vs `v1` — no dedup, but impl might dedup `v1` later segment incorrectly. Spec says "any first segment" — ambiguous if it means any segment or first-of-base vs first-of-client.
- **Header substitution bomb** `docs/functional-spec.md:283`: `replaceAll(clientKey, providerKey)` for each header value. If client key is `test` and header `User-Agent: test-agent` contains it, it becomes `sk-proj-...-agent`. Worse, if client key is short, it corrupts. Must be exact header-value match or specific header allowlist (`Authorization`, `x-api-key`, `api-key`), not substring.
  Also: "If no header contained the client key → set `Authorization: Bearer <providerKey>` (overwriting)". So if client used `x-api-key: <clientKey>`, substitution replaces that header's value, and you do NOT set `Authorization`. Upstream then receives `x-api-key: <providerKey>` — but many upstreams expect `Authorization`. Behavior fork untested. Also `host` removed (`docs/functional-spec.md:283`) but no mention of `content-length`, `connection`, `proxy-authorization` hop-by-hop.
- **Body fallback** `docs/functional-spec.md:286-287`: "If Provider declares `fallback_models: { from: to }` and JSON body contains `model == from`, rewrite to `to` before forwarding. Only one rewrite per request. Non-JSON bodies are left untouched." Which `model`? Top-level field only or JSONPath? What about `model` inside `messages`? What about body already streamed/chunked? Invalid JSON — error or passthrough? "Only one rewrite" implies first match wins, but map may have multiple — order?
- **Timeout reuse** `docs/functional-spec.md:292-294`: `request_timeout` is TTFB via abort controller for HTTP proxies and socket timeout for SOCKS. On expiry → network error, proxy circuit recorded, key not penalized. Good, but then `stream_idle` reuses same value as inactivity watchdog. If provider overrides `request_timeout: 120s` (`docs/functional-spec.md:143`), does stream idle also become 120s? Must state. Also server idle timeout (`ReadHeaderTimeout`/`IdleTimeout`) "is not a functional config knob beyond `server.request_timeout`" (`docs/functional-spec.md:296`) — leaks implementation (Go `net/http`) into functional spec.
- **Response headers** `docs/functional-spec.md:303-305`: "On every proxied response ... adds `x-conflux-key-number` / `x-conflux-proxy-number`" then "These are additive and do not overwrite upstream headers of same name unless collision (upstream wins is acceptable)" — "additive and do not overwrite unless collision (upstream wins)" is self-contradictory. Either additive (both) or upstream wins (Conflux suppresses). Pick.

### §8 Error Classification & Retry `docs/functional-spec.md:308-337`
**Critical — the heart of key burn logic is unimplementable.**

- **SHARED_POOL condition** `docs/functional-spec.md:318`: "`429` and `limit_source in {upstream_provider_shared_pool, model_shared_capacity}` OR body `limit_source`/`raw`/`message` contains `temporarily rate-limited upstream`/`upstream provider`". Contains where? Case? Is `limit_source` a field inside `error` object, or inside `metadata`? Envelope described as "`error` or `type==error` envelope, plus `metadata`" (`docs/functional-spec.md:313`) — envelope shape undefined. Two impls will parse different paths.
- **401/403/402 grouping** `docs/functional-spec.md:319` `KEY_AUTH_FATAL | 401 or 403 or 402 | yes | yes` — `402 Payment Required` burning the key is a policy choice; client errors like `400 invalid params` (`docs/functional-spec.md:322`) correctly not penalized, but 402 as auth fatal means a billing hiccup retires keys forever if `retire_on_exhaustion:true`.
- **500-599** `docs/functional-spec.md:321` `UPSTREAM_OUTAGE | 500-599 | no | yes` — retry with next key but no burn. Good for isolation, but interacts with sticky bug above.
- **UNKNOWN** `docs/functional-spec.md:323` `fallback | no | no` — then `docs/functional-spec.md:332` "Non-penalized categories still retry but do not increment" — contradicts table `retryWithNextKey: no` for `UNKNOWN` and `CLIENT_ERROR`. Which prose wins? Checklist `docs/functional-spec.md:592` asserts "`SHARED_POOL` 429 does not penalize, `401`/`429` `KEY_*` does penalize, `400` does not retry" — checklist would fail whichever reading you pick.
- **Phantom retry config** `docs/functional-spec.md:332`: "`max_attempts_per_request` times where ... Default `max_attempts = min(len(active_window), 3)` or explicit `defaults.retry.max_attempts` / provider override." Field `retry.max_attempts` never defined in Canonical Schema `docs/functional-spec.md:98-147`. No defaults, no validation, no example. Also `max_attempts_per_request` vs `max_attempts` vs `defaults.retry.max_attempts` — three names for one knob.
- **Proxy vs key retry ambiguity** `docs/functional-spec.md:68` (Request Lifecycle) "on network/proxy error → record proxy error, retry with next key (do not penalize key)" vs `docs/functional-spec.md:324` `PROXY_NETWORK_ERROR | ... | no | yes (next proxy, same key not penalized; proxy circuit opened)` — first says next key, second says next proxy same key. Checklist `docs/functional-spec.md:594` doesn't cover proxy retries.
- **TriedKeys vs standby** `docs/functional-spec.md:332` "each attempt is distinct `(key,proxy)` selection excluding `triedKeys`" — does `triedKeys` exclude from active window only or from full pool including standby? If `active_window=2` with 10 total keys, retries limited to `min(2,3)=2` attempts, so 8 standby healthy keys never tried before 503. Is that intended? `docs/functional-spec.md:505` 503 message says "All API keys ... are exhausted" — but standby are not exhausted, just not in window. Misleading.
- **503 trigger** `docs/functional-spec.md:332` "If all healthy keys are exhausted mid-request, immediately return `503`" — same as `docs/functional-spec.md:505`. But `docs/functional-spec.md:505` is pre-attempt check, `docs/functional-spec.md:332` mid-request. Duplicate, slightly different wording.

### §9 Streaming `docs/functional-spec.md:339-368`
**Critical.**

- **Detection** `docs/functional-spec.md:344` `content-type` containing `text/event-stream` — what about `text/event-stream; charset=utf-8` (contains, ok) vs uppercase? vs missing header but body is SSE? Must define case-insensitive contains.
- **First-chunk peek** `docs/functional-spec.md:348-354`: "Upstream body is read for first chunk before any bytes are sent to client. Chunk decoded as UTF-8 and split on lines; each `data:` line parsed as JSON. If any parsed object has `error` or `type=="error"`, considered SSE error (even though HTTP 200)." Chunk size undefined. Wait timeout undefined (does `request_timeout` TTFB cover first chunk wait?). `data:` line without space? `data:  {}` vs `data:{}`? `[DONE]` not JSON — ignored? What about split across chunks — `data:` line cut in half? All untestable. False positives: completion text `{"content": "Error 401 in story"}` inside `data:` JSON would have no `error` key so safe, but spec claims `docs/functional-spec.md:364` "prevents key burn from text containing `Error 401` in completion content" without explaining how — because it only checks envelope, not content. But if completion contains `{"error": "oops"}` as content, it *would* burn incorrectly.
- **Retry budgets collision** `docs/functional-spec.md:352` "`recordError` and write error trace. Increment `streamRetryCount`; if `< max_stream_retries`, cancel upstream and retry whole request with next key (counts toward retry budget, `triedKeys` includes failed key)." So one stream retry consumes one `retry.max_attempts` attempt? And `max_stream_retries` is per-request SSE budget (`docs/functional-spec.md:124` default 3) — how do two budgets interact? If `max_stream_retries=3` but `max_attempts=2`, which wins? Checklist `docs/functional-spec.md:593` only mentions `max_stream_retries`.
- **Empty body** `docs/functional-spec.md:356` "Empty upstream body (immediate `done`) is forwarded as empty SSE." Status? `200` with no body is still SSE? Not defined.
- **Passthrough** `docs/functional-spec.md:360-364`: keepalive `: keepalive\n\n` every `15s` (`docs/functional-spec.md:361`) until closed — but if `request_timeout=5s` (`docs/functional-spec.md:60-77`), keepalive never fires before idle watchdog kills stream. Also "if `content-type` is SSE, enqueue ..." — after first-chunk approval, content-type already determined, but what about non-SSE `200` with later SSE? No.
  Idle watchdog `stream_idle = request_timeout` reset on each `read()` (`docs/functional-spec.md:362`) — on expiry, cancel upstream and error downstream with `TimeoutError` (`docs/functional-spec.md:363`). But downstream headers already sent as `200`, so client cannot see status; only connection abort. Contract in `docs/functional-spec.md:510-512` says "Idle timeout surfaces as stream error (`TimeoutError`), not HTTP status change" — acknowledges but provides no client-visible signal spec (close? error chunk?).
  In-stream error detection `docs/functional-spec.md:364` "each chunk is decoded and probed; on first detection `recordError` and write trace, but stream continues (client sees error chunk). This prevents key burn from text containing `Error 401`..." — first detection burn, but if error chunk appears mid-stream, key already used for successful prefix; burning after partial delivery is weird. Also still doesn't prevent false positive.
- **Trace for streams** `docs/functional-spec.md:366` one file per trace with `request.json`, `response.stream` appended per chunk, `response_headers.json`, `meta.json` — but `docs/functional-spec.md:451-452` earlier said layout is `response.json` + `meta.json` for JSON, or `response.stream` + ... for SSE. Duplication, and `response.stream` append semantics (atomic? interleaved keepalives?) not defined.

### §10 Proxy Subsystem `docs/functional-spec.md:370-398`
**Major/Critical.**

- **Pool resolution** `docs/functional-spec.md:374-379` priority: inline → provider proxies → global proxies → direct. `proxyNumber` 1-based index in chosen pool. But if inline single proxy tripped, do you still use it? `docs/functional-spec.md:374` says "that single proxy is used; no selection." Health filter (`docs/functional-spec.md:394`) not applied to inline. So tripped inline still used — contradicts circuit breaker intent.
- **Assignment & rotation** `docs/functional-spec.md:385`: `proxy index is (keyIndex + shift) % len(healthyPool)` where `shift = floor(cycleCount / rotate_interval)`. Example `docs/functional-spec.md:385` with 2 keys, 3 proxies, interval 2: cycles 1-2 shift 0, cycle 3+ shift 1. Problem: `keyIndex` is index within active window, which changes when keys exhaust or window slides — so stable pinning is unstable. Also `cycleCount` increments "each time round-robin wraps" — but round-robin with `requests_per_key=K` wraps after K*N requests, not N. Not defined. Sticky has no cycles, so no rotation ever — likely not intended but spec says cycle is round-robin only.
- **Health & circuit breaker** `docs/functional-spec.md:389-394`: `consecutiveErrors` per proxy URL, `deadUntil = now + cooldown`, `filterHealthyProxies` excludes tripped, "if all are tripped, returns original list to avoid total outage (availability over strictness)". This last clause defeats breaker under total outage — you will hammer dead proxies instead of failing fast or using direct. No metric for this fallback, no config to disable.
- **Duplicate string match definition** `docs/functional-spec.md:396` vs `docs/functional-spec.md:324`: proxy error strings lists differ slightly (`econnreset` vs `ETIMEDOUT`, case). Which canonical? Checklist `docs/functional-spec.md:594` asserts tripped fallback returns full list but doesn't define which strings trip.

### §11 Observability `docs/functional-spec.md:400-460`
**Major.**

- **Metrics** `docs/functional-spec.md:406-426` names: `conflux_uptime_seconds`, `conflux_requests_total`, `conflux_requests_by_provider{provider,status}`, `conflux_error_categories_total`, `conflux_request_duration_ms`, `conflux_keys{state}`, `conflux_proxy_healthy`. Gaps:
  - `conflux_requests_by_provider` status label is upstream HTTP code, but SSE errors are `200` with error chunk — miscounts (`docs/functional-spec.md:510`).
  - `conflux_keys` gauge states `active/exhausted/retired` — where is `standby` (healthy but beyond `active_window`)? Implementer may count standby as active, skewing dashboard.
  - `conflux_proxy_healthy{proxy="<url>"}` label contains raw URL; if URL has `http://user:pass@host` creds, leak via metrics.
  - `conflux_request_duration_ms` is summary — quantiles, buckets not defined.
- **Status JSON** `docs/functional-spec.md:432-439`: example shows `proxies: { "http://p:8080": {"healthy": ...}}` top-level plus `status: {"universalProxies": [], "providers": {"name": {"baseUrl": "...", "confluxApiKeys": [], "maxConsecutiveErrors": 5, "cooldownMs": 3600000, ...}}}` — field names drift from config (`base_url`→`baseUrl`, `client_keys`→`confluxApiKeys`, `max_errors`→`maxConsecutiveErrors`, `cooldown`→`cooldownMs`, `mode`→`keyStrategy`, `retire_on_exhaustion`→`retireKeys`, `active_window`→`activeKeys`, `requests_per_key`→`requestsPerKey`). Spec says `docs/functional-spec.md:517` "_status JSON is best-effort; not a stability contract but fields listed are compatibility set" — contradictory: best-effort but compatibility set.
- **Tracing** `docs/functional-spec.md:442-457`:
  - Levels `full/errors_only/off` in `docs/functional-spec.md:445-447` — `full` traces every request under `logs/trace/<timestamp>_<id>/` and error under `logs/error/...`. Does `full` duplicate errors (once in trace, once in error)? Or only failures get error dir? Prose says trace every and error under `logs/error/...` — suggests duplication, but `errors_only` "skip successful JSON/stream traces; still write `logs/error`" clarifies. Need explicit.
  - Per-trace layout `docs/functional-spec.md:451-452`: `response.json` vs `response.stream` — but status codes? Are traces written before or after retry? Only final attempt or every attempt?
  - Redaction `docs/functional-spec.md:455`: masks `authorization|x-api-key|api-key|proxy-authorization: sk-... → sk-proj…abcd, short tokens → ****` or `tok…12`. Non-sensitive headers preserved verbatim — but `proxy` header or URL creds not covered. Mask thresholds (what is "short"?) undefined. `sk-proj…abcd` reveals 2+4 chars plus length hint.
  - Retention `docs/functional-spec.md:456`: "background pruning deletes oldest subdirs beyond `logging.max_dirs`. Sorted by name (timestamp-prefixed). Same limit applied to `logs/trace` and `logs/error` individually." Interval, concurrency, failure mode not defined. Sorting by name assumes timestamp prefix lexicographic == chronological — true only if format is `YYYYMMDD...`, not if OS reorders. Also `max_dirs=0` (`docs/functional-spec.md:106` allows `>=0`) means zero traces? Or no pruning? Not defined.
- **Console** `docs/functional-spec.md:459` local ISO `YYYY-MM-DDTHH:MM:SS.mmm±HH:MM` — local timezone makes cross-host correlation painful; UTC is standard. Minor but costly.

### §12 Operational Concerns `docs/functional-spec.md:462-496`
**Major.**

- **Persistence** `docs/functional-spec.md:466-483`:
  - File kind ambiguous `state.yaml` or `state.json` with explicit kind — no config knob for path. Where is state file? Not in schema. Implementer guesses `state.yaml` next to config.
  - Example `docs/functional-spec.md:470-478` shows `key_hash: sha256:…` — "masked display; full key never logged but needed for exact match" (`docs/functional-spec.md:471` comment). To match, you hash config keys with SHA256 and compare to stored hashes — but hash algo, hex vs base64, lower/upper not defined. Also `consecutiveErrors` persisted but `keyNumber` stability depends on config order — after reorder, hash matching may preserve health but keyNumber headers change.
  - Atomic write via temp-file + rename (`docs/functional-spec.md:481`) good, but debounce 1s or configurable not in schema — so not testable. "Immediate flush on retirement" vs debounce — race.
  - "File is not version-controlled" — obvious filler.
- **Admin & Reload** `docs/functional-spec.md:486-489`:
  - `POST /admin/reload` with `Authorization: Bearer <admin_token>` — separate from client keys, configured under `server.admin_token`. But what if `admin_token` missing in config? Is endpoint open/404/500? Not defined. Also `admin_token` must not be loggable (`docs/functional-spec.md:494`) but status currently masks `confluxApiKeys` — does it also mask admin token? Not stated.
  - "atomically swaps provider state preserving health for keys that still exist (surviving keys keep `consecutiveErrors`/`exhaustedAt`/`retired`)" (`docs/functional-spec.md:487`) — surviving definition: value match or index match? If key moved from index 2 to 5 after reorder, is it surviving? Health should follow value, but `keyNumber` is index-based, so header will lie after reload.
  - "New keys start healthy; removed keys no longer selectable but in-flight requests holding old key references must not panic when calling `recordSuccess`/`recordError`" (`docs/functional-spec.md:487`) — implies old key objects retained until in-flight drains. Memory lifecycle not in functional spec, but observable via race.
  - `SIGHUP` may alias reload as alternative (`docs/functional-spec.md:487`) — SIGHUP vs SIGINT/SIGTERM (`docs/functional-spec.md:488`) handling conflated. Graceful shutdown "flushing state, draining streams" — drain timeout not defined.
- **Security Notes** `docs/functional-spec.md:492-495` correct intent but incomplete: prometheus labels never include key material — but metrics include provider name which may be sensitive; proxy URL label may leak creds; trace `request.json` `url` may contain secrets in query.

### §13 Request/Response Contracts `docs/functional-spec.md:500-521`
**Major.**

- **Proxied Request** `docs/functional-spec.md:502-506`: "Method, path, search, headers, body are forwarded after rewriting per §7. Upstream response status, headers, body forwarded verbatim except added `x-conflux-*` headers and optional body model rewrite pre-forward." But keepalive (`docs/functional-spec.md:361`) and idle watchdog mutate body stream — not verbatim. Must define.
- **SSE Contract** `docs/functional-spec.md:508-512`: "`200` forwarded even when body is SSE error chunk after retry budget (client must inspect `data: {"error"…}`)" — good to state, but then status metric is 200, so caller cannot distinguish via status. And "Downstream sees replayed first chunk + remainder as single stream; keepalives interleave" — replay semantics (concatenation) must be atomic. "Idle timeout surfaces as stream error (`TimeoutError`), not HTTP status change" — after headers sent, error is TCP reset, not JSON. Client library may hang.
- **Metrics & Status Contracts** `docs/functional-spec.md:514-517`: "Prometheus exposition version `0.0.4` content-type." Good pinpoint, but metric family names may evolve — must version. "_status JSON is best-effort; not a stability contract but fields listed are compatibility set" — again hedged. Pick stable or best-effort.

### §14 Configuration Examples `docs/functional-spec.md:526-578`
**Minor/Major.**

- Minimal `docs/functional-spec.md:530-538` uses `default` provider with no `client_keys` — allowed per `docs/functional-spec.md:182` but then `docs/functional-spec.md:128` validation "must not contain :" for provider name — `default` passes, but what about `client_keys: ["*"]` vs name==default — both declare default, at most one (`docs/functional-spec.md:36`). Example doesn't test `*`.
- Production `docs/functional-spec.md:545` `admin_token: env:ADMIN_TOKEN` — as above, DSL vs literal. Also `proxies.urls: [http://proxy1:8080, socks5://proxy2:1080]` inline flow vs block — valid YAML but different from earlier block style, may confuse strict parser if it checks shape.
- `fallback_models: {"claude-3-opus-20240229": claude-3-5-sonnet-20241022}` — value unquoted, but contains hyphens; still YAML string? Should be quoted.

### §15 Compliance Checklist `docs/functional-spec.md:582-598`
**Major — untestable.**

- Item `docs/functional-spec.md:585` aggregates: "`parseConfig` rejects unknown YAML keys, validates `active_window` ≤ `len(keys)`, `requests_per_key` with `sticky` → error." Good, but missing `rotate_interval`, `cooldown` string validation, proxy URL shape.
- `docs/functional-spec.md:586` "Header-only extraction: `Authorization: Bearer X` > `x-api-key` > `api-key`; query-string keys ignored; `Bearer` alone → missing." Testable if `plausible` removed. With `plausible`, not.
- `docs/functional-spec.md:587` "Default provider selection for unmapped key works; unknown with no default → 401." Testable.
- `docs/functional-spec.md:589` `active_window` promotion and cooldown restoration — depends on lazy vs strict `docs/functional-spec.md:252` vs `608`.
- `docs/functional-spec.md:591` "Proxy `shift` rotation after N cycles matches `(keyIndex+shift)%len` formula." Untestable for sticky (no cycles) and unstable when keys exhaust (keyIndex shifts).
- `docs/functional-spec.md:592` shared-pool vs key-rate-limit vs 400 — hinges on substring grep.
- `docs/functional-spec.md:593` SSE first-chunk retries up to `max_stream_retries` — collides with phantom `max_attempts_per_request`.
- `docs/functional-spec.md:594` all-tripped fallback returns full list — defeats breaker; checklist asserts it must.
- `docs/functional-spec.md:595` reload preserves `consecutiveErrors`/`exhaustedAt` — but matching by what?
- `docs/functional-spec.md:596` Redaction masks — thresholds undefined.
- `docs/functional-spec.md:597` Prometheus exposition matches metric family names — but status JSON names drift.

### §16 Appendix `docs/functional-spec.md:600-608`
**Minor but telling.**

- "Provider key order is stable and defines `keyNumber` (1-based)" (`docs/functional-spec.md:603`) — stable across what? Reload reorder breaks it.
- "Success resets `consecutiveErrors` synchronously before next selection" (`docs/functional-spec.md:604`) — synchronous vs lazy `exhaustedAt` clearing unclear.
- "Proxy health is global per proxy URL, not per provider" (`docs/functional-spec.md:605`) — then why provider `proxies` override global pool? If health global, tripping global `http://p:8080` must evict it from provider pool that overrides — implied but not stated.
- "Exhausted key becomes eligible again strictly after `cooldown`; no jitter required" (`docs/functional-spec.md:608`) — contradicts lazy checked on next selection (`docs/functional-spec.md:252`). No jitter is an impl choice, but "strictly after" without jitter causes thundering herd.

---

## 5. Concrete Contradiction Matrix

| A | B | Clash |
|---|---|-------|
| `docs/functional-spec.md:89` no env shadowing except `CONFIG_PATH` | `docs/functional-spec.md:546` `env:ADMIN_TOKEN` | DSL vs strict |
| `docs/functional-spec.md:172` `>len(keys)` → error | `docs/functional-spec.md:232` `>=len` → all healthy | cap vs error |
| `docs/functional-spec.md:100` required port | `docs/functional-spec.md:152` default 4010 | required vs default |
| `docs/functional-spec.md:332` non-penalized still retry | `docs/functional-spec.md:323` UNKNOWN `retry=no` | table vs prose |
| `docs/functional-spec.md:68` proxy error → next key | `docs/functional-spec.md:324` proxy error → next proxy same key | next key vs next proxy |
| `docs/functional-spec.md:244` sticky until penalized | `docs/functional-spec.md:318-321` shared/outage non-penalized but retry next key | sticky pinned vs retry moves |
| `docs/functional-spec.md:252` lazy cooldown | `docs/functional-spec.md:608` strictly after | lazy vs strict |
| `docs/functional-spec.md:305` additive headers | `docs/functional-spec.md:305` upstream wins on collision | additive vs wins |
| `docs/functional-spec.md:396` proxy strings list A | `docs/functional-spec.md:324` proxy strings list B | two canonicals |
| `docs/functional-spec.md:98-147` schema no `retry.max_attempts` | `docs/functional-spec.md:332` references it | phantom field |
| `docs/functional-spec.md:108-114` `proxies.urls` | `docs/functional-spec.md:134-135` `proxies: []` | shape split |
| `docs/functional-spec.md:87` single canonical name | `docs/functional-spec.md:438` `baseUrl/cooldownMs/...` | name drift across layers |

---

## 6. What Must Be Fixed (Prioritized)

### P0 — Blocks conformance
1. **Define or delete `retry.max_attempts_per_request`.** Add to `defaults`/`provider` schema with defaults (`min(len(active_window),3)`), validation, and its interaction with `max_stream_retries`. Remove phantom. File: `docs/functional-spec.md:98-147` + `332`.
2. **Fix `active_window` semantics.** Choose: `>len` is error *or* capped, not both. Define `healthy vs total` and standby exclusion from retry budget. Align `docs/functional-spec.md:172`, `232`, `589`.
3. **Replace `replaceAll` substring with header-targeted logic.** Allowlist headers, exact-value match, define leak-free fallback. File: `docs/functional-spec.md:283`.
4. **Eliminate `plausible`.** Define Bearer parsing exact: trimmed, case-insensitive `Bearer`, single space, no raw fallback, or formally spec `plausible` (min length, charset). File: `docs/functional-spec.md:193`.
5. **Pin classification.** Provide JSONPaths, case, exact vs contains, and canonical string sets. Reconcile table `retryWithNextKey` with prose. File: `docs/functional-spec.md:313-324`, `332`.
6. **Disambiguate proxy vs key retry.** One sentence: proxy network error → next proxy *same key* (breaker), HTTP retry → next key. Align `docs/functional-spec.md:68` vs `324` vs `396`.
7. **Define streaming peek.** Chunk boundary, first-chunk timeout (reuse `request_timeout`?), `data:` parsing strict, `[DONE]` handling, budget interaction, replay. File: `docs/functional-spec.md:348-366`.

### P1 — High
8. Unify config shapes: `proxies.urls` everywhere, document `request_timeout` inheritance, resolve `port` required vs default, remove `env:` DSL or spec it.
9. Fix sticky: define advance on non-penalized retry or declare sticky does not retry on outage/shared. Align `docs/functional-spec.md:244-245` + `318-322`.
10. Fix proxy rotation: define `cycleCount` for `requests_per_key=K` and for sticky (or declare no rotation for sticky), handle tripped inline proxy, remove or gate all-tripped fallback.
11. Unify naming across `config`/`status`/`persistence`; define state file path, hash algo (e.g. `hex(sha256(key))`), reload matching by value.
12. Define status JSON stability: either stable contract or best-effort, not both. Remove name drift.

### P2 — Medium
13. Define reserved-path evaluation order and auth, LRU bound hard limit, hop-by-hop header removal set, body `model` JSONPath, keepalive vs idle interaction, trace write point (per-attempt vs final), redaction thresholds, pruning interval, drain timeout, metric quantiles, ISO timezone (use UTC).

---

## 7. Suggestions for Spec Rewrite

- Split into **Conformance MUST** vs **Implementation NOTE** — move `ReadHeaderTimeout`, `replaceAll`, `15s` keepalive, `failed to fetch` into NON-NORMATIVE notes or delete.
- Provide **golden tables**: `requests_per_key=2, active_window=3, keys=[A,B,C]` sequence for round-robin and sticky; `2 keys, 3 proxies, rotate_interval=2` proxy assignment table including exhaustion case.
- Provide **error envelope examples** for OpenAI/Anthropic/OpenRouter with exact JSON, and state which field `limit_source` lives in.
- Add **state diagram** for key health (`healthy → exhausted (cooldown) → healthy` and `→ retired`) and proxy breaker.
- Version the spec and add **compatibility set** for `/_status` with semver.

---

## 8. Final Note

This spec is ambitious — key rotation, proxy multiplexing, error isolation, streaming peeks — but ambition without precision is just folklore with a checklist. As written, it cannot adjudicate between two implementations that both claim "conforming" yet burn different keys on the same 429, pin proxies differently, and kill streams at different timeouts.

Fix P0. Then P1. Then run the verification-planning skill against the rewritten spec before touching `src/`.

---

*Generated without inspecting `src/`. All refs are `docs/functional-spec.md:<line>`. If you want this report expanded with exact proposed schema diffs (YAML patch) or golden test vectors, say the word.*
