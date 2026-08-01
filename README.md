# Conflux

Multi-provider API key rotation proxy for LLM APIs (OpenAI, Anthropic, Gemini, etc.). Sits between your AI clients and upstream providers, routing requests based on client keys, rotating through provider key pools, and forwarding requests through universal proxies.

## Quick Start

```bash
# Install dependencies
bun install

# Run (loads config.toml by default)
bun run dev

# Build JavaScript bundle to ./dist
bun run build

# Build standalone executable binary to ./dist/conflux
bun run build:compile
```

## Configuration (`config.toml`)

Conflux is configured using `config.toml`. Specify provider sections, base URLs, key pools, and universal proxies:

```toml
# Server Port
port = 4010

# Max consecutive 429/402 errors before an API key is marked exhausted (default: 5)
max_consecutive_errors = 5

# Cooldown duration after key exhaustion (e.g. "5h", "30m", "300s"; default: "5h")
cooldown = "5h"

# Optional: Rotate proxy mapping between keys every N requests (e.g. shift proxy assignment by 1 after 10 requests)
rotate_proxies_interval = 10

# Universal proxies applied round-robin across all provider key requests
proxies = [
  "http://proxy1.example.com:8080",
  "socks5://proxy2.example.com:1080"
]

# Section 1: Route client key "dummy1" to OpenAI
[providers.openai_dev]
conflux_api_key = "dummy1"
base_url = "https://api.openai.com"
api_keys = [
  "sk-proj-dev-key1",
  "sk-proj-dev-key2|http://inline-proxy:8080"
]
rotate_proxies_interval = 10

# Section 2: Route client key "dummy2" to Anthropic
[providers.anthropic_prod]
conflux_api_key = "dummy2"
base_url = "https://api.anthropic.com"
api_keys = [
  "sk-ant-api03-prod1",
  "sk-ant-api03-prod2"
]

# Default section for requests without a specific matching key
[providers.default]
base_url = "https://api.openai.com"
api_keys = [
  "sk-proj-default-key1"
]
```

## How It Works

1. **Client Key Extraction**: Conflux inspects incoming requests for client keys in `Authorization: Bearer <key>`, `x-api-key`, `api-key`, or query parameters (`?key=`).
2. **Provider Selection**: Matches the client key (e.g. `dummy1` or `dummy2`) to the provider section in `config.toml`.
3. **Key Rotation & Proxy Assignment**: Selects the next healthy key in the section's key pool (round-robin) and applies a proxy. If `rotate_proxies_interval` is configured (e.g. `10`), key-to-proxy mapping shifts deterministically every *N* requests (key 1 initially maps to proxy 1, key 2 to proxy 2; after 10 requests, key 1 shifts to proxy 2, etc.).
4. **Key Replacement**: Replaces the client key with the real provider API key across headers and query parameters.
5. **Upstream Request**: Forwards the request to the provider's `base_url`.
6. **Error Tracking & Cooldown**: Automatically tracks 429/402 errors per key. When a key reaches the consecutive error threshold (configurable via `max_consecutive_errors`, default: 5), it enters a cooldown window (configurable via `cooldown`, default: 5 hours). Both options can be configured globally or per-provider pool.
7. **Hot Reloading**: Editing `config.toml` updates configuration live without restarting the server.

## Endpoints

- `GET /_status` — JSON status check displaying universal proxies and real-time provider key states.
- `/*` — Proxied to matching upstream provider base URL.

## Trace Logs

Request and response traces are saved to `./logs/trace/<timestamp>/`:
- `request.json` — Method, URL, headers, body
- `response.json` — Status, headers, body (non-streaming)
- `response.stream` — Raw SSE chunks (streaming)
- `meta.json` — Timing, key used (`keyUsed`), key number (`keyNumber`), active proxy (`proxy`), proxy number (`proxyNumber`), streaming flag, status code

