/**
 * Round-robin API key selector with consecutive error tracking.
 *
 * Keys track consecutive errors (429, 402, etc.). When a key errors 5 times in a row,
 * it is marked exhausted with a cooldown. If a key succeeds before reaching 5 errors,
 * its error counter is reset to 0.
 */

const COOLDOWN_MS = 5 * 60 * 60 * 1000; // 5 hours — matches the $5/5h budget window
const MAX_CONSECUTIVE_ERRORS = 5;

export interface KeyState {
  key: string;
  proxy: string | null;
  exhaustedAt: number | null;
  consecutiveErrors: number;
}

let keys: KeyState[] = [];
let cursor = 0;

export function loadKeys(): void {
  const rawKeys = process.env.API_KEYS ?? "";
  const rawProxies = process.env.API_PROXIES ?? process.env.PROXIES ?? "";

  const proxyList = rawProxies
    .split(",")
    .map((p) => p.trim())
    .filter(Boolean);

  const parsedKeys = rawKeys
    .split(",")
    .map((k) => k.trim())
    .filter(Boolean);

  if (parsedKeys.length === 0) {
    console.error("❌ No API keys found. Set API_KEYS in .env");
    process.exit(1);
  }

  keys = parsedKeys.map((item, idx) => {
    let key = item;
    let proxy: string | null = null;

    if (item.includes("|")) {
      const parts = item.split("|");
      key = parts[0]!.trim();
      proxy = parts.slice(1).join("|").trim() || null;
    } else if (proxyList.length > 0) {
      proxy = proxyList[idx % proxyList.length]!;
    }

    return {
      key,
      proxy,
      exhaustedAt: null,
      consecutiveErrors: 0,
    };
  });

  const withProxyCount = keys.filter((k) => k.proxy !== null).length;
  console.log(`🔑 Loaded ${keys.length} API key(s) (${withProxyCount} with separate proxy)`);
}

/** Pick the next available key (round-robin, skipping exhausted). */
export function pickKey(): KeyState | null {
  const now = Date.now();
  const n = keys.length;

  for (let i = 0; i < n; i++) {
    const idx = (cursor + i) % n;
    const ks = keys[idx]!;

    // Re-enable if cooldown expired
    if (ks.exhaustedAt && now - ks.exhaustedAt >= COOLDOWN_MS) {
      ks.exhaustedAt = null;
      ks.consecutiveErrors = 0;
    }

    if (!ks.exhaustedAt) {
      cursor = (idx + 1) % n; // advance for next call
      return ks;
    }
  }

  return null; // all keys exhausted
}

/** Record an error for a key. If consecutive errors reach 5, mark as exhausted. */
export function recordError(key: string): void {
  const ks = keys.find((k) => k.key === key);
  if (ks) {
    ks.consecutiveErrors += 1;
    const masked = key.slice(0, 8) + "…" + key.slice(-4);
    if (ks.consecutiveErrors >= MAX_CONSECUTIVE_ERRORS) {
      ks.exhaustedAt = Date.now();
      const remaining = keys.filter((k) => !k.exhaustedAt).length;
      console.warn(
        `⚠️  Key ${masked} reached ${ks.consecutiveErrors} consecutive errors and is now EXHAUSTED. ${remaining}/${keys.length} keys remaining.`,
      );
    } else {
      console.warn(
        `⚠️  Key ${masked} error recorded (${ks.consecutiveErrors}/${MAX_CONSECUTIVE_ERRORS}).`,
      );
    }
  }
}

/** Record a success for a key, resetting its consecutive error counter to 0. */
export function recordSuccess(key: string): void {
  const ks = keys.find((k) => k.key === key);
  if (ks) {
    if (ks.consecutiveErrors > 0) {
      const masked = key.slice(0, 8) + "…" + key.slice(-4);
      console.log(
        `✅ Key ${masked} succeeded, resetting consecutive errors from ${ks.consecutiveErrors} to 0.`,
      );
    }
    ks.consecutiveErrors = 0;
  }
}

/** Mark a key as exhausted immediately so it won't be picked again until cooldown. */
export function markExhausted(key: string): void {
  const ks = keys.find((k) => k.key === key);
  if (ks) {
    ks.exhaustedAt = Date.now();
    ks.consecutiveErrors = MAX_CONSECUTIVE_ERRORS;
    const remaining = keys.filter((k) => !k.exhaustedAt).length;
    const masked = key.slice(0, 8) + "…" + key.slice(-4);
    console.warn(`⚠️  Key ${masked} exhausted. ${remaining}/${keys.length} keys remaining.`);
  }
}

/** Get a snapshot of all key states (for the status endpoint). */
export function getKeyStates(): {
  masked: string;
  proxy: string | null;
  exhausted: boolean;
  exhaustedAt: string | null;
  consecutiveErrors: number;
}[] {
  return keys.map((ks) => ({
    masked: ks.key.slice(0, 8) + "…" + ks.key.slice(-4),
    proxy: ks.proxy,
    exhausted: ks.exhaustedAt !== null,
    exhaustedAt: ks.exhaustedAt ? new Date(ks.exhaustedAt).toISOString() : null,
    consecutiveErrors: ks.consecutiveErrors,
  }));
}

/** Reset key state — for tests only. */
export function resetKeys(
  apiKeys: (string | { key: string; proxy?: string | null })[],
): void {
  keys = apiKeys.map((item) => {
    if (typeof item === "string") {
      if (item.includes("|")) {
        const parts = item.split("|");
        return {
          key: parts[0]!.trim(),
          proxy: parts.slice(1).join("|").trim() || null,
          exhaustedAt: null,
          consecutiveErrors: 0,
        };
      }
      return { key: item, proxy: null, exhaustedAt: null, consecutiveErrors: 0 };
    }
    return {
      key: item.key,
      proxy: item.proxy ?? null,
      exhaustedAt: null,
      consecutiveErrors: 0,
    };
  });
  cursor = 0;
}
