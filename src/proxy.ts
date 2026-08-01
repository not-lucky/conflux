/**
 * Multi-provider proxy handler — forwards requests to matching upstream,
 * swapping client API keys with provider keys, and proxying through universal/key proxies.
 */

import { pickKeyForClient, recordError, recordSuccess, markExhausted, getActiveConfig } from "./keys";
import {
  startTrace,
  logJsonResponse,
  createStreamFile,
  appendStreamChunk,
  finalizeStreamTrace,
} from "./logger";

function isKeyErrorStatus(status: number): boolean {
  return (
    status === 401 ||
    status === 402 ||
    status === 403 ||
    status === 408 ||
    status === 429 ||
    status >= 500
  );
}

function isFatalKeyErrorStatus(status: number): boolean {
  return status === 401 || status === 403;
}

function isTestEnv(): boolean {
  return process.env.NODE_ENV === "test" || Boolean(process.env.BUN_TEST) || process.env.LOG_LEVEL === "silent";
}

function maskKey(key: string): string {
  return key.slice(0, 8) + "…" + key.slice(-4);
}

export function extractClientKey(req: Request, url: URL): string | null {
  const auth = req.headers.get("authorization");
  if (auth) {
    const parts = auth.split(" ");
    if (parts.length === 2 && parts[0]?.toLowerCase() === "bearer") {
      return parts[1]?.trim() ?? null;
    }
    return auth.trim();
  }

  const xApiKey = req.headers.get("x-api-key");
  if (xApiKey) return xApiKey.trim();

  const apiKey = req.headers.get("api-key");
  if (apiKey) return apiKey.trim();

  const qKey = url.searchParams.get("key") ?? url.searchParams.get("api_key");
  if (qKey) return qKey.trim();

  return null;
}

function isStreamingResponse(res: Response): boolean {
  const ct = res.headers.get("content-type") ?? "";
  return ct.includes("text/event-stream");
}

export async function handleRequest(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const clientKey = extractClientKey(req, url);

  // Buffer body text to enable logging and forwarding across retries
  const bodyText = req.body ? await req.text() : null;

  const config = getActiveConfig();
  const keysToReplace = new Set<string>();
  if (clientKey) keysToReplace.add(clientKey);

  if (config) {
    for (const ck of config.clientKeyMap.keys()) {
      keysToReplace.add(ck);
    }
  }

  const triedKeys = new Set<string>();
  let lastResponse: Response | null = null;
  let lastMasked = "";
  let lastProxy: string | null = null;
  let lastKeyNumber: number | null = null;
  let lastProxyNumber: number | null = null;
  let trace: Awaited<ReturnType<typeof startTrace>> | null = null;

  while (true) {
    // ── Select Provider & Key (excluding keys already tried for this request) ──
    const selection = pickKeyForClient(clientKey, triedKeys);

    if (selection.error === "unknown_client_key") {
      return new Response(
        JSON.stringify({
          error: "Unauthorized: Unknown or missing Conflux client API key",
          clientKey: clientKey ?? null,
        }),
        { status: 401, headers: { "content-type": "application/json" } },
      );
    }

    if (selection.error === "all_keys_exhausted" || !selection.keyState || !selection.baseUrl) {
      const now = Date.now();
      const hasUnexhaustedKeys = selection.provider?.keys.some((ks) => {
        if (!ks.exhaustedAt) return true;
        if (now - ks.exhaustedAt >= (selection.provider?.cooldownMs ?? 300000)) {
          return true;
        }
        return false;
      });

      if (hasUnexhaustedKeys && triedKeys.size > 0) {
        if (!isTestEnv()) {
          console.log(
            `🔄 All available keys tried in current pass for provider '${selection.provider?.name}'. Restarting rotation pass...`,
          );
        }
        triedKeys.clear();
        continue;
      }

      if (lastResponse) {
        if (trace) {
          return forwardJson(lastResponse, trace, lastMasked, lastProxy, lastKeyNumber, lastProxyNumber);
        }
        return lastResponse;
      }
      const providerName = selection.provider?.name ?? "default";
      return new Response(
        JSON.stringify({
          error: `All API keys for provider section '${providerName}' are exhausted. Try again later.`,
        }),
        { status: 503, headers: { "content-type": "application/json" } },
      );
    }

    const providerKey = selection.keyState.key;
    const keyNumber = selection.keyNumber;
    const proxyNumber = selection.proxyNumber;
    triedKeys.add(providerKey);
    const masked = maskKey(providerKey);

    trace ??= await startTrace(
      { method: req.method, url: req.url, headers: Object.fromEntries(req.headers.entries()) },
      bodyText,
      masked,
      keyNumber,
      proxyNumber,
    );

    // ── Build Upstream Headers & URL for this attempt ───────────
    const headers = new Headers(req.headers);
    headers.delete("host");

    const currentUrl = new URL(req.url);
    let replaced = false;

    // Replace client key anywhere in headers
    for (const [name, val] of headers.entries()) {
      let newVal = val;
      for (const k of keysToReplace) {
        if (newVal.includes(k)) {
          newVal = newVal.replaceAll(k, providerKey);
          replaced = true;
        }
      }
      if (newVal !== val) {
        headers.set(name, newVal);
      }
    }

    // Replace client key anywhere in query params
    for (const [param, val] of currentUrl.searchParams.entries()) {
      let newVal = val;
      for (const k of keysToReplace) {
        if (newVal.includes(k)) {
          newVal = newVal.replaceAll(k, providerKey);
          replaced = true;
        }
      }
      if (newVal !== val) {
        currentUrl.searchParams.set(param, newVal);
      }
    }

    // Default Authorization header if none was modified
    if (!replaced && !headers.has("authorization")) {
      headers.set("authorization", `Bearer ${providerKey}`);
    }

    const upstreamBase = selection.baseUrl.replace(/\/$/, "");
    const upstreamUrl = `${upstreamBase}${currentUrl.pathname}${currentUrl.search}`;

    let upstreamRes: Response;
    try {
      const fetchOpts: RequestInit & { proxy?: string } = {
        method: req.method,
        headers,
        body: bodyText,
      };
      if (selection.proxy) {
        fetchOpts.proxy = selection.proxy;
      }
      upstreamRes = await fetch(upstreamUrl, fetchOpts);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      if (!isTestEnv()) {
        console.error(
          `❌ Upstream fetch failed (${upstreamUrl}) using key #${keyNumber} (${masked}) and proxy #${proxyNumber ?? "none"} (${selection.proxy ?? "direct"}): ${msg}`,
        );
      }
      recordError(providerKey);
      lastMasked = masked;
      lastProxy = selection.proxy;
      lastKeyNumber = keyNumber;
      lastProxyNumber = proxyNumber;
      lastResponse = new Response(
        JSON.stringify({ error: "Upstream request failed", detail: msg }),
        { status: 502, headers: { "content-type": "application/json" } },
      );
      continue;
    }

    // ── Track Key Error / Success & Retry on Upstream Key Error ──────
    if (isKeyErrorStatus(upstreamRes.status)) {
      if (isFatalKeyErrorStatus(upstreamRes.status)) {
        markExhausted(providerKey);
      } else {
        recordError(providerKey);
      }
      lastMasked = masked;
      lastProxy = selection.proxy;
      lastKeyNumber = keyNumber;
      lastProxyNumber = proxyNumber;
      lastResponse = upstreamRes;
      if (!isTestEnv()) {
        console.warn(
          `⚠️  Upstream returned error status ${upstreamRes.status} for key #${keyNumber} (${masked}) and proxy #${proxyNumber ?? "none"}. Retrying with next key...`,
        );
      }
      continue;
    }

    if (upstreamRes.status >= 200 && upstreamRes.status < 300) {
      recordSuccess(providerKey);
    }

    if (!isTestEnv()) {
      console.log(
        `🔑 Used key #${keyNumber} (${masked}) and proxy #${proxyNumber ?? "none"}${selection.proxy ? ` (${selection.proxy})` : ""}`,
      );
    }

    // ── Forward Response ───────────────────────────────────────────
    if (isStreamingResponse(upstreamRes)) {
      return forwardStreaming(upstreamRes, trace, masked, providerKey, selection.proxy, keyNumber, proxyNumber);
    }

    return forwardJson(upstreamRes, trace, masked, selection.proxy, keyNumber, proxyNumber);
  }
}

/** Forward JSON response. */
async function forwardJson(
  upstreamRes: Response,
  trace: Awaited<ReturnType<typeof startTrace>>,
  masked: string,
  proxy: string | null = null,
  keyNumber: number | null = null,
  proxyNumber: number | null = null,
): Promise<Response> {
  const body = await upstreamRes.text();

  void logJsonResponse(trace, upstreamRes, body, masked, proxy, keyNumber, proxyNumber).catch((e: unknown) => {
    if (!isTestEnv()) {
      console.error("Trace write error:", e);
    }
  });

  const resHeaders = new Headers(upstreamRes.headers);
  if (keyNumber !== null) {
    resHeaders.set("x-conflux-key-number", String(keyNumber));
  }
  if (proxyNumber !== null) {
    resHeaders.set("x-conflux-proxy-number", String(proxyNumber));
  }

  return new Response(body, {
    status: upstreamRes.status,
    headers: resHeaders,
  });
}

/** Forward SSE streaming response. */
async function forwardStreaming(
  upstreamRes: Response,
  trace: Awaited<ReturnType<typeof startTrace>>,
  masked: string,
  providerKey: string,
  proxy: string | null = null,
  keyNumber: number | null = null,
  proxyNumber: number | null = null,
): Promise<Response> {
  const streamPath = await createStreamFile(trace);

  const upstream = upstreamRes.body;
  const resHeaders = new Headers(upstreamRes.headers);
  if (keyNumber !== null) {
    resHeaders.set("x-conflux-key-number", String(keyNumber));
  }
  if (proxyNumber !== null) {
    resHeaders.set("x-conflux-proxy-number", String(proxyNumber));
  }

  if (!upstream) {
    return new Response(null, {
      status: upstreamRes.status,
      headers: resHeaders,
    });
  }

  const reader = upstream.getReader() as ReadableStreamDefaultReader<Uint8Array>;
  const decoder = new TextDecoder();
  let streamErrorRecorded = false;

  const passthrough = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const readResult = await reader.read();
        if (readResult.done) {
          controller.close();
          void finalizeStreamTrace(trace, upstreamRes, masked, upstreamRes.status, proxy, keyNumber, proxyNumber).catch((e: unknown) => {
            if (!isTestEnv()) {
              console.error("Trace finalize error:", e);
            }
          });
          return;
        }

        const chunk = readResult.value;
        controller.enqueue(chunk);

        const text = decoder.decode(chunk, { stream: true });
        void appendStreamChunk(streamPath, text).catch((e: unknown) => {
          if (!isTestEnv()) {
            console.error("Trace chunk write error:", e);
          }
        });

        if (
          !streamErrorRecorded &&
          text.includes('"error"') &&
          (text.includes("429") || text.includes("402") || text.includes("insufficient") || text.includes("401") || text.includes("403"))
        ) {
          streamErrorRecorded = true;
          if (text.includes("401") || text.includes("403") || text.includes("invalid_api_key")) {
            markExhausted(providerKey);
          } else {
            recordError(providerKey);
          }
        }
      } catch (err: unknown) {
        controller.error(err);
      }
    },
    cancel() {
      void reader.cancel();
    },
  });

  return new Response(passthrough, {
    status: upstreamRes.status,
    headers: upstreamRes.headers,
  });
}
