# Conflux — Functional Specification

> **Scope:** Pure functional description. No implementation detail. This document defines *what* the system does and the observable contracts any conforming implementation must satisfy.

---

## 1. Purpose

Conflux is a reverse proxy that multiplexes many upstream LLM provider keys behind a small set of client-facing keys.

It sits between AI clients (SDKs, OpenAI-compatible tools) and upstream providers (OpenAI, Anthropic, Gemini, OpenRouter, custom endpoints). Clients authenticate with a Conflux client key; Conflux forwards the request to the matching upstream using a rotated provider key, optionally via a proxy (HTTP or SOCKS), and returns the upstream response transparently.

### Non-Goals

- Not a general HTTP reverse proxy or API gateway.
- No prompt or message transformation, except optional `model` rewriting via `fallback_models`.
- No per-user accounting or billing.
- No distributed coordination; single-process, single-binary.

### Primary Value

- **Key rotation** prevents single-key exhaustion from blocking traffic.
- **Proxy multiplexing** spreads egress across proxies and survives proxy outages.
- **Error isolation** distinguishes key-specific failures from shared upstream outages so healthy keys are not burned.
- **Observability** exposes key health, proxy health and request metrics.

---

## 2. Glossary

| Term | Meaning |
|------|---------|
| **Provider** | Named upstream configuration (e.g. `openai_prod`). Owns `base_url`, a pool of provider keys, proxy pool, and policy. |
| **Client Key** | Secret presented by the downstream client in a header. Maps 1:1 to a Provider. |
| **Provider Key** | Real upstream API key rotated within a Provider. May carry an inline proxy override. |
| **Default Provider** | Catch-all Provider used when client key is unmapped. Declared either as `name: default` or `client_keys: ["*"]`. At most one may exist. |
| **Active Window** | Top-N healthy provider keys eligible for selection. Standby keys (beyond N) are promoted FIFO on exhaustion. |
| **Exhaustion** | Temporary removal after `max_errors` consecutive failures. Recovers after `cooldown`. |
| **Retirement** | Permanent removal when `retire_on_exhaustion: true`. Never auto-recovers. |
| **Cycle** | One sweep through the entire active window in round-robin order. Used only for proxy rotation accounting. |
| **Proxy** | HTTP or SOCKS endpoint used as egress for upstream requests. |

---

## 3. System Overview

### 3.1 Components

- **Listener** — HTTP server on `server.port`. All `/*` beyond reserved paths is proxied.
- **Router** — Extracts client key, matches Provider, selects `(provider key, proxy, base_url)`.
- **Forwarder** — Rewrites headers/body/URL, issues upstream request with TTFB timeout, optionally via proxy, classifies response.
- **Health Subsystems** — Per-key error counters + per-proxy circuit breakers.
- **Observability** — Trace logs on disk, Prometheus metrics, status JSON.
- **Persistence** — State file for exhaustion/retirement across restarts.
- **Admin** — Config reload and health endpoints.

### 3.2 Request Lifecycle

```
Client Request
  → extract client key (header only)
  → match Provider (or default → 401 if absent)
  → enforce per-client rate limit (429 if exceeded)
  → select (provider key, proxy, base_url) from active window
  → rewrite headers / URL / optional model fallback
  → attempt upstream fetch (TTFB bounded)
      on network/proxy error → record proxy error, retry with next key (do not penalize key)
      on HTTP >=400 → classify
          penalize?  → increment key error counter (may exhaust/retire)
          retry?     → next attempt with next key (bounded budget)
          otherwise  → forward error to client
      on 2xx → record success (reset key error counter)
  → forward response
      JSON → single response
      SSE  → first-chunk peek (retry on error) then passthrough with keepalive + idle watchdog
  → emit trace / metrics / headers
```

Retry is bounded by `retry.max_attempts_per_request` and `max_stream_retries`. See §8 and §9.

---

## 4. Configuration Model (YAML, Strict)

### 4.1 Principles

- **Single canonical name per field.** Unknown keys cause a startup error.
- **Single inheritance level.** `defaults` → `providers.<name>` only. No top-level shadow keys, no environment shadowing except `CONFIG_PATH` (overrides config file path). Environment is not a config source.
- **Explicit units.** Durations are strings with units (`500ms`, `30s`, `5m`, `2h`). Bare numbers are rejected.
- **Strict types.** Strings, ints, booleans are validated.

### 4.2 File

Default path `config.yaml`, overridable via `CONFIG_PATH` env. Implementation must fail fast if file missing or YAML invalid.

### 4.3 Canonical Schema

```yaml
server:
  port: 24118                    # int, 1-65535, required
  request_timeout: 60s           # duration, TTFB for upstream headers; also stream idle timeout. Required, default 60s
  admin_token: secret            # string, required for POST /admin/reload

logging:
  level: full                    # enum: full | errors_only | off
  max_dirs: 1000                 # int >=0, prune oldest trace/error dirs beyond this

proxies:                         # global egress pool, optional
  urls:                          # string[] of URLs
    - http://proxy1.example.com:8080
    - socks5://proxy2.example.com:1080
  rotate_interval: 20            # int >=1, cycles before proxy shift. Default: no rotation (sticky)
  max_errors: 3                  # int >=1, consecutive proxy errors before circuit open. Default 3
  cooldown: 5m                   # duration, how long tripped proxy is excluded. Default 30s

defaults:                        # baseline for every provider; any field may be overridden per-provider
  key_selection:
    mode: round_robin             # enum: round_robin | sticky
    requests_per_key: 1           # int >=1, only valid with round_robin. Reuses key N times before rotating
  active_window: 5                # int >=1, max healthy keys in rotation; omitted = all keys
  max_errors: 5                   # int >=1, consecutive penalized failures before exhaustion. Default 5
  cooldown: 1h                    # duration, time before exhausted key re-enters pool. Default 5h
  retire_on_exhaustion: false     # bool, default false. True = permanent retirement instead of cooldown
  max_stream_retries: 3           # int >=0, SSE first-chunk retries. Default 3

providers:
  openai_prod:                    # map key is provider name, must be unique, must not contain :
    client_keys: [c1, c2]         # string[], 1+ required unless name==default or client_keys: ["*"]
    base_url: https://api.openai.com   # URL, trailing slash stripped, required
    keys:                         # (provider key) pool, 1+ required
      - key: sk-proj-...          # string, required
        proxy: http://inline:8080 # optional inline egress override (object form)
      - key: sk-proj-...
    proxies:                      # provider-scoped pool overrides global; optional
      - http://provider-proxy:8080
    key_selection:                # overrides defaults
      mode: sticky
    active_window: 4
    max_errors: 3
    cooldown: 30m
    retire_on_exhaustion: true
    max_stream_retries: 2
    request_timeout: 120s         # overrides server.request_timeout for this provider
    rate_limit_rpm: 120           # int >=1, per client_key sliding window (60s). Omitted = unlimited
    fallback_models:              # optional model rewrite map
      claude-3-opus-20240229: claude-3-5-sonnet-20241022
```

#### Field Defaults Summary

| Field | Default |
|-------|---------|
| `server.port` | `4010` |
| `server.request_timeout` | `60s` |
| `logging.level` | `full` |
| `logging.max_dirs` | `1000` |
| `proxies.rotate_interval` | unset (no rotation) |
| `proxies.max_errors` | `3` |
| `proxies.cooldown` | `30s` |
| `defaults.key_selection.mode` | `round_robin` |
| `defaults.max_errors` | `5` |
| `defaults.cooldown` | `5h` |
| `defaults.retire_on_exhaustion` | `false` |
| `defaults.max_stream_retries` | `3` |
| `provider.base_url` | required (no implicit default) |

### 4.4 Validation Rules

- Unknown top-level or per-provider keys → error.
- `client_keys` must be non-empty strings, duplicates across providers → error.
- `keys[].key` must be non-empty strings.
- `active_window` > `len(keys)` → error (explicit failure; capping is not allowed).
- `requests_per_key` with `mode: sticky` → error.
- `base_url` must be absolute `http(s)://`.
- `proxies.urls` entries must be valid proxy URLs.
- `rate_limit_rpm` and `max_errors` must be >=1 if present.

### 4.5 Providers Section

- Each entry in `providers` is a Provider. Key is logical name used in metrics/status.
- `client_keys: ["*"]` or `name == default` declares the default provider. At most one.
- A Provider with no `client_keys` and not default → startup error.
- All `client_keys` across providers must be unique.

---

## 5. Routing

### 5.1 Client Key Extraction — Header Only

Client key is extracted **only** from headers, in precedence order:

1. `Authorization: Bearer <token>` — case-insensitive `Bearer`, trimmed. `Bearer` with no token → treated as missing. Raw value without `Bearer` prefix is taken as-is if plausible.
2. `x-api-key`
3. `api-key`

No query-string extraction (`?key=`, `?api_key=`). This avoids leaking client secrets into URL logs and traces.

### 5.2 Provider Matching

- If extracted client key matches an entry in any `provider.client_keys` → that Provider.
- Else if `default` provider exists → default.
- Else → `401 Unauthorized` with body `{error: "Unauthorized: Unknown or missing Conflux client API key"}`. Prometheus category `UNAUTHORIZED`.

Empty header value after trimming is treated as missing and follows the default/401 path.

### 5.3 Rate Limiting

Per-Provider, optional `rate_limit_rpm`:

- Sliding window 60s per exact client key value.
- If `requests_in_last_60s >= rpm` → immediate `429 {error: "Too Many Requests: Conflux client rate limit exceeded"}` without consuming a provider key. Category `CLIENT_RATE_LIMIT`.
- In-memory only; not distributed. Recommend LRU bound (e.g. 10k keys) to prevent unbounded growth.

### 5.4 Reserved Paths (Not Proxied)

- `GET /metrics` — Prometheus exposition.
- `GET /_status` — JSON health.
- `POST /admin/reload` — reload config from disk (see §12.2).
- All other paths `/*` are proxied to the matched Provider's `base_url` (see §7).

---

## 6. Key Pool & Selection

### 6.1 Pool Definition

Each Provider owns an ordered list `keys`. Order is significant: `active_window` selects the earliest healthy entries. `keyNumber` exposed to callers is 1-based index in this list.

### 6.2 Active Window (`active_window`)

- If unset or `>= len(keys)` → all healthy keys participate.
- If set to `N` → only the first `N` healthy keys (by declaration order) are eligible each request. Standbys beyond N are promoted FIFO when an active key exhausts or retires. When a previously exhausted key's cooldown expires, the window recomputes and that key re-enters if it falls within the first N healthy slots.
- `sticky` mode also respects `active_window`.

### 6.3 Selection Modes

#### Round-Robin (default)

- Cursor advances by one per request among the active window.
- With `requests_per_key = K`: the same key is reused for K consecutive requests before advancing. Cursor only advances after K uses, wrapping within the active window. If the cursor's key is no longer healthy, it jumps to the next healthy slot and resets counter.

#### Sticky (`mode: sticky`)

- Sticks to the current healthy key indefinitely until a penalized error occurs on that key. On error, it advances to the next healthy key past the current index, wrapping to the start if needed. No `requests_per_key`.

### 6.4 Key Health

State per key:

- `consecutiveErrors: int` — increments on each penalized failure, reset to `0` on success.
- `exhaustedAt: timestamp|null` — set when `consecutiveErrors >= max_errors` and `retire_on_exhaustion == false`. Cleared lazily on next selection if `now - exhaustedAt >= cooldown`.
- `retired: bool`, `retiredAt` — set instead of `exhaustedAt` when `retire_on_exhaustion == true`. Never cleared automatically.

All health is per-Provider, not global. A key value appearing in two providers has independent counters.

### 6.5 Success / Failure Accounting

- `recordSuccess(key)` — resets `consecutiveErrors` to `0` if >0.
- `recordError(key)` — increments; if threshold reached → exhaust or retire; if `mode: sticky`, also advances sticky index.
- `markExhausted(key)` — immediate exhaustion/retirement regardless of counter (used for fatal auth paths).

---

## 7. Forwarding

### 7.1 Upstream URL

```
upstream = base_url_without_trailing_slash + clientPath + ?search
```

Client path normalization: if `base_url` ends with a first path segment equal to the first segment of `clientPath`, that duplicate segment is removed from `clientPath`. This avoids double-prefix 404s (e.g. `base_url https://api.example.com/v1` + client path `/v1/chat/completions` → `/v1/chat/completions`, not `/v1/v1/chat/completions`). The rule generalizes to any first segment.

`search` is forwarded verbatim.

### 7.2 Header & Body Rewriting

**Header substitution:**

- For each request header value, `replaceAll(clientKey, providerKey)` for the matched client key. `clientKey` here is the exact header-extracted value (trimmed).
- If no header contained the client key → set `Authorization: Bearer <providerKey>` (overwriting any existing `Authorization`). Remove `host` header.
- No header value beyond key replacement is mutated.

**Body `model` fallback:**

- If Provider declares `fallback_models: { from: to }` and the JSON body contains `model == from`, rewrite to `to` before forwarding. Only one rewrite per request. Non-JSON bodies are left untouched.

### 7.3 Timeout Model

Two timeouts only:

- `request_timeout` (TTFB) — time to receive response headers. Implemented via abort controller for HTTP proxies and socket timeout for SOCKS. On expiry → network error, proxy circuit recorded, key not penalized.
- `stream_idle` — for SSE responses, same `request_timeout` value reused as inactivity watchdog: if no bytes arrive for `request_timeout`, the stream errors (`TimeoutError`). Each successful `read()` resets the timer.

Server idle timeout (`ReadHeaderTimeout`/`IdleTimeout`) is not a functional config knob beyond `server.request_timeout`.

### 7.4 Response Headers Added

On every proxied response (JSON or SSE), Conflux adds diagnostic headers:

- `x-conflux-key-number: <1-based>` if a provider key was used.
- `x-conflux-proxy-number: <1-based>` if a proxy was used. Absent if direct.

These are additive and do not overwrite upstream headers of same name unless collision (upstream wins is acceptable).

---

## 8. Error Classification & Retry

### 8.1 Classification

Classification inspects HTTP status and JSON body (`error` or `type==error` envelope, plus `metadata`).

| Category | Condition | `penalizeKey` | `retryWithNextKey` | Note |
|----------|-----------|---------------|--------------------|------|
| `SUCCESS` | `200-299` | no | no | — |
| `SHARED_POOL_RATE_LIMITED` | `429` and `limit_source in {upstream_provider_shared_pool, model_shared_capacity}` OR body `limit_source`/`raw`/`message` contains `temporarily rate-limited upstream`/`upstream provider` | **no** | yes | Key not burned; whole pool may be throttled upstream |
| `KEY_AUTH_FATAL` | `401` or `403` or `402` | yes | yes | Counts toward exhaustion/retirement |
| `KEY_RATE_LIMITED` | `429` otherwise | yes | yes | Per-key quota |
| `UPSTREAM_OUTAGE` | `500-599` | no | yes | No key penalty, allows failover |
| `CLIENT_ERROR` | `400-499` not matched above (e.g. `400 invalid params`) | no | **no** | Forward immediately |
| `UNKNOWN_ERROR` | fallback | no | no | — |
| `PROXY_NETWORK_ERROR` | transport error matching `ECONNREFUSED/ETIMEDOUT/ENOTFOUND/connection refused/socket hung up/timeout/SOCKS…/failed to fetch` | no | yes (next proxy, same key not penalized; proxy circuit opened) | Proxy error, not key error |

`402 Payment Required` is grouped with auth fatal (out of credits).

### 8.2 Retry Budget

A single request retries at most `max_attempts_per_request` times where each attempt is a distinct `(key,proxy)` selection excluding `triedKeys`. Default `max_attempts = min(len(active_window), 3)` or explicit `defaults.retry.max_attempts` / provider override. If all healthy keys are exhausted mid-request, immediately return `503 {error: "All API keys for provider 'X' are exhausted…"}`.

On each penalized classification, the tried key's `consecutiveErrors` is incremented before next attempt. Non-penalized categories still retry but do not increment key counters.

### 8.3 Streaming Retry Exception

SSE errors detected during streaming have a separate budget `max_stream_retries` (see §9).

---

## 9. Streaming

### 9.1 Detection

Streaming is inferred from `content-type` containing `text/event-stream`. Non-streaming responses follow the JSON path.

### 9.2 First-Chunk Peek & Retry

- Upstream body is read for the first chunk before any bytes are sent to the client.
- Chunk is decoded as UTF-8 and split on lines; each `data:` line parsed as JSON. If any parsed object has `error` or `type=="error"`, it is considered an SSE error (even though HTTP status is 200).
- On error:
  - `recordError(providerKey)` and write an error trace.
  - Increment `streamRetryCount`; if `< max_stream_retries`, cancel the upstream body and retry whole request with next key (counts toward retry budget, `triedKeys` includes failed key).
  - If budget exhausted → forward the error chunk stream as-is to the client.
- On clean first chunk, a composite stream is built that replays the first chunk then continues reading remainder. Past this point errors are **not** retried (see 9.3).

Empty upstream body (immediate `done`) is forwarded as empty SSE.

### 9.3 Passthrough

After first-chunk approval, Conflux proxies bytes with:

- **SSE keepalive** — if `content-type` is SSE, enqueue `: keepalive\n\n` every `15s` until closed, to keep downstream intermediaries from timing out.
- **Idle watchdog** — `stream_idle = request_timeout`. Reset on each successful `read()`. On expiry, cancel upstream and error the downstream stream with `TimeoutError`.
- **In-stream error detection** — each chunk is decoded and probed; on first detection `recordError(providerKey)` and write error trace, but stream continues (client sees error chunk). This prevents key burn from text containing `Error 401` in completion content.

Trace for streams: one file per trace with `request.json`, `response.stream` (appended per chunk), `response_headers.json` and `meta.json`.

---

## 10. Proxy Subsystem

### 10.1 Pools & Resolution

A Provider's effective proxy pool is resolved in priority:

1. Inline `keys[].proxy` for the selected key → that single proxy is used; no selection.
2. Provider `proxies` list (if non-empty) → round over that pool.
3. Global `proxies.urls` (if non-empty) → round over that pool.
4. Direct (no proxy).

Resolution is recorded via `proxyNumber` (1-based index in the chosen pool) and `proxy` URL for traces.

### 10.2 Assignment & Rotation

When a pool is used, the proxy index is `(keyIndex + shift) % len(healthyPool)` where `shift = floor(cycleCount / rotate_interval)`. `cycleCount` increments each time round-robin wraps. This pins each key to a stable proxy until `rotate_interval` cycles complete, then shifts the mapping by one. Example with 2 keys, 3 proxies, interval 2: cycles 1-2 use shift 0, cycle 3+ uses shift 1.

### 10.3 Health & Circuit Breaker

Per unique proxy URL:

- `consecutiveErrors` increments on each `recordProxyError`.
- When `>= max_errors`, `deadUntil = now + cooldown`; proxy is considered unhealthy until deadline.
- `recordProxySuccess` resets `consecutiveErrors` and clears `deadUntil`.
- `filterHealthyProxies` excludes tripped proxies; if all are tripped, returns original list to avoid total outage (availability over strictness).

Proxy errors are recognized via transport error string inspection: `econnrefused/etimedout/econnreset/enotfound/connection refused/socket hung up/timeout/socks…/failed to fetch`.

- Network/proxy errors **do not** penalize the provider key; they are attributable to egress. Only classification-penalized HTTP statuses do.

---

## 11. Observability

### 11.1 Metrics

**Prometheus** `GET /metrics`:

```
# HELP conflux_uptime_seconds gauge
conflux_uptime_seconds <sec>
# HELP conflux_requests_total counter
conflux_requests_total <n>
# HELP conflux_requests_by_provider counter
conflux_requests_by_provider{provider="<name>",status="<code>"} <n>
# HELP conflux_error_categories_total counter
conflux_error_categories_total{provider="<name>",category="<CATEGORY>"} <n>
# HELP conflux_request_duration_ms summary
conflux_request_duration_ms_sum{provider="<name>"} <ms>
conflux_request_duration_ms_count{provider="<name>"} <n>
# HELP conflux_keys gauge
conflux_keys{provider="<name>",state="active"} <n>
conflux_keys{provider="<name>",state="exhausted"} <n>
conflux_keys{provider="<name>",state="retired"} <n>
# HELP conflux_proxy_healthy gauge (1 healthy, 0 tripped)
conflux_proxy_healthy{proxy="<url>"} 0|1
```

Global counters plus per-provider status codes, categories, durations.

**Status** `GET /_status`:

```json
{
  "ok": true,
  "uptimeSeconds": 1234,
  "proxies": { "http://p:8080": {"healthy": true, "consecutiveErrors": 0, "deadUntil": null, "lastError": null} },
  "metrics": {"totalRequests": 100, "totalErrors": 5, "providers": {}},
  "status": {"universalProxies": [], "providers": {"name": {"baseUrl": "...", "confluxApiKeys": [], "maxConsecutiveErrors": 5, "cooldownMs": 3600000, "keyStrategy": "round_robin", "requestsPerKey": 1, "retireKeys": false, "activeKeys": 5, "keys": [{"masked": "sk-proj…abcd", "proxy": null, "exhausted": false, "exhaustedAt": null, "retired": false, "retiredAt": null, "consecutiveErrors": 0}]}}}
}
```

### 11.2 Tracing

**Levels:**

- `full` — trace every request under `logs/trace/<timestamp>_<id>/` and error under `logs/error/...`.
- `errors_only` — skip successful JSON/stream traces; still write `logs/error` for failures.
- `off` — no disk traces.

**Per-trace layout:**

- `logs/trace/<ts>_<id>/request.json` — `method, url, headers (redacted), body`
- `response.json` + `meta.json` for JSON, or `response.stream` + `response_headers.json` + `meta.json` for SSE
- `logs/error/<ts>_<id>/error.json` — `provider, keyMasked, keyNumber, proxy, proxyNumber, request, response, error, durationMs, timestamp`
- Redaction masks `authorization|x-api-key|api-key|proxy-authorization`: `sk-...` → `sk-proj…abcd`, short tokens → `****` or `tok…12`. Non-sensitive headers are preserved verbatim.
- Retention: background pruning deletes oldest subdirs beyond `logging.max_dirs`. Sorted by name (timestamp-prefixed). Same limit applied to `logs/trace` and `logs/error` individually.

Console logs are timestamped with local ISO `YYYY-MM-DDTHH:MM:SS.mmm±HH:MM`.

---

## 12. Operational Concerns

### 12.1 Persistence

State file (e.g. `state.yaml` or `state.json`) with explicit kind:

```yaml
keys:
  - provider: openai_prod
    key_hash: sha256:…  # masked display; full key never logged but needed for exact match
    consecutiveErrors: 2
    exhaustedAt: 2026-08-25T03:12:01.123Z  # null if not exhausted
  - provider: openai_prod
    key_hash: sha256:…
    retired: true
    retiredAt: 2026-08-25T03:12:01.123Z
    reason: "max_consecutive_errors reached (5/5)"
```

- Atomic write via temp-file + rename. Load on startup restores counters.
- Debounce window (e.g. 1s) or configurable; immediate flush on retirement.
- File is not version-controlled.

### 12.2 Admin & Reload

- **Admin reload** — `POST /admin/reload` with `Authorization: Bearer <admin_token>` (separate from client keys; configured under `server.admin_token`). Handler re-parses `config.yaml`, validates, and atomically swaps provider state preserving health for keys that still exist (surviving keys keep `consecutiveErrors`/`exhaustedAt`/`retired`). New keys start healthy; removed keys are no longer selectable but in-flight requests holding old key references must not panic when calling `recordSuccess`/`recordError`. Signals `SIGHUP` may alias this endpoint as alternative.
- Graceful shutdown on `SIGINT`/`SIGTERM` flushing state, draining streams.
- No polling hot-reload.

### 12.3 Security Notes

- Provider keys must never appear in logs except masked form. Redaction covers traces and error JSON. Prometheus labels never include key material.
- `admin_token` must not be loggable. `_status` masks client keys list and provider keys beyond masked view.
- Config file permissions should be `600`.

---

## 13. Request/Response Contracts

### 13.1 Proxied Request (`/*`)

- Method, path, search, headers, body are forwarded after rewriting per §7.
- Upstream response status, headers, body are forwarded verbatim except added `x-conflux-*` headers and optional body model rewrite pre-forward.
- If Provider pool is empty before any attempt → `503 {error: "All API keys for provider '<name>' are exhausted. Try again later."}`.
- If unknown client key and no default → `401`.

### 13.2 SSE Contract

- Status `200` forwarded even when body is SSE error chunk after retry budget (client must inspect `data: {"error"…}`).
- Downstream sees replayed first chunk + remainder as single stream; keepalives interleave.
- Idle timeout surfaces as stream error (`TimeoutError`), not HTTP status change.

### 13.3 Metrics & Status Contracts

- Prometheus exposition version `0.0.4` content-type.
- `_status` JSON is best-effort; not a stability contract but fields listed in §11.1 are the compatibility set.

### 13.4 Admin Reload

- `POST /admin/reload` → `200 {ok: true, reloaded: true, providers: N, keys: M}` on success, `400 {error: "<validation>"}` on parse/validation failure (state not swapped). Requires bearer `admin_token`; otherwise `401`.

---

## 14. Configuration Examples

### Minimal

```yaml
server:
  port: 24118
  request_timeout: 60s
providers:
  default:
    base_url: https://api.openai.com
    keys:
      - key: sk-proj-...
```

### Production (Full)

```yaml
server:
  port: 24118
  request_timeout: 120s
  admin_token: env:ADMIN_TOKEN  # resolve env at load, not via config file literal

logging:
  level: full
  max_dirs: 1000

proxies:
  urls: [http://proxy1:8080, socks5://proxy2:1080]
  rotate_interval: 20
  max_errors: 3
  cooldown: 5m

defaults:
  key_selection: {mode: round_robin, requests_per_key: 1}
  active_window: 5
  max_errors: 5
  cooldown: 1h
  retire_on_exhaustion: false
  max_stream_retries: 3

providers:
  openai_prod:
    client_keys: [c-prod]
    base_url: https://api.openai.com
    keys: [{key: sk-a}, {key: sk-b, proxy: http://inline:8080}]
    fallback_models: {"claude-3-opus-20240229": claude-3-5-sonnet-20241022}
    rate_limit_rpm: 120
  anthropic_prod:
    client_keys: [c-anth]
    base_url: https://api.anthropic.com
    key_selection: {mode: sticky}
    active_window: 2
```

---

## 15. Compliance Checklist

A conforming implementation must satisfy:

- [ ] `parseConfig` rejects unknown YAML keys, validates `active_window` ≤ `len(keys)`, `requests_per_key` with `sticky` → error.
- [ ] Header-only extraction: `Authorization: Bearer X` > `x-api-key` > `api-key`; query-string keys ignored; `Bearer` alone → missing.
- [ ] Default provider selection for unmapped key works; unknown with no default → 401.
- [ ] `active_window` promotion and cooldown restoration: standby promoted FIFO on exhaustion, exhausted key re-enters after cooldown if within first N healthy.
- [ ] `sticky` vs `round_robin` + `requests_per_key` sequences match expected rotation semantics.
- [ ] Proxy `shift` rotation after N cycles matches `(keyIndex+shift)%len` formula.
- [ ] `SHARED_POOL` 429 does not penalize, `401`/`429` `KEY_*` does penalize, `400` does not retry.
- [ ] SSE first-chunk error retries up to `max_stream_retries` then forwards error chunk.
- [ ] Tripped proxy excluded; all-tripped fallback returns full list (availability over strictness).
- [ ] `POST /admin/reload` preserves `consecutiveErrors`/`exhaustedAt` for surviving keys, new keys start healthy.
- [ ] Redaction masks `Authorization` as `Bearer sk-proj…cdef` and trace dirs pruned beyond `max_dirs`.
- [ ] Prometheus exposition matches metric family names in §11.1.

---

## 16. Appendix — Behavioral Invariants to Preserve

- Provider key order is stable and defines `keyNumber` (1-based) for metrics/headers/traces.
- Success resets `consecutiveErrors` synchronously before next selection.
- Proxy health is global per proxy URL, not per provider.
- URL normalization must not double-prefix the first path segment.
- `fallback_models` rewrites JSON `model` field only, once per request.
- Exhausted key becomes eligible again strictly after `cooldown`; no jitter required.
