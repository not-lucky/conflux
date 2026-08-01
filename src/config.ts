export interface KeyConfig {
  key: string;
  proxy: string | null;
}

export interface ProviderConfig {
  name: string;
  confluxApiKeys: string[];
  baseUrl: string;
  keys: KeyConfig[];
  proxies: string[];
  maxConsecutiveErrors: number;
  cooldownMs: number;
  rotateProxiesInterval?: number;
}

export interface ConfluxConfig {
  port: number;
  universalProxies: string[];
  maxConsecutiveErrors: number;
  cooldownMs: number;
  rotateProxiesInterval?: number;
  providers: Map<string, ProviderConfig>;
  clientKeyMap: Map<string, ProviderConfig>;
  defaultProvider?: ProviderConfig;
  configPath: string;
}

export const DEFAULT_MAX_CONSECUTIVE_ERRORS = 5;
export const DEFAULT_COOLDOWN_MS = 5 * 60 * 60 * 1000; // 5 hours in milliseconds

export function parseMaxConsecutiveErrors(raw: unknown, defaultVal = DEFAULT_MAX_CONSECUTIVE_ERRORS): number {
  if (raw === undefined || raw === null) return defaultVal;
  const num = Number(raw);
  if (!isNaN(num) && num > 0) {
    return Math.floor(num);
  }
  return defaultVal;
}

export function parseCooldownToMs(raw: unknown, defaultMs = DEFAULT_COOLDOWN_MS): number {
  if (raw === undefined || raw === null) return defaultMs;
  if (typeof raw === "number") {
    if (isNaN(raw) || raw <= 0) return defaultMs;
    return raw >= 100000 ? raw : raw * 1000;
  }
  if (typeof raw === "string") {
    const trimmed = raw.trim();
    if (!trimmed) return defaultMs;
    const match = /^(\d+(?:\.\d+)?)\s*(h|m|s|ms|hours?|minutes?|mins?|secs?|seconds?)?$/i.exec(trimmed);
    if (match?.[1]) {
      const val = parseFloat(match[1]);
      const unit = (match[2] ?? "s").toLowerCase();
      if (isNaN(val) || val <= 0) return defaultMs;
      if (unit.startsWith("h")) return val * 60 * 60 * 1000;
      if (unit.startsWith("m") && !unit.startsWith("ms")) return val * 60 * 1000;
      if (unit === "ms") return val;
      if (unit.startsWith("s")) return val * 1000;
    }
    const num = Number(trimmed);
    if (!isNaN(num) && num > 0) {
      return num >= 100000 ? num : num * 1000;
    }
  }
  return defaultMs;
}

export function parseRotateProxiesInterval(raw: unknown): number | undefined {
  if (raw === undefined || raw === null) return undefined;
  const num = Number(raw);
  if (!isNaN(num) && num > 0) {
    return Math.floor(num);
  }
  return undefined;
}

interface RawProvider {
  base_url?: string;
  baseUrl?: string;
  api_keys?: unknown;
  apiKeys?: unknown;
  proxies?: unknown;
  conflux_api_key?: unknown;
  conflux_api_keys?: unknown;
  confluxApiKey?: unknown;
  max_consecutive_errors?: unknown;
  maxConsecutiveErrors?: unknown;
  max_errors?: unknown;
  maxErrors?: unknown;
  cooldown_ms?: unknown;
  cooldownMs?: unknown;
  cooldown_seconds?: unknown;
  cooldownSeconds?: unknown;
  cooldown_hours?: unknown;
  cooldownHours?: unknown;
  cooldown?: unknown;
  rotate_proxies_interval?: unknown;
  rotateProxiesInterval?: unknown;
  rotate_proxy_interval?: unknown;
  rotateProxyInterval?: unknown;
  proxy_rotate_interval?: unknown;
  proxyRotateInterval?: unknown;
  proxy_rotation_interval?: unknown;
  proxyRotationInterval?: unknown;
  rotate_interval?: unknown;
  rotateInterval?: unknown;
}

interface RawToml {
  port?: unknown;
  proxies?: unknown;
  max_consecutive_errors?: unknown;
  maxConsecutiveErrors?: unknown;
  max_errors?: unknown;
  maxErrors?: unknown;
  cooldown_ms?: unknown;
  cooldownMs?: unknown;
  cooldown_seconds?: unknown;
  cooldownSeconds?: unknown;
  cooldown_hours?: unknown;
  cooldownHours?: unknown;
  cooldown?: unknown;
  rotate_proxies_interval?: unknown;
  rotateProxiesInterval?: unknown;
  rotate_proxy_interval?: unknown;
  rotateProxyInterval?: unknown;
  proxy_rotate_interval?: unknown;
  proxyRotateInterval?: unknown;
  proxy_rotation_interval?: unknown;
  proxyRotationInterval?: unknown;
  rotate_interval?: unknown;
  rotateInterval?: unknown;
  providers?: Record<string, RawProvider>;
}

export function parseTomlConfig(content: string, filePath = "config.toml"): ConfluxConfig {
  const raw = Bun.TOML.parse(content) as unknown as RawToml;
  const envPort = process.env.PORT ? Number(process.env.PORT) : NaN;
  const parsedPort = typeof raw.port === "number" || typeof raw.port === "string" ? Number(raw.port) : NaN;
  const port = !isNaN(parsedPort) ? parsedPort : (!isNaN(envPort) ? envPort : 4010);

  const universalProxies: string[] = Array.isArray(raw.proxies)
    ? raw.proxies.map((p: unknown) => String(p).trim()).filter(Boolean)
    : [];

  const rawGlobalMaxErrors =
    raw.max_consecutive_errors ??
    raw.maxConsecutiveErrors ??
    raw.max_errors ??
    raw.maxErrors ??
    process.env.MAX_CONSECUTIVE_ERRORS;

  const globalMaxConsecutiveErrors = parseMaxConsecutiveErrors(rawGlobalMaxErrors, DEFAULT_MAX_CONSECUTIVE_ERRORS);

  let rawGlobalCooldown: unknown =
    raw.cooldown_ms ??
    raw.cooldownMs ??
    raw.cooldown_seconds ??
    raw.cooldownSeconds ??
    raw.cooldown_hours ??
    raw.cooldownHours ??
    raw.cooldown;

  if (rawGlobalCooldown === undefined) {
    if (process.env.COOLDOWN_MS) {
      rawGlobalCooldown = Number(process.env.COOLDOWN_MS);
    } else if (process.env.COOLDOWN_SECONDS) {
      rawGlobalCooldown = Number(process.env.COOLDOWN_SECONDS) * 1000;
    } else if (process.env.COOLDOWN_HOURS) {
      rawGlobalCooldown = Number(process.env.COOLDOWN_HOURS) * 60 * 60 * 1000;
    } else if (process.env.COOLDOWN) {
      rawGlobalCooldown = process.env.COOLDOWN;
    }
  }

  let globalCooldownMs = DEFAULT_COOLDOWN_MS;
  if (raw.cooldown_ms !== undefined || raw.cooldownMs !== undefined) {
    const val = Number(raw.cooldown_ms ?? raw.cooldownMs);
    if (!isNaN(val) && val > 0) globalCooldownMs = val;
  } else if (raw.cooldown_seconds !== undefined || raw.cooldownSeconds !== undefined) {
    const val = Number(raw.cooldown_seconds ?? raw.cooldownSeconds);
    if (!isNaN(val) && val > 0) globalCooldownMs = val * 1000;
  } else if (raw.cooldown_hours !== undefined || raw.cooldownHours !== undefined) {
    const val = Number(raw.cooldown_hours ?? raw.cooldownHours);
    if (!isNaN(val) && val > 0) globalCooldownMs = val * 60 * 60 * 1000;
  } else {
    globalCooldownMs = parseCooldownToMs(rawGlobalCooldown, DEFAULT_COOLDOWN_MS);
  }

  const rawGlobalRotateInterval =
    raw.rotate_proxies_interval ??
    raw.rotateProxiesInterval ??
    raw.rotate_proxy_interval ??
    raw.rotateProxyInterval ??
    raw.proxy_rotate_interval ??
    raw.proxyRotateInterval ??
    raw.proxy_rotation_interval ??
    raw.proxyRotationInterval ??
    raw.rotate_interval ??
    raw.rotateInterval ??
    process.env.ROTATE_PROXIES_INTERVAL ??
    process.env.PROXY_ROTATE_INTERVAL;

  const globalRotateProxiesInterval = parseRotateProxiesInterval(rawGlobalRotateInterval);

  const providers = new Map<string, ProviderConfig>();
  const clientKeyMap = new Map<string, ProviderConfig>();
  let defaultProvider: ProviderConfig | undefined = undefined;

  const rawProviders = raw.providers ?? {};
  for (const [name, p] of Object.entries(rawProviders)) {
    const rawUrl = typeof p.base_url === "string" ? p.base_url : (typeof p.baseUrl === "string" ? p.baseUrl : "https://api.openai.com");
    const baseUrl = rawUrl.replace(/\/$/, "");

    const rawApiKeys = p.api_keys ?? p.apiKeys;
    const rawKeys: string[] = Array.isArray(rawApiKeys)
      ? rawApiKeys.map((k: unknown) => String(k).trim()).filter(Boolean)
      : [];

    const sectionProxies: string[] = Array.isArray(p.proxies)
      ? p.proxies.map((pr: unknown) => String(pr).trim()).filter(Boolean)
      : [];

    let confluxKeys: string[] = [];
    const rawConfluxKeys = p.conflux_api_key ?? p.conflux_api_keys ?? p.confluxApiKey;
    if (Array.isArray(rawConfluxKeys)) {
      confluxKeys = rawConfluxKeys.map((k: unknown) => String(k).trim()).filter(Boolean);
    } else if (typeof rawConfluxKeys === "string" || typeof rawConfluxKeys === "number") {
      confluxKeys = [String(rawConfluxKeys).trim()];
    }

    const keys: KeyConfig[] = rawKeys.map((item) => {
      if (item.includes("|")) {
        const parts = item.split("|");
        const keyPart = parts[0]?.trim() ?? "";
        const proxyPart = parts.slice(1).join("|").trim();
        return {
          key: keyPart,
          proxy: proxyPart.length > 0 ? proxyPart : null,
        };
      }
      return { key: item, proxy: null };
    });

    const rawPMaxErrors =
      p.max_consecutive_errors ??
      p.maxConsecutiveErrors ??
      p.max_errors ??
      p.maxErrors;
    const providerMaxConsecutiveErrors = parseMaxConsecutiveErrors(rawPMaxErrors, globalMaxConsecutiveErrors);

    let providerCooldownMs = globalCooldownMs;
    if (p.cooldown_ms !== undefined || p.cooldownMs !== undefined) {
      const val = Number(p.cooldown_ms ?? p.cooldownMs);
      if (!isNaN(val) && val > 0) providerCooldownMs = val;
    } else if (p.cooldown_seconds !== undefined || p.cooldownSeconds !== undefined) {
      const val = Number(p.cooldown_seconds ?? p.cooldownSeconds);
      if (!isNaN(val) && val > 0) providerCooldownMs = val * 1000;
    } else if (p.cooldown_hours !== undefined || p.cooldownHours !== undefined) {
      const val = Number(p.cooldown_hours ?? p.cooldownHours);
      if (!isNaN(val) && val > 0) providerCooldownMs = val * 60 * 60 * 1000;
    } else if (p.cooldown !== undefined) {
      providerCooldownMs = parseCooldownToMs(p.cooldown, globalCooldownMs);
    }

    const rawPRotateInterval =
      p.rotate_proxies_interval ??
      p.rotateProxiesInterval ??
      p.rotate_proxy_interval ??
      p.rotateProxyInterval ??
      p.proxy_rotate_interval ??
      p.proxyRotateInterval ??
      p.proxy_rotation_interval ??
      p.proxyRotationInterval ??
      p.rotate_interval ??
      p.rotateInterval;
    const providerRotateProxiesInterval = parseRotateProxiesInterval(rawPRotateInterval) ?? globalRotateProxiesInterval;

    const providerConfig: ProviderConfig = {
      name,
      confluxApiKeys: confluxKeys,
      baseUrl,
      keys,
      proxies: sectionProxies,
      maxConsecutiveErrors: providerMaxConsecutiveErrors,
      cooldownMs: providerCooldownMs,
      rotateProxiesInterval: providerRotateProxiesInterval,
    };

    providers.set(name, providerConfig);

    for (const ck of confluxKeys) {
      if (ck === "*") {
        defaultProvider = providerConfig;
      } else {
        clientKeyMap.set(ck, providerConfig);
      }
    }

    if (name === "default" && !defaultProvider) {
      defaultProvider = providerConfig;
    }
  }

  return {
    port,
    universalProxies,
    maxConsecutiveErrors: globalMaxConsecutiveErrors,
    cooldownMs: globalCooldownMs,
    rotateProxiesInterval: globalRotateProxiesInterval,
    providers,
    clientKeyMap,
    defaultProvider,
    configPath: filePath,
  };
}

export async function loadConfig(customPath?: string): Promise<ConfluxConfig> {
  const targetPath = customPath ?? process.env.CONFIG_PATH ?? "config.toml";
  const file = Bun.file(targetPath);

  if (!(await file.exists())) {
    throw new Error(`Config file not found at: ${targetPath}`);
  }

  const content = await file.text();
  return parseTomlConfig(content, targetPath);
}
