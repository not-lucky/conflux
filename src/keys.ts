/**
 * Multi-provider key management with round-robin key selection,
 * consecutive error tracking, and proxy resolution.
 */

import { type ConfluxConfig, DEFAULT_MAX_CONSECUTIVE_ERRORS, DEFAULT_COOLDOWN_MS } from "./config";

function isTestEnv(): boolean {
  return process.env.NODE_ENV === "test" || Boolean(process.env.BUN_TEST) || process.env.LOG_LEVEL === "silent";
}

export interface KeyState {
  key: string;
  proxy: string | null;
  exhaustedAt: number | null;
  consecutiveErrors: number;
}

export interface ProviderState {
  name: string;
  baseUrl: string;
  confluxApiKeys: string[];
  keys: KeyState[];
  cursor: number;
  proxies: string[];
  proxyCursor: number;
  maxConsecutiveErrors: number;
  cooldownMs: number;
  rotateProxiesInterval?: number;
  requestCount: number;
}

export interface KeySelection {
  keyState: KeyState | null;
  keyNumber: number | null;
  provider: ProviderState | null;
  proxy: string | null;
  proxyNumber: number | null;
  baseUrl: string | null;
  error: "unknown_client_key" | "all_keys_exhausted" | null;
}

export interface ProviderKeySnapshot {
  masked: string;
  proxy: string | null;
  exhausted: boolean;
  exhaustedAt: string | null;
  consecutiveErrors: number;
}

export interface ProviderStatusSnapshot {
  baseUrl: string;
  confluxApiKeys: string[];
  maxConsecutiveErrors: number;
  cooldownMs: number;
  rotateProxiesInterval?: number;
  keys: ProviderKeySnapshot[];
}

export interface KeyStatesSnapshot {
  universalProxies: string[];
  providers: Record<string, ProviderStatusSnapshot>;
}

let activeConfig: ConfluxConfig | null = null;
const providerStates = new Map<string, ProviderState>();
const clientKeyToProvider = new Map<string, ProviderState>();
let defaultProviderState: ProviderState | undefined = undefined;
let universalProxyCursor = 0;
let universalRequestCount = 0;

export function initKeys(config: ConfluxConfig): void {
  activeConfig = config;
  providerStates.clear();
  clientKeyToProvider.clear();
  defaultProviderState = undefined;
  universalProxyCursor = 0;
  universalRequestCount = 0;

  for (const [name, pConfig] of config.providers.entries()) {
    const pState: ProviderState = {
      name: pConfig.name,
      baseUrl: pConfig.baseUrl,
      confluxApiKeys: pConfig.confluxApiKeys,
      keys: pConfig.keys.map((k) => ({
        key: k.key,
        proxy: k.proxy,
        exhaustedAt: null,
        consecutiveErrors: 0,
      })),
      cursor: 0,
      proxies: pConfig.proxies,
      proxyCursor: 0,
      maxConsecutiveErrors: pConfig.maxConsecutiveErrors ?? config.maxConsecutiveErrors ?? DEFAULT_MAX_CONSECUTIVE_ERRORS,
      cooldownMs: pConfig.cooldownMs ?? config.cooldownMs ?? DEFAULT_COOLDOWN_MS,
      rotateProxiesInterval: pConfig.rotateProxiesInterval ?? config.rotateProxiesInterval,
      requestCount: 0,
    };

    providerStates.set(name, pState);

    for (const ck of pConfig.confluxApiKeys) {
      if (ck !== "*") {
        clientKeyToProvider.set(ck, pState);
      }
    }

    if (config.defaultProvider?.name === name) {
      defaultProviderState = pState;
    }
  }

  if (!defaultProviderState && config.defaultProvider) {
    const pConfig = config.defaultProvider;
    defaultProviderState = {
      name: pConfig.name,
      baseUrl: pConfig.baseUrl,
      confluxApiKeys: pConfig.confluxApiKeys,
      keys: pConfig.keys.map((k) => ({
        key: k.key,
        proxy: k.proxy,
        exhaustedAt: null,
        consecutiveErrors: 0,
      })),
      cursor: 0,
      proxies: pConfig.proxies,
      proxyCursor: 0,
      maxConsecutiveErrors: pConfig.maxConsecutiveErrors ?? config.maxConsecutiveErrors ?? DEFAULT_MAX_CONSECUTIVE_ERRORS,
      cooldownMs: pConfig.cooldownMs ?? config.cooldownMs ?? DEFAULT_COOLDOWN_MS,
      rotateProxiesInterval: pConfig.rotateProxiesInterval ?? config.rotateProxiesInterval,
      requestCount: 0,
    };
    providerStates.set(pConfig.name, defaultProviderState);
  }

  const totalKeys = Array.from(providerStates.values()).reduce((sum, p) => sum + p.keys.length, 0);
  if (!isTestEnv()) {
    console.log(
      `🔑 Loaded ${providerStates.size} provider profile(s) with ${totalKeys} key(s) total and ${config.universalProxies.length} universal proxy/proxies.`,
    );
  }
}

/** Get active config */
export function getActiveConfig(): ConfluxConfig | null {
  return activeConfig;
}

/** Pick an available API key & proxy for an incoming client key. */
export function pickKeyForClient(clientKey?: string | null, excludeKeys?: Set<string>): KeySelection {
  let providerState: ProviderState | undefined;

  if (clientKey) {
    providerState = clientKeyToProvider.get(clientKey);
  }

  providerState ??= defaultProviderState;

  if (!providerState) {
    return {
      keyState: null,
      keyNumber: null,
      provider: null,
      proxy: null,
      proxyNumber: null,
      baseUrl: null,
      error: "unknown_client_key",
    };
  }

  const now = Date.now();
  const keys = providerState.keys;
  const n = keys.length;

  let selectedKey: KeyState | null = null;
  let keyNumber: number | null = null;

  for (let i = 0; i < n; i++) {
    const idx = (providerState.cursor + i) % n;
    const ks = keys[idx];
    if (!ks) continue;

    // Reset cooldown if cooldown duration has passed
    if (ks.exhaustedAt && now - ks.exhaustedAt >= providerState.cooldownMs) {
      ks.exhaustedAt = null;
      ks.consecutiveErrors = 0;
    }

    if (!ks.exhaustedAt && (!excludeKeys?.has(ks.key))) {
      providerState.cursor = (idx + 1) % n;
      selectedKey = ks;
      keyNumber = idx + 1;
      break;
    }
  }

  if (!selectedKey) {
    return {
      keyState: null,
      keyNumber: null,
      provider: providerState,
      proxy: null,
      proxyNumber: null,
      baseUrl: providerState.baseUrl,
      error: "all_keys_exhausted",
    };
  }

  // Resolve proxy: Key inline proxy > Provider section proxy > Universal proxy
  let proxy: string | null = null;
  let proxyNumber: number | null = null;

  const rotateInterval = providerState.rotateProxiesInterval ?? activeConfig?.rotateProxiesInterval;

  if (selectedKey.proxy) {
    proxy = selectedKey.proxy;
    proxyNumber = 1;
  } else if (providerState.proxies.length > 0) {
    if (rotateInterval && rotateInterval > 0) {
      const keyIndex = keyNumber !== null ? keyNumber - 1 : 0;
      const shift = Math.floor(providerState.requestCount / rotateInterval);
      const pIndex = (keyIndex + shift) % providerState.proxies.length;
      proxy = providerState.proxies[pIndex] ?? null;
      proxyNumber = pIndex + 1;
    } else {
      const pIndex = providerState.proxyCursor % providerState.proxies.length;
      proxy = providerState.proxies[pIndex] ?? null;
      proxyNumber = pIndex + 1;
      providerState.proxyCursor = (providerState.proxyCursor + 1) % providerState.proxies.length;
    }
  } else if (activeConfig && activeConfig.universalProxies.length > 0) {
    if (rotateInterval && rotateInterval > 0) {
      const keyIndex = keyNumber !== null ? keyNumber - 1 : 0;
      const shift = Math.floor(universalRequestCount / rotateInterval);
      const uIndex = (keyIndex + shift) % activeConfig.universalProxies.length;
      proxy = activeConfig.universalProxies[uIndex] ?? null;
      proxyNumber = uIndex + 1;
    } else {
      const uIndex = universalProxyCursor % activeConfig.universalProxies.length;
      proxy = activeConfig.universalProxies[uIndex] ?? null;
      proxyNumber = uIndex + 1;
      universalProxyCursor = (universalProxyCursor + 1) % activeConfig.universalProxies.length;
    }
  }

  providerState.requestCount += 1;
  universalRequestCount += 1;

  return {
    keyState: selectedKey,
    keyNumber,
    provider: providerState,
    proxy,
    proxyNumber,
    baseUrl: providerState.baseUrl,
    error: null,
  };
}

/** Record an error for a key. If consecutive errors reach maxConsecutiveErrors, mark as exhausted. */
export function recordError(key: string): void {
  for (const provider of providerStates.values()) {
    const ks = provider.keys.find((k) => k.key === key);
    if (ks) {
      if (ks.exhaustedAt !== null) return;
      ks.consecutiveErrors += 1;
      const masked = key.slice(0, 8) + "…" + key.slice(-4);
      if (ks.consecutiveErrors >= provider.maxConsecutiveErrors) {
        ks.exhaustedAt = Date.now();
        const remaining = provider.keys.filter((k) => !k.exhaustedAt).length;
        if (!isTestEnv()) {
          console.warn(
            `⚠️  [${provider.name}] Key ${masked} reached ${ks.consecutiveErrors} consecutive errors and is now EXHAUSTED. ${remaining}/${provider.keys.length} keys remaining.`,
          );
        }
      } else if (!isTestEnv()) {
        console.warn(
          `⚠️  [${provider.name}] Key ${masked} error recorded (${ks.consecutiveErrors}/${provider.maxConsecutiveErrors}).`,
        );
      }
      return;
    }
  }
}

/** Record a success for a key, resetting its consecutive error counter to 0. */
export function recordSuccess(key: string): void {
  for (const provider of providerStates.values()) {
    const ks = provider.keys.find((k) => k.key === key);
    if (ks) {
      if (ks.consecutiveErrors > 0 && !isTestEnv()) {
        const masked = key.slice(0, 8) + "…" + key.slice(-4);
        console.log(
          `✅ [${provider.name}] Key ${masked} succeeded, resetting consecutive errors from ${ks.consecutiveErrors} to 0.`,
        );
      }
      ks.consecutiveErrors = 0;
      return;
    }
  }
}

/** Mark a key as exhausted immediately. */
export function markExhausted(key: string): void {
  for (const provider of providerStates.values()) {
    const ks = provider.keys.find((k) => k.key === key);
    if (ks) {
      if (ks.exhaustedAt !== null) return;
      ks.exhaustedAt = Date.now();
      ks.consecutiveErrors = provider.maxConsecutiveErrors;
      const remaining = provider.keys.filter((k) => !k.exhaustedAt).length;
      const masked = key.slice(0, 8) + "…" + key.slice(-4);
      if (!isTestEnv()) {
        console.warn(
          `⚠️  [${provider.name}] Key ${masked} exhausted (auth/invalid key error). ${remaining}/${provider.keys.length} keys remaining.`,
        );
      }
      return;
    }
  }
}

/** Get snapshot of all key states grouped by provider section for /_status. */
export function getKeyStates(): KeyStatesSnapshot {
  const result: Record<string, ProviderStatusSnapshot> = {};

  for (const [name, provider] of providerStates.entries()) {
    result[name] = {
      baseUrl: provider.baseUrl,
      confluxApiKeys: provider.confluxApiKeys,
      maxConsecutiveErrors: provider.maxConsecutiveErrors,
      cooldownMs: provider.cooldownMs,
      rotateProxiesInterval: provider.rotateProxiesInterval,
      keys: provider.keys.map((ks) => ({
        masked: ks.key.slice(0, 8) + "…" + ks.key.slice(-4),
        proxy: ks.proxy,
        exhausted: ks.exhaustedAt !== null,
        exhaustedAt: ks.exhaustedAt ? new Date(ks.exhaustedAt).toISOString() : null,
        consecutiveErrors: ks.consecutiveErrors,
      })),
    };
  }

  return {
    universalProxies: activeConfig?.universalProxies ?? [],
    providers: result,
  };
}

/** Reset keys for unit tests */
export function resetProviderStates(config: ConfluxConfig): void {
  initKeys(config);
}
