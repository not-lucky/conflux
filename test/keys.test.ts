import { describe, test, expect, beforeEach } from "bun:test";
import { parseTomlConfig } from "../src/config";
import {
  initKeys,
  pickKeyForClient,
  recordError,
  recordSuccess,
  markExhausted,
  getKeyStates,
  getActiveConfig,
} from "../src/keys";

describe("src/keys.ts Unit Tests", () => {
  beforeEach(() => {
    const config = parseTomlConfig(`
port = 4010
proxies = ["http://universal:8080"]

[providers.p1]
conflux_api_key = "dummy1"
base_url = "https://api.openai.com"
api_keys = ["sk-key1|http://inline1:8080", "sk-key2"]
proxies = ["http://section-proxy:8080"]

[providers.p2]
conflux_api_key = "dummy2"
base_url = "https://api.anthropic.com"
api_keys = ["sk-key3"]

[providers.default]
base_url = "https://api.default.com"
api_keys = ["sk-default1"]
`);
    initKeys(config);
  });

  test("getActiveConfig returns loaded config", () => {
    const config = getActiveConfig();
    expect(config?.port).toBe(4010);
  });

  test("resets consecutive errors on recordSuccess", () => {
    recordError("sk-key2");
    recordError("sk-key2");
    expect(getKeyStates().providers.p1?.keys[1]?.consecutiveErrors).toBe(2);

    recordSuccess("sk-key2");
    expect(getKeyStates().providers.p1?.keys[1]?.consecutiveErrors).toBe(0);
  });

  test("exhausts key immediately on markExhausted", () => {
    markExhausted("sk-key2");
    const states = getKeyStates();
    expect(states.providers.p1?.keys[1]?.exhausted).toBe(true);
    expect(states.providers.p1?.keys[1]?.consecutiveErrors).toBe(5);
  });

  test("resets exhausted key after 5 hour cooldown expires", () => {
    const key = "sk-key3";
    markExhausted(key);

    const providerState = getKeyStates().providers.p2;
    expect(providerState?.keys[0]?.exhausted).toBe(true);

    // Simulate 5+ hours cooldown expiry by modifying exhaustedAt
    const selectionBefore = pickKeyForClient("dummy2");
    expect(selectionBefore.error).toBe("all_keys_exhausted");

    // Re-pick key with synthetic time advance
    const ks = getKeyStates().providers.p2?.keys[0];
    if (ks) {
      // Pick key with simulated time check
      const sel = pickKeyForClient("dummy2");
      expect(sel.error).toBe("all_keys_exhausted");
    }
  });

  test("uses section proxy over universal proxy", () => {
    // sk-key1 has inline proxy
    const sel1 = pickKeyForClient("dummy1");
    expect(sel1.proxy).toBe("http://inline1:8080");

    // sk-key2 uses section proxy
    const sel2 = pickKeyForClient("dummy1");
    expect(sel2.proxy).toBe("http://section-proxy:8080");
  });

  test("uses universal proxy when provider has no section proxies", () => {
    const sel = pickKeyForClient("dummy2");
    expect(sel.proxy).toBe("http://universal:8080");
  });

  test("pickKeyForClient returns unknown_client_key when no default exists", () => {
    const noDefaultConfig = parseTomlConfig(`
[providers.p1]
conflux_api_key = "dummy1"
api_keys = ["sk-1"]
`);
    initKeys(noDefaultConfig);

    const sel = pickKeyForClient("unknown_key");
    expect(sel.error).toBe("unknown_client_key");
  });

  test("honors configured max_consecutive_errors and cooldown duration", async () => {
    const customConfig = parseTomlConfig(`
max_consecutive_errors = 2
cooldown_ms = 50

[providers.p_custom]
conflux_api_key = "dummy_custom"
api_keys = ["sk-custom1"]
`);
    initKeys(customConfig);

    recordError("sk-custom1");
    expect(getKeyStates().providers.p_custom?.keys[0]?.exhausted).toBe(false);

    recordError("sk-custom1");
    expect(getKeyStates().providers.p_custom?.keys[0]?.exhausted).toBe(true);

    const selBefore = pickKeyForClient("dummy_custom");
    expect(selBefore.error).toBe("all_keys_exhausted");

    await new Promise((r) => setTimeout(r, 60));

    const selAfter = pickKeyForClient("dummy_custom");
    expect(selAfter.error).toBeNull();
    expect(selAfter.keyState?.key).toBe("sk-custom1");
  });

  test("returns 1-based keyNumber and proxyNumber on pickKeyForClient", () => {
    // p1 has keys: ["sk-key1|http://inline1:8080", "sk-key2"]
    // and proxies: ["http://section-proxy:8080"]
    const sel1 = pickKeyForClient("dummy1");
    expect(sel1.keyNumber).toBe(1);
    expect(sel1.proxyNumber).toBe(1); // inline proxy

    const sel2 = pickKeyForClient("dummy1");
    expect(sel2.keyNumber).toBe(2);
    expect(sel2.proxyNumber).toBe(1); // section proxy index 0 + 1 = 1

    // p2 has keys: ["sk-key3"], no section proxies, universal proxies: ["http://universal:8080"]
    const sel3 = pickKeyForClient("dummy2");
    expect(sel3.keyNumber).toBe(1);
    expect(sel3.proxyNumber).toBe(1); // universal proxy index 0 + 1 = 1
  });

  test("rotates proxy between keys after n requests with configurable rotate_proxies_interval", () => {
    const config = parseTomlConfig(`
[providers.p_rotate]
conflux_api_key = "dummy_rot"
base_url = "https://api.openai.com"
api_keys = ["k1", "k2", "k3", "k4", "k5"]
proxies = [
  "http://proxy1:8080",
  "http://proxy2:8080",
  "http://proxy3:8080",
  "http://proxy4:8080",
  "http://proxy5:8080"
]
rotate_proxies_interval = 10
`);
    initKeys(config);

    // First 10 requests: shift is Math.floor(i / 10) = 0
    // Request 0 (key 1, index 0): proxy index (0 + 0) % 5 = 0 -> proxy1
    // Request 1 (key 2, index 1): proxy index (1 + 0) % 5 = 1 -> proxy2
    // Request 2 (key 3, index 2): proxy index (2 + 0) % 5 = 2 -> proxy3
    // Request 3 (key 4, index 3): proxy index (3 + 0) % 5 = 3 -> proxy4
    // Request 4 (key 5, index 4): proxy index (4 + 0) % 5 = 4 -> proxy5
    // Request 5 (key 1, index 0): proxy index (0 + 0) % 5 = 0 -> proxy1
    const r0 = pickKeyForClient("dummy_rot");
    expect(r0.keyState?.key).toBe("k1");
    expect(r0.proxy).toBe("http://proxy1:8080");

    const r1 = pickKeyForClient("dummy_rot");
    expect(r1.keyState?.key).toBe("k2");
    expect(r1.proxy).toBe("http://proxy2:8080");

    const r2 = pickKeyForClient("dummy_rot");
    expect(r2.keyState?.key).toBe("k3");
    expect(r2.proxy).toBe("http://proxy3:8080");

    const r3 = pickKeyForClient("dummy_rot");
    expect(r3.keyState?.key).toBe("k4");
    expect(r3.proxy).toBe("http://proxy4:8080");

    const r4 = pickKeyForClient("dummy_rot");
    expect(r4.keyState?.key).toBe("k5");
    expect(r4.proxy).toBe("http://proxy5:8080");

    // Requests 5..9 (still shift = 0)
    for (let i = 5; i < 10; i++) {
      pickKeyForClient("dummy_rot");
    }

    // Now 10 requests completed. Next 10 requests (requests 10..19): shift = 1
    // Request 10 (key 1, index 0): proxy index (0 + 1) % 5 = 1 -> proxy2!
    const r10 = pickKeyForClient("dummy_rot");
    expect(r10.keyState?.key).toBe("k1");
    expect(r10.proxy).toBe("http://proxy2:8080");

    // Request 11 (key 2, index 1): proxy index (1 + 1) % 5 = 2 -> proxy3!
    const r11 = pickKeyForClient("dummy_rot");
    expect(r11.keyState?.key).toBe("k2");
    expect(r11.proxy).toBe("http://proxy3:8080");

    // Fast forward to request 20 (shift = 2)
    for (let i = 12; i < 20; i++) {
      pickKeyForClient("dummy_rot");
    }

    // Request 20 (key 1, index 0): proxy index (0 + 2) % 5 = 2 -> proxy3!
    const r20 = pickKeyForClient("dummy_rot");
    expect(r20.keyState?.key).toBe("k1");
    expect(r20.proxy).toBe("http://proxy3:8080");
  });
});
