/**
 * Integration & Unit tests for Conflux Multi-Provider Proxy.
 *
 * Validates:
 *   1. Section selection by incoming client key (dummy1 -> provider1, dummy2 -> provider2)
 *   2. Base URL and API key substitution per provider section
 *   3. Universal proxy assignment & round-robin rotation
 *   4. Per-provider key error tracking & 5-error exhaustion
 *   5. Fallback to default provider section
 *   6. 401 response for unknown client key when no default is present
 *   7. SSE streaming & trace logging
 *   8. Key extraction from api-key header and query parameters (?key=, ?api_key=)
 *   9. 502 error on upstream fetch failure
 */

import { describe, test, expect, beforeEach, afterAll } from "bun:test";
import { readdir, rm, readFile } from "node:fs/promises";
import { join } from "node:path";
import { parseTomlConfig } from "../src/config";
import {
  pickKeyForClient,
  getKeyStates,
  resetProviderStates,
} from "../src/keys";
import { handleRequest, extractClientKey } from "../src/proxy";

const TRACE_DIR = join(process.cwd(), "logs", "trace");

async function cleanTraces(): Promise<void> {
  await rm(TRACE_DIR, { recursive: true, force: true });
}

type MockHandler = (req: Request) => Response | Promise<Response>;

function startMockUpstream(handler: MockHandler): { server: ReturnType<typeof Bun.serve>; url: string } {
  const server = Bun.serve({
    port: 0,
    fetch: handler,
  });
  return { server, url: `http://localhost:${server.port}` };
}

function waitForTraces(ms = 200): Promise<void> {
  return new Promise((r) => { setTimeout(r, ms); });
}

interface TestResponseBody {
  provider?: string;
  receivedAuth?: string;
  error?: string;
  ok?: boolean;
}

describe("TOML Config Parsing & Multi-Provider Setup", () => {
  test("parses multi-provider TOML section with universal proxies", () => {
    const tomlContent = `
port = 5000
proxies = ["http://univ1:8080", "socks5://univ2:1080"]

[providers.dev]
conflux_api_key = "dummy1"
base_url = "https://api.openai.com"
api_keys = ["sk-dev-1", "sk-dev-2|http://inline-proxy:8080"]

[providers.prod]
conflux_api_key = ["dummy2", "dummy2_alt"]
base_url = "https://api.anthropic.com"
api_keys = ["sk-prod-1"]
`;

    const config = parseTomlConfig(tomlContent);

    expect(config.port).toBe(5000);
    expect(config.universalProxies).toEqual(["http://univ1:8080", "socks5://univ2:1080"]);
    expect(config.providers.size).toBe(2);

    const dev = config.clientKeyMap.get("dummy1");
    expect(dev?.name).toBe("dev");
    expect(dev?.baseUrl).toBe("https://api.openai.com");
    expect(dev?.keys[0]?.key).toBe("sk-dev-1");
    expect(dev?.keys[1]?.proxy).toBe("http://inline-proxy:8080");

    const prod1 = config.clientKeyMap.get("dummy2");
    const prod2 = config.clientKeyMap.get("dummy2_alt");
    expect(prod1?.name).toBe("prod");
    expect(prod2?.name).toBe("prod");
    expect(prod1?.baseUrl).toBe("https://api.anthropic.com");
  });
});

describe("Client Key Extraction", () => {
  test("extracts key from api-key header", () => {
    const req = new Request("http://localhost/v1", {
      headers: { "api-key": "key_from_api_key_header" },
    });
    expect(extractClientKey(req, new URL(req.url))).toBe("key_from_api_key_header");
  });

  test("extracts key from ?key= query param", () => {
    const req = new Request("http://localhost/v1?key=key_from_query");
    expect(extractClientKey(req, new URL(req.url))).toBe("key_from_query");
  });

  test("extracts key from ?api_key= query param", () => {
    const req = new Request("http://localhost/v1?api_key=key_from_api_key_query");
    expect(extractClientKey(req, new URL(req.url))).toBe("key_from_api_key_query");
  });

  test("returns null when no key header or query param is present", () => {
    const req = new Request("http://localhost/v1");
    expect(extractClientKey(req, new URL(req.url))).toBeNull();
  });
});

describe("Client-Key Multi-Provider Routing", () => {
  let mockServer1: ReturnType<typeof Bun.serve> | undefined;
  let mockUrl1: string;
  let mockServer2: ReturnType<typeof Bun.serve> | undefined;
  let mockUrl2: string;

  beforeEach(async () => {
    await cleanTraces();
  });

  afterAll(async () => {
    if (mockServer1) void mockServer1.stop();
    if (mockServer2) void mockServer2.stop();
    await cleanTraces();
  });

  test("routes dummy1 request to provider1 base URL and dummy2 to provider2 base URL", async () => {
    let capturedAuth1 = "";
    let capturedAuth2 = "";

    const m1 = startMockUpstream((req) => {
      capturedAuth1 = req.headers.get("authorization") ?? req.headers.get("x-api-key") ?? "";
      return new Response(JSON.stringify({ provider: "mock1", receivedAuth: capturedAuth1 }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });
    mockServer1 = m1.server;
    mockUrl1 = m1.url;

    const m2 = startMockUpstream((req) => {
      capturedAuth2 = req.headers.get("x-api-key") ?? req.headers.get("authorization") ?? "";
      return new Response(JSON.stringify({ provider: "mock2", receivedAuth: capturedAuth2 }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });
    mockServer2 = m2.server;
    mockUrl2 = m2.url;

    const testToml = `
port = 4010

[providers.provider1]
conflux_api_key = "dummy1"
base_url = "${mockUrl1}"
api_keys = ["sk-p1-key-AAAA", "sk-p1-key-BBBB"]

[providers.provider2]
conflux_api_key = "dummy2"
base_url = "${mockUrl2}"
api_keys = ["sk-p2-key-CCCC"]
`;

    const config = parseTomlConfig(testToml);
    resetProviderStates(config);

    // ── Request 1: using dummy1 key ──
    const req1 = new Request("http://localhost:4010/v1/chat/completions", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "authorization": "Bearer dummy1",
      },
      body: JSON.stringify({ model: "o3" }),
    });

    const res1 = await handleRequest(req1);
    expect(res1.status).toBe(200);
    const body1 = (await res1.json()) as TestResponseBody;
    expect(body1.provider).toBe("mock1");
    expect(capturedAuth1).toBe("Bearer sk-p1-key-AAAA");

    // ── Request 2: using dummy2 key ──
    const req2 = new Request("http://localhost:4010/v1/messages", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-api-key": "dummy2",
      },
      body: JSON.stringify({ model: "claude-3-5-sonnet" }),
    });

    const res2 = await handleRequest(req2);
    expect(res2.status).toBe(200);
    const body2 = (await res2.json()) as TestResponseBody;
    expect(body2.provider).toBe("mock2");
    expect(capturedAuth2).toBe("sk-p2-key-CCCC");

    // ── Request 3: second dummy1 request cycles key in provider1 pool ──
    const req3 = new Request("http://localhost:4010/v1/chat/completions", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "authorization": "Bearer dummy1",
      },
      body: JSON.stringify({ model: "o3" }),
    });

    await handleRequest(req3);
    expect(capturedAuth1).toBe("Bearer sk-p1-key-BBBB");

    void mockServer1.stop();
    void mockServer2.stop();
  });

  test("returns 502 Bad Gateway when upstream request fails to connect", async () => {
    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "http://127.0.0.1:59999"
api_keys = ["sk-key1"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(502);
    const body = (await res.json()) as TestResponseBody;
    expect(body.error).toBe("Upstream request failed");
  });

  test("universal proxies are applied round-robin across provider selections", () => {
    const testToml = `
port = 4010
proxies = ["http://proxy1:8080", "socks5://proxy2:1080"]

[providers.p1]
conflux_api_key = "dummy1"
base_url = "http://localhost:9999"
api_keys = ["sk-k1", "sk-k2|http://inline:8080"]
`;
    const config = parseTomlConfig(testToml);
    resetProviderStates(config);

    // Pick 1: sk-k1 uses universal proxy 1
    const sel1 = pickKeyForClient("dummy1");
    expect(sel1.keyState?.key).toBe("sk-k1");
    expect(sel1.proxy).toBe("http://proxy1:8080");

    // Pick 2: sk-k2 has inline proxy, so inline proxy overrides universal proxy
    const sel2 = pickKeyForClient("dummy1");
    expect(sel2.keyState?.key).toBe("sk-k2");
    expect(sel2.proxy).toBe("http://inline:8080");

    // Pick 3: sk-k1 (wrapped round-robin) uses universal proxy 2
    const sel3 = pickKeyForClient("dummy1");
    expect(sel3.keyState?.key).toBe("sk-k1");
    expect(sel3.proxy).toBe("socks5://proxy2:1080");
  });

  test("returns 401 when client key is unknown and no default provider is set", async () => {
    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "http://localhost:9999"
api_keys = ["sk-k1"]
`;
    const config = parseTomlConfig(testToml);
    resetProviderStates(config);

    const req = new Request("http://localhost:4010/v1/chat/completions", {
      method: "POST",
      headers: {
        "authorization": "Bearer unknown_key_xyz",
      },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(401);
    const body = (await res.json()) as TestResponseBody;
    expect(body.error).toContain("Unknown or missing Conflux client API key");
  });

  test("routes to default provider when client key is unmapped and default provider exists", async () => {
    let capturedAuth = "";
    const { server, url } = startMockUpstream((req) => {
      capturedAuth = req.headers.get("authorization") ?? "";
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.default]
base_url = "${url}"
api_keys = ["sk-default-key"]
`;
    const config = parseTomlConfig(testToml);
    resetProviderStates(config);

    const req = new Request("http://localhost:4010/v1/chat/completions", {
      method: "POST",
      headers: {
        "authorization": "Bearer unknown_client",
      },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(capturedAuth).toBe("Bearer sk-default-key");

    void server.stop();
  });

  test("5 consecutive errors exhaust key per provider pool", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(JSON.stringify({ error: "rate limit" }), {
        status: 429,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-key-error-test"]
`;
    const config = parseTomlConfig(testToml);
    resetProviderStates(config);

    for (let i = 0; i < 5; i++) {
      const req = new Request("http://localhost:4010/v1/chat", {
        method: "POST",
        headers: { "authorization": "Bearer dummy1" },
      });
      await handleRequest(req);
    }

    const states = getKeyStates();
    expect(states.providers.p1?.keys[0]?.exhausted).toBe(true);

    // Subsequent request returns 503 Service Unavailable
    const reqExhausted = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(reqExhausted);
    expect(res.status).toBe(503);

    void server.stop();
  });

  test("forwards SSE streaming response and writes trace log", async () => {
    const chunks = [
      'data: {"type":"response.created"}\n\n',
      'data: {"type":"response.done"}\n\n',
      "data: [DONE]\n\n",
    ];

    const { server, url } = startMockUpstream(() => {
      const stream = new ReadableStream({
        async start(controller) {
          const encoder = new TextEncoder();
          for (const chunk of chunks) {
            controller.enqueue(encoder.encode(chunk));
            await new Promise((r) => { setTimeout(r, 5); });
          }
          controller.close();
        },
      });

      return new Response(stream, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-stream-key"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/responses", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1", "content-type": "application/json" },
      body: JSON.stringify({ stream: true }),
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);

    const reader = res.body?.getReader() as ReadableStreamDefaultReader<Uint8Array> | undefined;
    expect(reader).toBeDefined();
    let collected = "";
    const decoder = new TextDecoder();
    if (reader) {
      while (true) {
        const readResult = await reader.read();
        if (readResult.done) break;
        const val: Uint8Array | undefined = readResult.value;
        if (val) {
          collected += decoder.decode(val, { stream: true });
        }
      }
    }

    expect(collected).toContain("response.created");
    expect(collected).toContain("[DONE]");

    await waitForTraces();
    const entries = await readdir(TRACE_DIR);
    expect(entries.length).toBeGreaterThanOrEqual(1);

    void server.stop();
  });

  test("automatically retries with next key when key 1 encounters 429 rate limit error", async () => {
    const attemptedKeys: string[] = [];

    const { server, url } = startMockUpstream((req) => {
      const auth = req.headers.get("authorization") ?? "";
      attemptedKeys.push(auth);
      if (auth.includes("sk-fail-key1")) {
        return new Response(JSON.stringify({ error: "Rate limit exceeded" }), {
          status: 429,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ ok: true, keyUsed: auth }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-fail-key1", "sk-success-key2"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);

    const body = (await res.json()) as TestResponseBody & { keyUsed?: string };
    expect(body.ok).toBe(true);
    expect(body.keyUsed).toBe("Bearer sk-success-key2");
    expect(attemptedKeys).toEqual(["Bearer sk-fail-key1", "Bearer sk-success-key2"]);

    const states = getKeyStates();
    expect(states.providers.p1?.keys[0]?.consecutiveErrors).toBe(1);
    expect(states.providers.p1?.keys[1]?.consecutiveErrors).toBe(0);

    void server.stop();
  });

  test("does not retry on 400 Bad Request client error", async () => {
    let attemptCount = 0;

    const { server, url } = startMockUpstream(() => {
      attemptCount++;
      return new Response(JSON.stringify({ error: "Invalid model" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-key1", "sk-key2"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(400);
    expect(attemptCount).toBe(1);

    const states = getKeyStates();
    expect(states.providers.p1?.keys[0]?.consecutiveErrors).toBe(0);

    void server.stop();
  });

  test("restarts key rotation pass when all keys fail once but are not yet exhausted", async () => {
    const attemptedKeys: string[] = [];

    const { server, url } = startMockUpstream((req) => {
      const auth = req.headers.get("authorization") ?? "";
      attemptedKeys.push(auth);
      // Fail first attempt for key1 and key2, then succeed on 2nd pass for key1
      if (attemptedKeys.length < 3) {
        return new Response(JSON.stringify({ error: "Temporary error" }), {
          status: 429,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ ok: true, keyUsed: auth }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-k1", "sk-k2"]
max_consecutive_errors = 3
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);

    const body = (await res.json()) as TestResponseBody & { keyUsed?: string };
    expect(body.ok).toBe(true);
    expect(body.keyUsed).toBe("Bearer sk-k1");
    expect(attemptedKeys).toEqual(["Bearer sk-k1", "Bearer sk-k2", "Bearer sk-k1"]);

    const states = getKeyStates();
    expect(states.providers.p1?.keys[0]?.consecutiveErrors).toBe(0);
    expect(states.providers.p1?.keys[1]?.consecutiveErrors).toBe(1);

    void server.stop();
  });

  test("includes x-conflux-key-number and x-conflux-proxy-number headers and records in trace meta.json", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-k1", "sk-k2"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(res.headers.get("x-conflux-key-number")).toBe("1");
    expect(res.headers.get("x-conflux-proxy-number")).toBeNull();

    await waitForTraces();
    const entries = await readdir(TRACE_DIR);
    expect(entries.length).toBeGreaterThanOrEqual(1);

    const firstTrace = entries[0];
    if (firstTrace) {
      const metaRaw = await readFile(join(TRACE_DIR, firstTrace, "meta.json"), "utf-8");
      const meta = JSON.parse(metaRaw) as { keyNumber?: number; proxyNumber?: number };
      expect(meta.keyNumber).toBe(1);
      expect(meta.proxyNumber).toBeNull();
    }

    void server.stop();
  });

  test("immediately exhausts key on 401 Unauthorized status", async () => {
    const { server, url } = startMockUpstream((req) => {
      const auth = req.headers.get("authorization") ?? "";
      if (auth.includes("sk-unauth-key1")) {
        return new Response(JSON.stringify({ error: "Invalid API key" }), {
          status: 401,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const testToml = `
port = 4010

[providers.p1]
conflux_api_key = "dummy1"
base_url = "${url}"
api_keys = ["sk-unauth-key1", "sk-valid-key2"]
`;
    resetProviderStates(parseTomlConfig(testToml));

    const req = new Request("http://localhost:4010/v1/chat", {
      method: "POST",
      headers: { "authorization": "Bearer dummy1" },
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);

    const states = getKeyStates();
    // Key 1 should be exhausted immediately on 1st 401 response
    expect(states.providers.p1?.keys[0]?.exhausted).toBe(true);
    expect(states.providers.p1?.keys[1]?.exhausted).toBe(false);

    void server.stop();
  });
});


