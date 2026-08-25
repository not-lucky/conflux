# Conflux

**A self-hosted LLM gateway that pools keys, rotates proxies, classifies errors, and streams SSE — all behind one stable endpoint.**

Conflux v3.0 is a single-binary reverse proxy written in Go. You point your
OpenAI/Anthropic-compatible clients at it once; Conflux routes each request to
the right provider, picks a healthy key, picks a healthy proxy, retries on
retriable failures, streams SSE back to the client, and keeps the whole pool's
state durable across restarts.

Clients never see your provider keys or your proxy list. They authenticate to
Conflux with a single client key and call one URL; Conflux handles everything
between that call and the upstream provider.

---

## Table of contents

- [What Conflux does](#what-conflux-does)
- [Feature overview](#feature-overview)
- [Architecture](#architecture)
- [Quick start](#quick-start)
- [Configuration](#configuration)
  - [Full schema reference](#full-schema-reference)
- [HTTP API](#http-api)
- [Client usage](#client-usage)
- [Observability](#observability)
  - [Metrics (`/metrics`)](#metrics-metrics)
  - [Status (`/_status`)](#status-_status)
  - [Request tracing](#request-tracing)
  - [Diagnostic headers](#diagnostic-headers)
  - [Dashboard (`/_dashboard`)](#dashboard-_dashboard)
- [How routing, keys, and proxies work](#how-routing-keys-and-proxies-work)
- [Project structure](#project-structure)
- [Development](#development)
- [Security notes](#security-notes)

---

## What Conflux does

Point many providers and many API keys at Conflux and expose one endpoint:

```
client ──Bearer sk-conflux-...──▶ Conflux ──Bearer sk-openai-...──▶ OpenAI
                                      └──Bearer sk-ant-...──▶ Anthropic
                                      └──via http://proxy-a ──▶ …
```

Conflux:

1. Authenticates the inbound client key against a global set.
2. Extracts the `model` field from the request body (JSON or multipart).
3. Routes to a provider by model id (exact → longest-prefix → catch-all).
4. Selects a healthy key from that provider's pool (active window, round-robin
   or sticky, `requests_per_key`).
5. Resolves a healthy proxy (inline → provider pool → global pool → direct)
   and applies rotation.
6. Rewrites headers (swaps the client key for the provider key, strips
   hop-by-hop headers), rewrites the URL, and optionally rewrites the model
   field for fallbacks.
7. Sends the request upstream via HTTP/HTTPS or SOCKS5, with a per-attempt
   time-to-first-byte (TTFB) deadline and an end-to-end request deadline.
8. Classifies the upstream response and decides: forward, retry on a new key,
   retry through a different proxy, or penalize the key.
9. For SSE: peeks the first chunk to detect an error envelope before streaming,
   applies keepalive comments and an idle watchdog, and retries empty/broken
   streams within a stream-retry budget.
10. Records metrics, writes a (redacted) trace, persists key/proxy state, and
    streams the response back to the client.

All of this is stateful and durable: key exhaustion, key retirement, proxy
circuit-breaker trips, and per-provider 5xx breaker state are restored from a
state file on restart so a restart does not reset your pools.

---

## Feature overview

| Area | What you get |
| --- | --- |
| **Routing** | Exact, longest-prefix, and catch-all model matching with cross-provider overlap validation |
| **Key pools** | Per-provider pools, active window + FIFO standby promotion, round-robin & sticky modes, `requests_per_key`, exhaustion & retirement, lazy cooldown re-entry |
| **Proxies** | Global pool, per-provider pool override, per-key inline override; `http`, `https`, `socks5`, `socks5h`; rotation by cycle; per-URL circuit breaking |
| **Resilience** | Per-provider upstream-5xx circuit breaker, retry loop with anti-drain guard, per-attempt and end-to-end deadlines, TTFB enforcement, stream-retry budget |
| **SSE** | First-chunk error peek, keepalive comments, idle watchdog, in-stream error detection, `[DONE]` handling, partial-line re-injection |
| **Error classification** | Success, Redirect, SharedPoolRateLimited, KeyRateLimited, KeyAuthFatal, KeyBilling, UpstreamOutage, ClientError, ProxyNetworkError, UnknownError — each with penalize/retryable semantics |
| **Fallbacks** | `fallback_models` maps one model id to another, re-serialized into the body |
| **Rate limiting** | Per-client-key 60s sliding window, 10k LRU, idle-first eviction |
| **Persistence** | State file (YAML or JSON), debounced flush, immediate flush on retirement, SHA256 key hashing, cross-restart restore |
| **Metrics** | Prometheus exposition at `/metrics` (counters, gauges, histogram) |
| **Status** | `/_status` JSON: version, uptime, proxy health, key gauges, provider detail |
| **Dashboard** | Built-in HTMX console at `/_dashboard`: live overview, key/proxy/breaker control, model table, trace viewer, and hot config reload |
| **Tracing** | Per-request on-disk traces with redaction and retention pruning; `full`, `errors_only`, `off` |
| **Redaction** | Secrets masked in traces, metrics, and status; proxy URLs stripped of credentials |
| **Auth** | Client key from `Authorization: Bearer` > `x-api-key` > `api-key`; global validation |
| **Diagnostics** | Optional `X-Conflux-*` response headers (provider, model, key #, proxy #, attempts, error) |
| **Config** | Strict YAML (unknown keys rejected), inheritance chain `provider → defaults → builtin`, typed errors, duration parsing |
| **Ops** | Single binary, graceful shutdown on SIGINT/SIGTERM, debounced state flush on shutdown |

---

## Architecture

Conflux is a layered Go application with a deliberate import direction. The
only package that wires concrete adapters together is `internal/app`; every
other package is a leaf or near-leaf with a small interface.

```
cmd/conflux          ── main: flags, config.Load, app.Build, server.Serve
   │
   ▼
internal/app         ── composition root: builds pools, breakers, proxy health,
   │                     forwarder, metrics, tracer, persistence from config.
   │                     Nothing imports app except cmd/conflux.
   │
   ├── internal/server    HTTP lifecycle: auth → body → model → route → rate-limit
   │                       → forward → trace/metrics → response. Reserved paths:
   │                       /metrics, /_status, /v1/models, /models.
   │
   ├── internal/forward   The deep orchestration module. One method: Do(ctx, *Request).
   │                       Retry loop, classification, key & proxy selection,
   │                       rewrite, deadlines, SSE first-chunk retry — all hidden here.
   │                       Imports classify + stream only.
   │
   ├── internal/keypool   Per-provider key selection & health (leaf).
   ├── internal/proxy     Proxy resolution, rotation, per-URL health (near-leaf).
   ├── internal/breaker   Per-provider upstream-5xx circuit breaker (leaf).
   ├── internal/ratelimit Per-client-key sliding window limiter (leaf).
   ├── internal/model     Model-id → provider routing table (leaf, pure).
   ├── internal/classify  Upstream error classification + SSE probe (leaf, pure).
   ├── internal/stream    SSE peek, pipe, keepalive, idle watchdog (near-leaf).
   ├── internal/persist   State file load/save, debounce, atomic write (near-leaf).
   ├── internal/metrics   Prometheus exposition + /_status snapshot (observer).
   ├── internal/trace     On-disk request tracing + retention pruning (observer).
   ├── internal/redact    Secret masking for logs/traces/metrics/status (near-leaf).
   ├── internal/auth      Client-key extraction + global validation (leaf).
   ├── internal/config    Strict YAML load + validation + resolution (leaf).
   └── internal/clock     Clock interface for deterministic tests (leaf).
```

The forwarder stays decoupled from keypool, proxy, and breaker: it talks to
them through the `forward.ProviderHandle` interface, which `app` implements
once per provider at build time (not per request). The HTTP transport is a
`forward.Doer` port with one production adapter (`httpDoer`, supporting HTTP
and SOCKS5, with TTFB and idle watchdog) and one test fake.

---

## Quick start

Requirements: **Go 1.26+** (module declares `go 1.26.4`).

```bash
# 1. Build the binary
go build -o conflux ./cmd/conflux
#   or:  make build

# 2. Create a config.yaml (see "Configuration" below). A sample is included:
cp config.yaml my-config.yaml   # then edit the keys and providers

# 3. Run it
./conflux -config my-config.yaml
#   or:  make run            # builds and runs with config.yaml
#   or:  CONFIG_PATH=my-config.yaml make run

# 4. Point a client at it
curl http://localhost:24118/v1/chat/completions \
  -H "Authorization: Bearer sk-conflux-global-001" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

Conflux logs `conflux v3.0 listening on :24118` on startup and shuts down
gracefully on `SIGINT`/`SIGTERM`, flushing any pending state.

---

## Configuration

Conflux reads a single YAML file (`config.yaml` by default; override with the
`-config` flag or the `CONFIG_PATH` env var). Decoding is **strict**: unknown
keys are rejected at every level, so a typo is a startup error, not a silent
misconfiguration.

Every policy field follows an inheritance chain:

```
providers.<name>.<field>  →  defaults.<field>  →  built-in default
```

The first present value wins, and the fully resolved value is baked into the
`Config` tree at load time — there is **no runtime inheritance lookup**, so
runtime code only ever reads resolved fields.

Here is the bundled `config.yaml` with every section annotated:

```yaml
server:
  port: 24118
  request_timeout: 60s        # per-attempt TTFB deadline
  stream_idle_timeout: 15s   # SSE idle watchdog
  stream_keepalive_interval: 15s
  request_deadline: 180s     # end-to-end deadline across all retries
  expose_diagnostics: true   # emit X-Conflux-* response headers
  admin_token: "admin-secret-change-me"

auth:
  client_keys:               # global gateway keys (any one grants all access)
    - "sk-conflux-global-001"
    - "sk-conflux-global-002"

logging:
  level: full                # full | errors_only | off
  max_dirs: 1000             # retention cap for trace/ and error/ dirs

proxies:                     # GLOBAL egress pool (optional). Empty = direct.
  urls: []
  max_errors: 5
  cooldown: 5m

defaults:                    # policy fields inherited by every provider
  key_selection:
    mode: round_robin        # round_robin | sticky
    requests_per_key: 1
  max_errors: 5
  cooldown: 5h
  retire_on_exhaustion: false
  max_stream_retries: 3
  upstream_5xx_threshold: 5
  upstream_5xx_cooldown: 30s
  request_timeout: 60s
  stream_idle_timeout: 15s
  stream_keepalive_interval: 15s
  request_deadline: 180s

providers:                   # a MAP keyed by provider name
  openai:
    base_url: "https://api.openai.com/v1"
    keys:
      - key: "sk-openai-…001"
      - key: "sk-openai-…002"
        proxy: "http://proxy-a:8080"   # inline per-key proxy override
    models:
      - gpt-4o               # exact match
      - gpt-4o-mini
      - gpt-4*               # prefix match (trailing *)
    # …any policy field can be overridden here…
    key_selection:
      mode: round_robin
      requests_per_key: 1

  anthropic:
    base_url: "https://api.anthropic.com/v1"
    keys:
      - key: "sk-ant-…001"
    models:
      - claude-3-5-sonnet
      - claude-3*
    fallback_models:
      claude-3-5-sonnet: claude-3-5-haiku   # rewrite the model field upstream

  catchall:
    base_url: "https://fallback.example.com/v1"
    keys:
      - key: "sk-fallback-…001"
    models:
      - "*"                  # catch-all: matches any unmatched model

persistence:
  path: "./state.yaml"       # optional; omit/empty to disable. .json also valid.
```

### Full schema reference

#### `server`

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `port` | int | — | Listen port. |
| `request_timeout` | duration | 60s | Per-attempt TTFB deadline (time to first response header). |
| `stream_idle_timeout` | duration | 15s | SSE idle watchdog; 0 disables. |
| `stream_keepalive_interval` | duration | 15s | SSE `: keepalive` comment interval; 0 disables. |
| `request_deadline` | duration | 180s | End-to-end deadline spanning all retry attempts. |
| `expose_diagnostics` | bool | false | When true, inject `X-Conflux-*` response headers. |
| `admin_token` | string | "" | Admin token. Empty ⇒ `/admin/reload` always returns 401. |

#### `auth`

| Field | Type | Notes |
| --- | --- | --- |
| `client_keys` | []string | Global gateway keys. Any one authenticates a client for all providers/models. Per-provider `client_keys` is rejected. |

#### `logging`

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `level` | string | full | `full` (per-request trace dirs + errors), `errors_only` (error dirs only), `off` (nothing). |
| `max_dirs` | int | 1000 | Retention cap; oldest `trace/` and `error/` dirs beyond this are pruned. |

#### `proxies` (global pool)

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `urls` | []string | [] | Global egress proxy URLs. Empty ⇒ direct. |
| `rotate_interval` | int | 0 (sticky) | Rotate the slot→proxy mapping every N provider cycles. 0 = sticky pinning. |
| `max_errors` | int | 5 | Consecutive errors before a proxy URL is tripped. |
| `cooldown` | duration | 5m | How long a tripped proxy stays dead. |

#### `defaults` (shared policy fields)

These fields also appear under each provider and are inherited:

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `key_selection.mode` | string | round_robin | `round_robin` or `sticky`. |
| `key_selection.requests_per_key` | int | 1 | Use a slot N times before advancing (round_robin). |
| `active_window` | int | all keys | Max keys active at once; extras are standby. |
| `max_errors` | int | 5 | Consecutive key errors before exhaust/retire. |
| `cooldown` | duration | 5h | Exhausted key re-entry time. |
| `retire_on_exhaustion` | bool | false | true ⇒ retire permanently instead of cooldown. |
| `max_stream_retries` | int | 3 | SSE error-chunk retry budget. |
| `upstream_5xx_threshold` | int | 5 | Consecutive 5xx before the provider breaker opens. |
| `upstream_5xx_cooldown` | duration | 30s | Breaker open duration. |
| `request_timeout` | duration | 60s | Per-attempt TTFB. |
| `stream_idle_timeout` | duration | 15s | SSE idle watchdog. |
| `stream_keepalive_interval` | duration | 15s | SSE keepalive. |
| `request_deadline` | duration | 180s | End-to-end deadline. |
| `rate_limit_rpm` | int | 0 (unlimited) | Per-client-key requests/min. Provider value overrides default. |
| `retry.max_attempts` | int | computed | Max retry attempts. Default = `min(active_window, 3)`, clamped ≥ 1. |
| `fallback_models` | map | {} | `{from: to}` model id rewrites applied to the body. |

#### `providers` (map, keyed by name)

| Field | Type | Notes |
| --- | --- | --- |
| `base_url` | string | Upstream base URL. Validated. |
| `keys` | []key | Each `key:` may carry an inline `proxy:`. Duplicate keys across the file are rejected. |
| `models` | []string | Exact (`gpt-4o`), prefix (`gpt-4*`), or catch-all (`*`). Cross-provider overlap and multiple catch-alls are validation errors. |
| `proxies` | object | Optional **provider-scoped** pool (object form only; an array is a startup error). Overrides the global pool. Same fields as `proxies` above. |
| *(policy fields)* | — | Any `defaults` policy field can be overridden per provider. |

#### `persistence` (optional)

| Field | Type | Notes |
| --- | --- | --- |
| `path` | string | State file path. Omit or empty ⇒ persistence disabled. `.json` ⇒ JSON, `.yaml`/`.yml` ⇒ YAML. |

**Duration syntax** accepts Go-style values (`30s`, `5m`, `1h`, `180s`) and
plain seconds (`30`). Invalid or non-positive durations are typed startup
errors (`invalid duration`, `duration must be >0`).

---

## HTTP API

Conflux is a **transparent passthrough**: it routes by the request's `model`
field and forwards the client path and body **verbatim** to the matched
provider's native API. It does **not** translate between API formats — the
client sends whichever shape the routed provider expects (OpenAI
`/v1/chat/completions` or `/v1/responses`, Anthropic `/v1/messages`, or any
other provider whose body carries a top-level `model` field).

Reserved paths are intercepted before proxying; everything else is forwarded.

| Method | Path | Purpose |
| --- | --- | --- |
| Any | `/*` (non-reserved) | Proxied to the matched provider. |
| GET | `/v1/models`, `/models` | List exact model ids with their owning provider. |
| GET | `/v1/models/:id`, `/models/:id` | Look up one model (exact, prefix, or catch-all match). |
| GET | `/_status` | JSON snapshot: version, uptime, proxy health, key gauges, provider detail. |
| GET | `/metrics` | Prometheus text exposition. |
| Any | `/_dashboard/*` | Management console (gated by `server.admin_token`); see [Dashboard](#dashboard). |

### `GET /v1/models`

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-4o", "provider": "openai" },
    { "id": "claude-3-5-sonnet", "provider": "anthropic" }
  ]
}
```

Only exact model ids are enumerated (prefix and catch-all patterns contribute
none). Order is provider declaration order, then id.

### `GET /v1/models/:id`

```json
{ "id": "gpt-4o", "provider": "openai" }
```

Returns the provider for any id that matches an exact, prefix, or catch-all
pattern. `404` when unknown and no catch-all exists.

### `GET /_status`

```json
{
  "version": "3.0",
  "ok": true,
  "uptimeSeconds": 1234,
  "proxies": {
    "http://proxy-a:8080": {
      "healthy": true,
      "consecutiveErrors": 0,
      "deadUntil": null,
      "lastError": ""
    }
  },
  "metrics": { "totalRequests": 42, "totalErrors": 3 },
  "status": {
    "globalProxies": ["http://proxy-a:8080"],
    "clientKeys": ["sk-…0001", "sk-…0002"],
    "providers": {
      "openai": {
        "baseUrl": "https://api.openai.com/v1",
        "models": ["gpt-4o", "gpt-4o-mini", "gpt-4*"],
        "maxConsecutiveErrors": 5,
        "cooldownMs": 18000000,
        "keyStrategy": "round_robin",
        "requestsPerKey": 1,
        "retireKeys": false,
        "activeKeys": 2,
        "retryMaxAttempts": 2
      }
    }
  }
}
```

Client keys and proxy URLs are **redacted** in the response (credentials
stripped, key values masked).

### `GET /metrics`

Prometheus exposition. Key series:

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `conflux_uptime_seconds` | gauge | — | Process uptime. |
| `conflux_requests_total` | counter | — | One per downstream response. |
| `conflux_requests_by_provider` | counter | `provider`, `status` | Final HTTP status per provider. |
| `conflux_requests_by_model` | counter | `model`, `provider`, `status` | Final status per model/provider. |
| `conflux_error_categories_total` | counter | `provider`, `category` | One per classified error attempt (retries counted). |
| `conflux_request_duration_ms` | histogram | `provider` | Admission → downstream response end. Buckets: 1…300000 ms + `+Inf`. |
| `conflux_keys` | gauge | `provider`, `state` | `active`/`standby`/`exhausted`/`retired` key counts. |
| `conflux_proxy_healthy` | gauge | `proxy` | 1 healthy, 0 tripped. |

---

## Client usage

Conflux is a format-agnostic passthrough: it forwards the request path and
body to the matched provider unchanged. Authenticate with one of your
`auth.client_keys`; Conflux swaps it for the provider key in `Authorization`,
`x-api-key`, and `api-key` (so both OpenAI Bearer auth and Anthropic
`x-api-key`/`anthropic-version` style work — non-hop-by-hop headers like
`anthropic-version` pass through untouched).

```bash
# OpenAI chat completion — routed to the openai provider via the model field
curl http://localhost:24118/v1/chat/completions \
  -H "Authorization: Bearer sk-conflux-global-001" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}'

# Anthropic messages — routed to the anthropic provider via the model field.
# Note the native Anthropic shape (x-api-key, anthropic-version, /v1/messages)
# is forwarded unchanged; Conflux substitutes your client key with the
# configured sk-ant provider key.
curl http://localhost:24118/v1/messages \
  -H "x-api-key: sk-conflux-global-001" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'

# Streaming (SSE) — same request with stream:true (OpenAI) or stream:true (Anthropic)
curl -N http://localhost:24118/v1/chat/completions \
  -H "Authorization: Bearer sk-conflux-global-001" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# List models (Conflux's synthetic list of exact ids across all providers)
curl http://localhost:24118/v1/models -H "Authorization: Bearer sk-conflux-global-001"
```

Because routing is by the `model` field, a request to `/v1/messages` with
`"model":"claude-3-5-sonnet"` is routed to the `anthropic` provider, while a
request to `/v1/chat/completions` with `"model":"gpt-4o-mini"` is routed to
`openai`. Use the request shape and endpoint that matches the routed
provider's native API; Conflux passes it through.

Client-key extraction precedence: `Authorization: Bearer <token>` → `x-api-key`
→ `api-key` (matched case-insensitively, values trimmed).

Per-client-key rate limiting: if `rate_limit_rpm` is set (per provider, else
defaults), exceeding it returns `429` with `{"error":{"type":"rate_limited",…}}`
and increments a `CLIENT_RATE_LIMIT` metric.

---

## Observability

### Metrics (`/metrics`)

Scrape with Prometheus. Key gauges (`conflux_keys`, `conflux_proxy_healthy`)
are pushed on every state change, so `/metrics` is correct whether or not
`/_status` has been hit.

### Status (`/_status`)

A JSON snapshot for ad-hoc checks and dashboards. Proxy health includes
`deadUntil` (unix ms) and `lastError`; provider detail includes effective
`activeKeys` and `retryMaxAttempts`.

### Request tracing

When `logging.level` is `full`, Conflux writes a per-request directory under
`./logs/trace/<timestamp>_<id>/` containing:

- `request.json` — method, redacted URL, redacted headers, body (truncated to
  64 KiB), model, provider.
- `response_headers.json` — upstream response headers.
- `response.json` — buffered JSON response body, **or**
- `response.stream` — the SSE bytes tee'd from the stream as they flow to the
  client.
- `meta.json` — provider, model, key #, proxy, proxy #, duration ms, category,
  attempt count.

Failures additionally write `./logs/error/<timestamp>_<id>/error.json`
(written at `full` **and** `errors_only`). Retention is capped by
`logging.max_dirs`; the oldest dirs are pruned.

All secrets are masked: `Authorization`/`x-api-key`/`api-key`/
`proxy-authorization` headers are redacted, and `?key=`/`?api_key=`/`?token=`
query params are replaced with `****`. Proxy URLs have credentials stripped.

### Diagnostic headers

When `server.expose_diagnostics` is true, successful responses carry:

| Header | Meaning |
| --- | --- |
| `X-Conflux-Provider` | Matched provider name. |
| `X-Conflux-Model` | Effective (post-fallback) model sent upstream. |
| `X-Conflux-Model-Original` | Original model, set only on a fallback rewrite. |
| `X-Conflux-Key` | 1-based key number used. |
| `X-Conflux-Proxy` | 1-based proxy number used (absent if direct). |
| `X-Conflux-Attempt` | Number of attempts taken. |
| `X-Conflux-Error` | Terminal downstream error message (when set). |

### Dashboard (`/_dashboard`)

Conflux ships a built-in management console at `/_dashboard/`, rendered with
[HTMX](https://htmx.org) and Go's `html/template` over the live runtime. It is
a single embedded page (no build step, no external CDN — CSS, JS, and a
vendored `htmx.min.js` are served from the binary) for operating the gateway
without a separate process.

The dashboard is **disabled** when `server.admin_token` is empty; set it to a
secret to enable it. Sign in with the admin token; the session is an
`HttpOnly`, `SameSite=Strict` cookie scoped to `/_dashboard`.

It reads live state through the same swappable snapshot the server uses, so a
hot reload is reflected immediately, and writes go straight to the live key
pools, proxy health, and breakers.

| Section | What you can do |
| --- | --- |
| **Overview** | Live uptime, request/error counts, per-provider key-state bars, top error categories. Auto-refreshes every 5s. |
| **Providers** | Resolved per-provider policy (cooldown, retries, breakers, fallbacks, models). |
| **Keys** | Per-provider keys with masked values, state pills, and `reset` / `retire` actions. |
| **Proxies** | Egress pool health with `reset` (re-enable) and `trip` (force-offline) actions. |
| **Breakers** | Per-provider 5xx breakers with `reset` (close) and `force open` actions. |
| **Models** | The routing table: exact ids and prefix/catch-all patterns. |
| **Traces** | Browse on-disk request traces; view `request.json`, `response.json`, `meta.json`, and `response.stream` per request. |

The **Reload config** action re-reads `config.yaml`, rebuilds the key pools,
proxy health, breakers, routing table, forwarder, and validator, and atomically
swaps them in — so a config change (new key, new model, new cooldown) takes
effect without restarting the process. In-memory key exhaustion and proxy trips
carry over across a reload; the metrics registry, tracer, and rate limiter are
preserved so counters and windows are not reset. A reload that fails config
validation is rejected and leaves the old runtime serving.

---

## How routing, keys, and proxies work

**Routing** matches the request's `model` field in this precedence: exact id →
longest matching prefix → catch-all. Cross-provider exact/prefix overlap and
more than one catch-all are rejected at config load time.

**Key selection** runs per attempt within a provider's pool:

- The **active window** is the first N healthy keys by declaration order
  (`active_window`, default all). Keys beyond N are **standby** and promote
  FIFO as active keys exhaust.
- **round_robin** advances through the healthy window, reusing each slot
  `requests_per_key` times; **sticky** pins to one key and only jumps on a
  penalizing error.
- A key **exhausts** after `max_errors` consecutive penalizing errors, then
  re-enters lazily after `cooldown` (or **retires** permanently if
  `retire_on_exhaustion`).
- `KEY_AUTH_FATAL` (401/403) exhausts the key **immediately**. An **anti-drain
  guard** stops retrying once the same auth-fatal condition has already
  exhausted another key, so one bad config does not burn the whole pool.

**Proxy resolution** order per attempt: the selected key's **inline** `proxy`
→ the provider's **pool** → the **global** pool → **direct**. Each level filters
out tripped URLs; if all are tripped it falls through to the next level, and
ultimately to direct. Rotation shifts the slot→proxy mapping every
`rotate_interval` provider cycles.

**The retry loop** is bounded by `retry.max_attempts` (default
`min(active_window, 3)`), the end-to-end `request_deadline`, and (for SSE) the
`max_stream_retries` budget. Penalized retries use a distinct key;
`PROXY_NETWORK_ERROR` retries reuse the same key through a different proxy.

**Classification** turns each upstream outcome into a `Category` with
`Penalize` and `Retryable` flags — e.g. a 429 with a key-specific marker
(`rate_limit_exceeded`, `api key`, `organization`, …) is `KEY_RATE_LIMITED`
(penalize, retry on a new key), while a bare 429 is `SHARED_POOL_RATE_LIMITED`
(no penalty). A 402 is `KEY_BILLING`; 5xx is `UPSTREAM_OUTAGE` (breaker-gated);
a transport/TTFB failure is `PROXY_NETWORK_ERROR` (penalize the proxy, not the
key).

---

## Project structure

```
conflux/
├── cmd/conflux/main.go        # entry point: flags, config.Load, app.Build, server.Serve
├── config.yaml               # sample configuration
├── go.mod / go.sum
├── Makefile
└── internal/
    ├── app/                  # composition root (builds runtime from config)
    ├── server/               # HTTP lifecycle + reserved endpoints
    ├── forward/              # deep orchestration: Do() retry loop, rewrite, SSE
    ├── keypool/              # per-provider key selection & health
    ├── proxy/               # proxy resolution, rotation, per-URL health
    ├── breaker/             # per-provider upstream-5xx circuit breaker
    ├── ratelimit/           # per-client-key sliding window limiter
    ├── model/               # model-id → provider routing table
    ├── classify/            # upstream error classification + SSE probe
    ├── stream/              # SSE peek, pipe, keepalive, idle watchdog
    ├── persist/             # state file load/save, debounce, atomic write
    ├── metrics/             # Prometheus exposition + /_status snapshot
    ├── trace/               # on-disk request tracing + retention pruning
    ├── redact/              # secret masking
    ├── auth/                # client-key extraction + validation
    ├── config/              # strict YAML load + validation + resolution
    ├── clock/               # clock interface for deterministic tests
    ├── runtime/             # swappable live snapshot (Store) for hot reload
    └── dashboard/           # HTMX management console at /_dashboard
    └── version/             # version string (single source of truth)
```

---

## Development

All commands below are wrapped by the `Makefile`; use `make help` to list them.

```bash
make build       # build ./conflux from ./cmd/conflux
make run         # build and run with config.yaml (CONFIG_PATH overrides)
make test        # go test ./...
make test-race   # go test -race ./...
make vet         # go vet ./...
make fmt         # go fmt ./... and gofumpt -w (if installed)
make fmt-check   # fail if any file is not formatted (gofumport if installed)
make lint        # golangci-lint run (if installed) else go vet
make cover       # test with coverage, open HTML report
make tidy        # go mod tidy
make install     # go install ./cmd/conflux
make clean       # remove built artifacts
make bench       # go test -bench=. -benchmem
make help        # list all targets
```

Run the full local check:

```bash
make vet && make test
```

Optional tools Conflux's Makefile will use when present: `gofumpt` (formatting),
`golangci-lint` (linting), `govulncheck` (`make vuln`). When absent, the
relevant target degrades gracefully to the stdlib equivalent.

---

## Security notes

- Conflux authenticates clients with `auth.client_keys`; keep these secret and
  distinct from your provider keys.
- Provider keys are never sent to clients and are masked everywhere they could
  appear (status, traces, logs).
- Proxy URLs are credential-stripped in all observable output.
- The state file (`persistence.path`) stores **SHA256 hashes** of keys, never
  raw key values. Store it with restrictive permissions; Conflux writes it with
  `0600` via an atomic temp-file + rename.
- `server.admin_token` is empty by default, which disables admin endpoints.
  Change it before enabling any admin surface.

Conflux is a self-hosted gateway. Treat your `config.yaml` (which contains raw
provider keys) and state file as secrets.
