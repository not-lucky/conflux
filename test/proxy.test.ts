/**
 * Integration tests for the Conflux proxy.
 *
 * Spins up a mock upstream server per test, points the proxy at it,
 * and validates:
 *   1. Non-streaming JSON passthrough
 *   2. Streaming SSE passthrough
 *   3. 402 error code properly retires keys
 *   4. All-keys-exhausted returns 503
 *   5. Trace logs are written to disk
 */

import { describe, test, expect, beforeEach, afterAll } from "bun:test";
import { readFile, readdir, rm } from "node:fs/promises";
import { join } from "node:path";
import { resetKeys, pickKey, markExhausted, getKeyStates, loadKeys } from "../src/keys";
import { handleRequest } from "../src/proxy";

// ── Test keys ────────────────────────────────────────────────────
const TEST_KEYS = ["sk-test-key-AAAA-0001", "sk-test-key-BBBB-0002", "sk-test-key-CCCC-0003"];

// ── Trace dir cleanup ────────────────────────────────────────────
const TRACE_DIR = join(process.cwd(), "logs", "trace");

async function cleanTraces() {
  await rm(TRACE_DIR, { recursive: true, force: true });
}

// ── Mock upstream server ─────────────────────────────────────────
type MockHandler = (req: Request) => Response | Promise<Response>;

function startMockUpstream(handler: MockHandler): { server: ReturnType<typeof Bun.serve>; url: string } {
  const server = Bun.serve({
    port: 0, // random available port
    fetch: handler,
  });
  return { server, url: `http://localhost:${server.port}` };
}

// ── Helpers ──────────────────────────────────────────────────────
function makeRequest(upstreamUrl: string, path: string, body?: object): Request {
  process.env.UPSTREAM_URL = upstreamUrl;
  return new Request(`http://localhost:4010${path}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
}

/** Wait briefly for async trace writes to flush. */
function waitForTraces(ms = 200): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

// ──────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────

describe("Key rotation (unit)", () => {
  beforeEach(() => {
    resetKeys([...TEST_KEYS]);
  });

  test("round-robin cycles through keys in order", () => {
    const k1 = pickKey();
    const k2 = pickKey();
    const k3 = pickKey();
    const k4 = pickKey(); // wraps around

    expect(k1?.key).toBe(TEST_KEYS[0]);
    expect(k2?.key).toBe(TEST_KEYS[1]);
    expect(k3?.key).toBe(TEST_KEYS[2]);
    expect(k4?.key).toBe(TEST_KEYS[0]); // back to first
  });

  test("markExhausted skips that key on next pick", () => {
    const k1 = pickKey();
    expect(k1?.key).toBe(TEST_KEYS[0]);

    markExhausted(TEST_KEYS[1]!); // exhaust key B

    const k2 = pickKey(); // should skip B, return C
    expect(k2?.key).toBe(TEST_KEYS[2]);

    const k3 = pickKey(); // should skip B, return A
    expect(k3?.key).toBe(TEST_KEYS[0]);
  });

  test("returns null when all keys exhausted", () => {
    for (const key of TEST_KEYS) {
      markExhausted(key);
    }
    expect(pickKey()).toBeNull();
  });

  test("getKeyStates reflects exhaustion status", () => {
    markExhausted(TEST_KEYS[0]!);
    const states = getKeyStates();

    expect(states[0]!.exhausted).toBe(true);
    expect(states[0]!.exhaustedAt).not.toBeNull();
    expect(states[1]!.exhausted).toBe(false);
    expect(states[2]!.exhausted).toBe(false);
  });
});

describe("Non-streaming proxy", () => {
  let mockServer: ReturnType<typeof Bun.serve>;
  let mockUrl: string;

  beforeEach(async () => {
    resetKeys([...TEST_KEYS]);
    await cleanTraces();
  });

  afterAll(async () => {
    await cleanTraces();
  });

  test("forwards JSON response with correct body and status", async () => {
    const responsePayload = {
      id: "resp_abc123",
      output: [{ type: "message", content: [{ type: "text", text: "Hello from mock!" }] }],
    };

    const { server, url } = startMockUpstream((req) => {
      return new Response(JSON.stringify(responsePayload), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });
    mockServer = server;
    mockUrl = url;

    const req = makeRequest(mockUrl, "/v1/responses", { model: "o3", input: "test" });
    const res = await handleRequest(req);

    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.id).toBe("resp_abc123");
    expect(body.output[0].content[0].text).toBe("Hello from mock!");

    mockServer.stop();
  });

  test("injects authorization header with selected key", async () => {
    let capturedAuth = "";

    const { server, url } = startMockUpstream((req) => {
      capturedAuth = req.headers.get("authorization") ?? "";
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const req = makeRequest(url, "/v1/responses", { model: "o3", input: "test" });
    await handleRequest(req);

    expect(capturedAuth).toBe(`Bearer ${TEST_KEYS[0]}`);

    mockServer.stop();
  });

  test("injects x-api-key header for Anthropic Messages API requests", async () => {
    let capturedXApiKey = "";
    let capturedVersion = "";

    const { server, url } = startMockUpstream((req) => {
      capturedXApiKey = req.headers.get("x-api-key") ?? "";
      capturedVersion = req.headers.get("anthropic-version") ?? "";
      return new Response(JSON.stringify({ type: "message", content: [] }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    process.env.UPSTREAM_URL = url;
    const req = new Request(`http://localhost:4010/v1/messages`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-api-key": "dummy",
        "anthropic-version": "2023-06-01",
      },
      body: JSON.stringify({ model: "claude-3-5-sonnet-20241022", messages: [] }),
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(capturedXApiKey).toBe(TEST_KEYS[0]!);
    expect(capturedVersion).toBe("2023-06-01");

    server.stop();
  });

  test("substitutes query parameter key for Google Gemini / query-based APIs", async () => {
    let capturedUrl = "";

    const { server, url } = startMockUpstream((req) => {
      capturedUrl = req.url;
      return new Response(JSON.stringify({ candidates: [] }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    process.env.UPSTREAM_URL = url;
    const req = new Request(`http://localhost:4010/v1beta/models/gemini-pro:generateContent?key=dummy`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ contents: [] }),
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(capturedUrl).toContain(`key=${TEST_KEYS[0]}`);

    server.stop();
  });

  test("dynamically scans and replaces any custom auth or dummy-containing header", async () => {
    let capturedCustomAuth = "";
    let capturedSecret = "";

    const { server, url } = startMockUpstream((req) => {
      capturedCustomAuth = req.headers.get("x-custom-auth-token") ?? "";
      capturedSecret = req.headers.get("x-vendor-secret") ?? "";
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    process.env.UPSTREAM_URL = url;
    const req = new Request(`http://localhost:4010/v1/custom-endpoint`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-custom-auth-token": "dummy",
        "x-vendor-secret": "dummy",
      },
      body: JSON.stringify({ test: true }),
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(capturedCustomAuth).toBe(TEST_KEYS[0]!);
    expect(capturedSecret).toBe(TEST_KEYS[0]!);

    server.stop();
  });

  test("replaces unique UUID/SHA256 client keys wherever they appear in headers", async () => {
    const clientUuid = "e7b92f41-0182-4f6c-8a19-94b2a8e7a001";
    process.env.CLIENT_KEY = clientUuid;
    let capturedAuth = "";
    let capturedHeader = "";

    const { server, url } = startMockUpstream((req) => {
      capturedAuth = req.headers.get("authorization") ?? "";
      capturedHeader = req.headers.get("x-client-uuid") ?? "";
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    process.env.UPSTREAM_URL = url;
    const req = new Request(`http://localhost:4010/v1/messages`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "authorization": `Bearer ${clientUuid}`,
        "x-client-uuid": clientUuid,
      },
      body: JSON.stringify({ test: true }),
    });

    const res = await handleRequest(req);
    expect(res.status).toBe(200);
    expect(capturedAuth).toBe(`Bearer ${TEST_KEYS[0]}`);
    expect(capturedHeader).toBe(TEST_KEYS[0]!);

    server.stop();
  });

  test("writes trace files for non-streaming response", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(JSON.stringify({ traced: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    const req = makeRequest(url, "/v1/responses", { model: "o3", input: "log me" });
    await handleRequest(req);

    await waitForTraces();

    // Find the trace directory
    const entries = await readdir(TRACE_DIR);
    expect(entries.length).toBeGreaterThanOrEqual(1);

    const traceDir = join(TRACE_DIR, entries[0]!);

    // Check request.json
    const reqLog = JSON.parse(await readFile(join(traceDir, "request.json"), "utf-8"));
    expect(reqLog.method).toBe("POST");
    expect(reqLog.body.input).toBe("log me");

    // Check response.json
    const resLog = JSON.parse(await readFile(join(traceDir, "response.json"), "utf-8"));
    expect(resLog.status).toBe(200);
    expect(resLog.body.traced).toBe(true);

    // Check meta.json
    const meta = JSON.parse(await readFile(join(traceDir, "meta.json"), "utf-8"));
    expect(meta.streaming).toBe(false);
    expect(meta.status).toBe(200);
    expect(meta.durationMs).toBeGreaterThanOrEqual(0);

    server.stop();
  });
});

describe("Streaming proxy", () => {
  beforeEach(async () => {
    resetKeys([...TEST_KEYS]);
    await cleanTraces();
  });

  afterAll(async () => {
    await cleanTraces();
  });

  test("forwards SSE stream and client receives all chunks", async () => {
    const chunks = [
      'data: {"type":"response.created","response":{"id":"resp_001"}}\n\n',
      'data: {"type":"response.output_item.added","item":{"type":"message"}}\n\n',
      'data: {"type":"response.content_part.delta","delta":{"text":"Hello"}}\n\n',
      'data: {"type":"response.content_part.delta","delta":{"text":" World"}}\n\n',
      "data: [DONE]\n\n",
    ];

    const { server, url } = startMockUpstream(() => {
      const stream = new ReadableStream({
        async start(controller) {
          const encoder = new TextEncoder();
          for (const chunk of chunks) {
            controller.enqueue(encoder.encode(chunk));
            await new Promise((r) => setTimeout(r, 10)); // simulate delay
          }
          controller.close();
        },
      });

      return new Response(stream, {
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
      });
    });

    const req = makeRequest(url, "/v1/responses", { model: "o3", input: "stream test", stream: true });
    const res = await handleRequest(req);

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("text/event-stream");

    // Read all chunks from the proxied stream
    const reader = res.body!.getReader();
    const decoder = new TextDecoder();
    let collected = "";

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      collected += decoder.decode(value, { stream: true });
    }

    // Verify all chunks arrived
    expect(collected).toContain("response.created");
    expect(collected).toContain("Hello");
    expect(collected).toContain(" World");
    expect(collected).toContain("[DONE]");

    server.stop();
  });

  test("writes trace files for streaming response", async () => {
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
            await new Promise((r) => setTimeout(r, 5));
          }
          controller.close();
        },
      });

      return new Response(stream, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    });

    const req = makeRequest(url, "/v1/responses", { model: "o3", input: "trace stream" });
    const res = await handleRequest(req);

    // Consume the stream fully so trace files get finalized
    const reader = res.body!.getReader();
    while (!(await reader.read()).done) {}

    await waitForTraces();

    const entries = await readdir(TRACE_DIR);
    expect(entries.length).toBeGreaterThanOrEqual(1);

    const traceDir = join(TRACE_DIR, entries[entries.length - 1]!);

    // Check request.json exists
    const reqLog = JSON.parse(await readFile(join(traceDir, "request.json"), "utf-8"));
    expect(reqLog.method).toBe("POST");

    // Check response.stream has the SSE data
    const streamLog = await readFile(join(traceDir, "response.stream"), "utf-8");
    expect(streamLog).toContain("response.created");
    expect(streamLog).toContain("[DONE]");

    // Check meta.json marks it as streaming
    const meta = JSON.parse(await readFile(join(traceDir, "meta.json"), "utf-8"));
    expect(meta.streaming).toBe(true);

    server.stop();
  });
});

describe("Error code key retirement", () => {
  beforeEach(async () => {
    resetKeys([...TEST_KEYS]);
    await cleanTraces();
  });

  afterAll(async () => {
    await cleanTraces();
  });

  test("single error increments counter but does NOT exhaust the key", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(
        JSON.stringify({ error: { message: "Rate limit exceeded" } }),
        { status: 429, headers: { "content-type": "application/json" } },
      );
    });

    const req = makeRequest(url, "/v1/responses", { model: "o3", input: "test" });
    const res = await handleRequest(req);
    expect(res.status).toBe(429);

    const states = getKeyStates();
    expect(states[0]!.consecutiveErrors).toBe(1);
    expect(states[0]!.exhausted).toBe(false);

    server.stop();
  });

  test("5 consecutive errors exhaust the key", async () => {
    let callCount = 0;
    const { server, url } = startMockUpstream((req) => {
      const auth = req.headers.get("authorization") ?? "";
      if (auth.includes("0001")) {
        callCount++;
        return new Response(JSON.stringify({ error: "rate limit" }), {
          status: 429,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    for (let i = 1; i <= 13; i++) {
      await handleRequest(makeRequest(url, "/v1/responses", { input: `req_${i}` }));
    }

    expect(callCount).toBe(5);
    const states = getKeyStates();
    expect(states[0]!.exhausted).toBe(true);
    expect(states[1]!.exhausted).toBe(false);
    expect(states[2]!.exhausted).toBe(false);

    server.stop();
  });

  test("success resets consecutive error counter", async () => {
    let callCount = 0;
    const { server, url } = startMockUpstream((req) => {
      const auth = req.headers.get("authorization") ?? "";
      if (auth.includes("0001")) {
        callCount++;
        if (callCount <= 2) {
          return new Response(JSON.stringify({ error: "rate limit" }), {
            status: 429,
            headers: { "content-type": "application/json" },
          });
        }
      }
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    await handleRequest(makeRequest(url, "/v1/responses", { input: "1" }));
    expect(getKeyStates()[0]!.consecutiveErrors).toBe(1);

    await handleRequest(makeRequest(url, "/v1/responses", { input: "2" }));
    await handleRequest(makeRequest(url, "/v1/responses", { input: "3" }));

    await handleRequest(makeRequest(url, "/v1/responses", { input: "4" }));
    expect(getKeyStates()[0]!.consecutiveErrors).toBe(2);

    await handleRequest(makeRequest(url, "/v1/responses", { input: "5" }));
    await handleRequest(makeRequest(url, "/v1/responses", { input: "6" }));

    await handleRequest(makeRequest(url, "/v1/responses", { input: "7" }));
    expect(getKeyStates()[0]!.consecutiveErrors).toBe(0);
    expect(getKeyStates()[0]!.exhausted).toBe(false);

    server.stop();
  });

  test("all keys exhausted returns 503 after each key errors 5 times", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(JSON.stringify({ error: "payment required" }), {
        status: 402,
        headers: { "content-type": "application/json" },
      });
    });

    for (let i = 0; i < 15; i++) {
      await handleRequest(makeRequest(url, "/v1/responses", { input: `${i}` }));
    }

    const req = makeRequest(url, "/v1/responses", { input: "16" });
    const res = await handleRequest(req);

    expect(res.status).toBe(503);
    const body: any = await res.json();
    expect(body.error).toContain("All API keys exhausted");

    server.stop();
  });
});

describe("Per-key proxy support", () => {
  const origApiKeys = process.env.API_KEYS;
  const origApiProxies = process.env.API_PROXIES;
  const origProxies = process.env.PROXIES;

  beforeEach(async () => {
    await cleanTraces();
  });

  afterAll(async () => {
    process.env.API_KEYS = origApiKeys;
    process.env.API_PROXIES = origApiProxies;
    process.env.PROXIES = origProxies;
    await cleanTraces();
  });

  test("parses inline proxy format key|proxy in API_KEYS", () => {
    process.env.API_KEYS = "sk-key-1|http://127.0.0.1:8080,sk-key-2|socks5://127.0.0.1:1080,sk-key-3";
    delete process.env.API_PROXIES;
    delete process.env.PROXIES;

    loadKeys();

    const k1 = pickKey();
    const k2 = pickKey();
    const k3 = pickKey();

    expect(k1?.key).toBe("sk-key-1");
    expect(k1?.proxy).toBe("http://127.0.0.1:8080");

    expect(k2?.key).toBe("sk-key-2");
    expect(k2?.proxy).toBe("socks5://127.0.0.1:1080");

    expect(k3?.key).toBe("sk-key-3");
    expect(k3?.proxy).toBeNull();
  });

  test("cycles proxies in round-robin when there are more keys than proxies in API_PROXIES", () => {
    process.env.API_KEYS = "sk-key-1,sk-key-2,sk-key-3,sk-key-4,sk-key-5";
    process.env.API_PROXIES = "http://proxy1:8080,http://proxy2:8080,http://proxy3:8080";
    delete process.env.PROXIES;

    loadKeys();

    const states = getKeyStates();
    expect(states[0]?.proxy).toBe("http://proxy1:8080");
    expect(states[1]?.proxy).toBe("http://proxy2:8080");
    expect(states[2]?.proxy).toBe("http://proxy3:8080");
    expect(states[3]?.proxy).toBe("http://proxy1:8080"); // wraps around to proxy1
    expect(states[4]?.proxy).toBe("http://proxy2:8080"); // wraps around to proxy2
  });

  test("single API_PROXIES serves as default for all keys without inline proxy", () => {
    process.env.API_KEYS = "sk-key-1,sk-key-2";
    process.env.API_PROXIES = "http://common-proxy:8080";

    loadKeys();

    const states = getKeyStates();
    expect(states[0]?.proxy).toBe("http://common-proxy:8080");
    expect(states[1]?.proxy).toBe("http://common-proxy:8080");
  });

  test("resets keys with custom proxies in resetKeys", () => {
    resetKeys([
      { key: "sk-test-1", proxy: "http://proxy-a:8080" },
      "sk-test-2|http://proxy-b:8080",
      "sk-test-3",
    ]);

    const states = getKeyStates();
    expect(states[0]?.proxy).toBe("http://proxy-a:8080");
    expect(states[1]?.proxy).toBe("http://proxy-b:8080");
    expect(states[2]?.proxy).toBeNull();
  });

  test("records proxy in meta.json on trace", async () => {
    const { server, url } = startMockUpstream(() => {
      return new Response(JSON.stringify({ proxied: true }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });

    resetKeys([{ key: "sk-test-key-0001", proxy: url }]);

    const req = makeRequest(url, "/v1/responses", { input: "proxy trace check" });
    const res = await handleRequest(req);
    expect(res.status).toBe(200);

    await waitForTraces();

    const entries = await readdir(TRACE_DIR);
    expect(entries.length).toBeGreaterThanOrEqual(1);

    const traceDir = join(TRACE_DIR, entries[entries.length - 1]!);
    const meta = JSON.parse(await readFile(join(traceDir, "meta.json"), "utf-8"));

    expect(meta.proxy).toBe(url);

    server.stop();
  });
});
