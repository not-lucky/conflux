/**
 * Conflux — OpenAI API key rotation proxy
 *
 * Sits between Codex and OpenAI, rotating through multiple API keys
 * so you never have to manually swap them when a budget runs out.
 */

import { loadKeys, getKeyStates } from "./src/keys";
import { handleRequest } from "./src/proxy";

// ── Load config ──────────────────────────────────────────────────
loadKeys();

const PORT = Number(process.env.PORT) || 4010;

// ── Server ───────────────────────────────────────────────────────
const server = Bun.serve({
  port: PORT,
  idleTimeout: 255, // seconds — max Bun allows; LLM streams can be long

  async fetch(req) {
    const url = new URL(req.url);

    // Health / status endpoint
    if (url.pathname === "/_status") {
      return new Response(
        JSON.stringify({ ok: true, keys: getKeyStates() }, null, 2),
        { headers: { "content-type": "application/json" } },
      );
    }

    // Everything else → proxy to upstream
    const start = performance.now();
    const res = await handleRequest(req);
    const ms = (performance.now() - start).toFixed(1);

    console.log(
      `${req.method} ${url.pathname} → ${res.status} (${ms}ms)`,
    );

    return res;
  },

  error(err) {
    console.error("Unhandled server error:", err);
    return new Response(
      JSON.stringify({ error: "Internal proxy error" }),
      { status: 500, headers: { "content-type": "application/json" } },
    );
  },
});

console.log(`
┌──────────────────────────────────────────────┐
│  🌀 Conflux proxy listening on :${PORT}         │
│                                              │
│  Point Codex to: http://localhost:${PORT}       │
│  Status:         http://localhost:${PORT}/_status│
└──────────────────────────────────────────────┘
`);