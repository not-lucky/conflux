/**
 * Proxy handler — forwards requests to upstream OpenAI,
 * swapping the API key and pass-through-ing the response.
 */

import { pickKey, recordError, recordSuccess, markExhausted, type KeyState } from "./keys";
import {
  startTrace,
  logJsonResponse,
  createStreamFile,
  appendStreamChunk,
  finalizeStreamTrace,
} from "./logger";

function getUpstream(): string {
  return (process.env.UPSTREAM_URL ?? "https://api.openai.com").replace(/\/$/, "");
}

/** Budget / rate-limit related status codes that mean "try another key". */
const EXHAUSTION_CODES = new Set([429, 402]);

function maskKey(key: string): string {
  return key.slice(0, 8) + "…" + key.slice(-4);
}

function isStreamingRequest(req: Request): boolean {
  const ct = req.headers.get("content-type") ?? "";
  if (!ct.includes("json")) return false;
  // We can't peek at body without consuming it, but OpenAI streaming is
  // indicated by `"stream": true` in the JSON body. We'll detect streaming
  // from the *response* content-type instead (text/event-stream).
  return false; // detection happens on response side
}

function isStreamingResponse(res: Response): boolean {
  const ct = res.headers.get("content-type") ?? "";
  return ct.includes("text/event-stream");
}

export async function handleRequest(req: Request): Promise<Response> {
  // ── Pick a key ─────────────────────────────────────────────────
  const ks = pickKey();
  if (!ks) {
    return new Response(
      JSON.stringify({ error: "All API keys exhausted. Try again later." }),
      { status: 503, headers: { "content-type": "application/json" } },
    );
  }

  const masked = maskKey(ks.key);

  // ── Build upstream request ─────────────────────────────────────
  const url = new URL(req.url);

  const headers = new Headers(req.headers);
  headers.delete("host");

  // ── Replace conflux key anywhere it appears in headers or query params ───
  const clientKeys = Array.from(
    new Set([process.env.CLIENT_KEY, "dummy"].filter((k): k is string => Boolean(k))),
  );

  let replaced = false;

  // Replace conflux key anywhere it appears in ANY header
  for (const [name, val] of headers.entries()) {
    let newVal = val;
    for (const k of clientKeys) {
      if (newVal.includes(k)) {
        newVal = newVal.replaceAll(k, ks.key);
        replaced = true;
      }
    }
    if (newVal !== val) {
      headers.set(name, newVal);
    }
  }

  // Replace conflux key anywhere it appears in ANY query parameter
  for (const [param, val] of url.searchParams.entries()) {
    let newVal = val;
    for (const k of clientKeys) {
      if (newVal.includes(k)) {
        newVal = newVal.replaceAll(k, ks.key);
        replaced = true;
      }
    }
    if (newVal !== val) {
      url.searchParams.set(param, newVal);
    }
  }

  // Fallback: If no auth header was present or replaced at all, default to Bearer token
  if (!replaced && !headers.has("authorization")) {
    headers.set("authorization", `Bearer ${ks.key}`);
  }

  const upstreamUrl = `${getUpstream()}${url.pathname}${url.search}`;

  // Buffer body once — avoids stream-consumed-twice issues between
  // the trace logger and the upstream fetch.
  const bodyText = req.body ? await req.text() : null;

  // Start trace with the buffered body
  const trace = await startTrace(
    { method: req.method, url: req.url, headers: Object.fromEntries(req.headers.entries()) },
    bodyText,
    masked,
  );

  let upstreamRes: Response;
  try {
    const fetchOpts: RequestInit & { proxy?: string } = {
      method: req.method,
      headers,
      body: bodyText,
    };
    if (ks.proxy) {
      fetchOpts.proxy = ks.proxy;
    }
    upstreamRes = await fetch(upstreamUrl, fetchOpts);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    console.error(`❌ Upstream fetch failed: ${msg}`);
    return new Response(
      JSON.stringify({ error: "Upstream request failed", detail: msg }),
      { status: 502, headers: { "content-type": "application/json" } },
    );
  }

  // ── Check for key error / success ─────────────────────────────
  if (EXHAUSTION_CODES.has(upstreamRes.status)) {
    recordError(ks.key);
  } else if (upstreamRes.status >= 200 && upstreamRes.status < 300) {
    recordSuccess(ks.key);
  }

  // ── Forward response ───────────────────────────────────────────
  if (isStreamingResponse(upstreamRes)) {
    return forwardStreaming(upstreamRes, trace, masked, ks);
  }

  return forwardJson(upstreamRes, trace, masked, ks.proxy);
}

/** Passthrough a regular JSON response. */
async function forwardJson(
  upstreamRes: Response,
  trace: Awaited<ReturnType<typeof startTrace>>,
  masked: string,
  proxy: string | null = null,
): Promise<Response> {
  const body = await upstreamRes.text();

  // Log asynchronously — don't block the response
  logJsonResponse(trace, upstreamRes, body, masked, proxy).catch((e) =>
    console.error("Trace write error:", e),
  );

  return new Response(body, {
    status: upstreamRes.status,
    headers: upstreamRes.headers,
  });
}

/** Passthrough a streaming (SSE) response, teeing chunks to disk. */
async function forwardStreaming(
  upstreamRes: Response,
  trace: Awaited<ReturnType<typeof startTrace>>,
  masked: string,
  ks: KeyState,
): Promise<Response> {
  const streamPath = await createStreamFile(trace);

  const upstream = upstreamRes.body;
  if (!upstream) {
    return new Response(null, {
      status: upstreamRes.status,
      headers: upstreamRes.headers,
    });
  }

  const reader = upstream.getReader();
  const decoder = new TextDecoder();

  const passthrough = new ReadableStream({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          controller.close();
          // Finalize trace after stream ends
          finalizeStreamTrace(trace, upstreamRes, masked, upstreamRes.status, ks.proxy).catch((e) =>
            console.error("Trace finalize error:", e),
          );
          return;
        }

        // Forward chunk to client
        controller.enqueue(value);

        // Log chunk to disk (fire-and-forget)
        const text = decoder.decode(value, { stream: true });
        appendStreamChunk(streamPath, text).catch((e) =>
          console.error("Trace chunk write error:", e),
        );

        // Check for budget errors mid-stream (OpenAI can send error events)
        if (text.includes('"error"') && (text.includes("429") || text.includes("402") || text.includes("insufficient"))) {
          recordError(ks.key);
        }
      } catch (err) {
        controller.error(err);
      }
    },
    cancel() {
      reader.cancel();
    },
  });

  return new Response(passthrough, {
    status: upstreamRes.status,
    headers: upstreamRes.headers,
  });
}
