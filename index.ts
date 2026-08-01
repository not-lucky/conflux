/**
 * Conflux — Multi-provider API key rotation proxy
 *
 * Routes incoming client requests to provider target endpoints based on TOML configuration,
 * rotating API keys per provider pool, and dispatching requests through universal proxies.
 */

import { loadConfig } from "./src/config";
import { initKeys, getKeyStates } from "./src/keys";
import { handleRequest } from "./src/proxy";

// ── Load Config ──────────────────────────────────────────────────
let config = await loadConfig();
initKeys(config);

// Watch config file for changes using Bun.file
const configPath = config.configPath;
let lastModified = (await Bun.file(configPath).exists()) ? Bun.file(configPath).lastModified : 0;

setInterval(() => {
  void (async () => {
    try {
      const file = Bun.file(configPath);
      if (await file.exists()) {
        const modified = file.lastModified;
        if (modified > lastModified) {
          lastModified = modified;
          console.log(`\n🔄 Reloading configuration from ${configPath}...`);
          const newConfig = await loadConfig(configPath);
          initKeys(newConfig);
          config = newConfig;
          console.log("✅ Configuration reloaded successfully.");
        }
      }
    } catch (err: unknown) {
      console.error("❌ Error reloading configuration:", err);
    }
  })();
}, 2000);

const PORT = config.port;

// ── Server ───────────────────────────────────────────────────────
Bun.serve({
  port: PORT,
  idleTimeout: 255, // seconds — max Bun allows for long LLM streams

  async fetch(req) {
    const url = new URL(req.url);

    // Health / status endpoint
    if (url.pathname === "/_status") {
      return new Response(
        JSON.stringify({ ok: true, status: getKeyStates() }, null, 2),
        { headers: { "content-type": "application/json" } },
      );
    }

    // Proxy request to upstream
    const start = performance.now();
    const res = await handleRequest(req);
    const ms = (performance.now() - start).toFixed(1);

    const keyNum = res.headers.get("x-conflux-key-number");
    const proxyNum = res.headers.get("x-conflux-proxy-number");
    const keyDetails = keyNum ? ` [Key #${keyNum}, Proxy #${proxyNum ?? "none"}]` : "";

    console.log(`${req.method} ${url.pathname} → ${res.status} (${ms}ms)${keyDetails}`);

    return res;
  },

  error(err) {
    console.error("Unhandled server error:", err);
    return new Response(
      JSON.stringify({ error: "Internal proxy error", detail: String(err) }),
      { status: 500, headers: { "content-type": "application/json" } },
    );
  },
});

console.log(`
┌──────────────────────────────────────────────┐
│  🌀 Conflux Multi-Provider Proxy listening   │
│  Port:    http://localhost:${PORT}             │
│  Status:  http://localhost:${PORT}/_status      │
└──────────────────────────────────────────────┘
`);