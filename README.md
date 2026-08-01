# Conflux

API key rotation proxy for LLM APIs (OpenAI, Anthropic, Gemini, Azure, etc.). Sits between your AI client and the API, automatically rotating through multiple keys so you don't have to manually swap them when a budget runs out.

## Setup

```bash
# Install dependencies
bun install

# Copy and fill in your keys
cp .env.example .env
# Edit .env → paste your API keys comma-separated
# Set UPSTREAM_URL (e.g. https://api.anthropic.com for Anthropic)

# Run
bun run dev
```

## Configuration (`.env`)

| Variable       | Default                    | Description                                                            |
| -------------- | -------------------------- | ---------------------------------------------------------------------- |
| `API_KEYS`     | —                          | Comma-separated API keys (or `key|proxy_url` format per key)           |
| `API_PROXIES`  | —                          | Optional comma-separated proxies matched 1:1 with `API_KEYS`           |
| `UPSTREAM_URL`  | `https://api.openai.com`   | Upstream API base URL (e.g. `https://api.anthropic.com`)               |
| `CLIENT_KEY`   | `dummy`                    | Optional custom key/UUID/SHA256 string to replace in incoming requests  |
| `PORT`         | `4010`                     | Port the proxy listens on                                              |

### Multi-Provider Support

Conflux performs direct, exact string substitution for your client key (UUID, SHA256, `dummy`, or custom `CLIENT_KEY`):
* Wherever the client API key string appears in **any header** or **any query parameter**, Conflux replaces it with the active rotated key from `.env`.
* No pattern matching or hardcoded header names — works out of the box with OpenAI (`Authorization: Bearer`), Anthropic (`x-api-key`), Azure (`api-key`), Google Gemini (`x-goog-api-key` / `?key=`), and any custom API header.

### Per-Key Proxies (Avoid IP Rate Limits)

To prevent providers with IP-based rate limits from throttling all keys together, each key can route through a separate upstream proxy.

You can configure proxies in two ways:

1. **Inline in `API_KEYS`**:
   ```bash
   API_KEYS=sk-key1|http://127.0.0.1:8080,sk-key2|socks5://127.0.0.1:1080,sk-key3
   ```

2. **Using `API_PROXIES`**:
   ```bash
   API_KEYS=sk-key1,sk-key2,sk-key3
   API_PROXIES=http://proxy1:8080,http://proxy2:8080,http://proxy3:8080
   ```

## Usage with Codex

Point Codex at the proxy instead of OpenAI directly:

```bash
# In your Codex config, set the base URL to:
export OPENAI_BASE_URL=http://localhost:4010
# Leave the API key as any dummy value — the proxy injects the real one
export OPENAI_API_KEY=dummy
```

## How it works

1. Receives any request from the client
2. Picks the next available API key (round-robin)
3. Forwards the request to the upstream OpenAI API with the selected key
4. Passes the response back to the client as-is (supports both JSON and streaming/SSE)
5. If a key errors out 5 times in a row (`429` / `402`), it's marked exhausted for 5 hours. A successful request resets its error counter.
6. Every request/response is logged to `./logs/trace/<timestamp>/` for debugging

## Endpoints

- `/_status` — JSON health check showing key states
- Everything else → proxied to upstream

## Logs

Each request creates a directory under `./logs/trace/` containing:

- `request.json` — method, URL, headers, body
- `response.json` — status, headers, body (non-streaming)
- `response.stream` — raw SSE chunks (streaming)
- `meta.json` — timing, key used, streaming flag
