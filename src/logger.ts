/**
 * Bun-native request/response trace logger.
 *
 * Each request gets its own directory under ./logs/trace/<timestamp>-<id>/
 * containing:
 *   - request.json         — method, url, headers, body
 *   - response.json        — status, headers, body (for non-streaming)
 *   - response.stream      — raw concatenated chunks (for streaming / SSE)
 *   - response_headers.json — headers (for streaming)
 *   - meta.json            — timing, key used, streaming flag, active proxy
 */

import { mkdir, appendFile } from "node:fs/promises";
import { dirname } from "node:path";

const TRACE_DIR = "logs/trace";

let counter = 0;

export interface TraceHandle {
  dir: string;
  id: string;
  startTime: number;
}

function pad(n: number, len = 4): string {
  return String(n).padStart(len, "0");
}

export async function startTrace(
  meta: { method: string; url: string; headers: Record<string, string> },
  bodyText: string | null,
  _keyMasked: string,
  _keyNumber?: number | null,
  _proxyNumber?: number | null,
): Promise<TraceHandle> {
  const now = new Date();
  const ts = now.toISOString().replace(/[:.]/g, "-");
  const id = `${ts}_${pad(counter++)}`;
  const dir = `${TRACE_DIR}/${id}`;

  await mkdir(dir, { recursive: true });

  const requestData = {
    method: meta.method,
    url: meta.url,
    headers: meta.headers,
    body: tryParseJson(bodyText),
  };

  await Bun.write(`${dir}/request.json`, JSON.stringify(requestData, null, 2));

  return { dir, id, startTime: Date.now() };
}

export async function logJsonResponse(
  trace: TraceHandle,
  res: Response,
  body: string,
  keyMasked: string,
  proxy: string | null = null,
  keyNumber: number | null = null,
  proxyNumber: number | null = null,
): Promise<void> {
  const responseData = {
    status: res.status,
    headers: Object.fromEntries(res.headers.entries()),
    body: tryParseJson(body),
  };

  await Promise.all([
    Bun.write(`${trace.dir}/response.json`, JSON.stringify(responseData, null, 2)),
    writeMeta(trace, keyMasked, false, res.status, proxy, keyNumber, proxyNumber),
  ]);
}

export async function createStreamFile(trace: TraceHandle): Promise<string> {
  const path = `${trace.dir}/response.stream`;
  await mkdir(trace.dir, { recursive: true });
  await appendFile(path, "");
  return path;
}

export async function appendStreamChunk(path: string, chunk: string): Promise<void> {
  try {
    await appendFile(path, chunk);
  } catch {
    await mkdir(dirname(path), { recursive: true });
    await appendFile(path, chunk);
  }
}

export async function finalizeStreamTrace(
  trace: TraceHandle,
  res: Response,
  keyMasked: string,
  status: number,
  proxy: string | null = null,
  keyNumber: number | null = null,
  proxyNumber: number | null = null,
): Promise<void> {
  const responseHeaders = {
    status,
    headers: Object.fromEntries(res.headers.entries()),
  };
  await Promise.all([
    Bun.write(`${trace.dir}/response_headers.json`, JSON.stringify(responseHeaders, null, 2)),
    writeMeta(trace, keyMasked, true, status, proxy, keyNumber, proxyNumber),
  ]);
}

async function writeMeta(
  trace: TraceHandle,
  keyMasked: string,
  streaming: boolean,
  status: number,
  proxy: string | null = null,
  keyNumber: number | null = null,
  proxyNumber: number | null = null,
): Promise<void> {
  const meta = {
    id: trace.id,
    keyUsed: keyMasked,
    keyNumber,
    proxy,
    proxyNumber,
    streaming,
    status,
    durationMs: Date.now() - trace.startTime,
    timestamp: new Date().toISOString(),
  };
  await Bun.write(`${trace.dir}/meta.json`, JSON.stringify(meta, null, 2));
}

function tryParseJson(text: string | null): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
