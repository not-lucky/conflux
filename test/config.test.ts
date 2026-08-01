import { describe, test, expect } from "bun:test";
import { parseTomlConfig, loadConfig } from "../src/config";

describe("src/config.ts Unit Tests", () => {
  test("parses port from process.env when missing in TOML", () => {
    const origPort = process.env.PORT;
    process.env.PORT = "9090";

    const config = parseTomlConfig(`
[providers.p1]
conflux_api_key = "k1"
api_keys = ["sk-1"]
`);

    expect(config.port).toBe(9090);
    process.env.PORT = origPort;
  });

  test("parses wildcard conflux_api_key '*' as default provider", () => {
    const config = parseTomlConfig(`
[providers.wildcard_section]
conflux_api_key = "*"
api_keys = ["sk-wildcard"]
`);

    expect(config.defaultProvider?.name).toBe("wildcard_section");
  });

  test("throws error when config file does not exist", () => {
    expect(loadConfig("non_existent_file_12345.toml")).rejects.toThrow("Config file not found");
  });

  test("loads config successfully from file", async () => {
    const tempPath = "test-temp-config.toml";
    const sampleToml = `
port = 4010
[providers.p1]
conflux_api_key = "k1"
api_keys = ["sk-1"]
`;
    await Bun.write(tempPath, sampleToml);
    try {
      const config = await loadConfig(tempPath);
      expect(config.port).toBe(4010);
      expect(config.providers.size).toBe(1);
    } finally {
      const file = Bun.file(tempPath);
      if (await file.exists()) {
        await file.unlink();
      }
    }
  });

  test("parses custom max_consecutive_errors and cooldown settings", () => {
    const config = parseTomlConfig(`
max_consecutive_errors = 3
cooldown = "10m"

[providers.custom_p1]
conflux_api_key = "k1"
api_keys = ["sk-1"]

[providers.custom_p2]
conflux_api_key = "k2"
api_keys = ["sk-2"]
max_consecutive_errors = 2
cooldown_seconds = 60
`);

    expect(config.maxConsecutiveErrors).toBe(3);
    expect(config.cooldownMs).toBe(10 * 60 * 1000);

    const p1 = config.providers.get("custom_p1");
    expect(p1?.maxConsecutiveErrors).toBe(3);
    expect(p1?.cooldownMs).toBe(10 * 60 * 1000);

    const p2 = config.providers.get("custom_p2");
    expect(p2?.maxConsecutiveErrors).toBe(2);
    expect(p2?.cooldownMs).toBe(60 * 1000);
  });

  test("parses rotate_proxies_interval at global and provider level", () => {
    const config = parseTomlConfig(`
rotate_proxies_interval = 10

[providers.p1]
conflux_api_key = "k1"
api_keys = ["sk-1"]

[providers.p2]
conflux_api_key = "k2"
api_keys = ["sk-2"]
proxy_rotate_interval = 5
`);

    expect(config.rotateProxiesInterval).toBe(10);

    const p1 = config.providers.get("p1");
    expect(p1?.rotateProxiesInterval).toBe(10);

    const p2 = config.providers.get("p2");
    expect(p2?.rotateProxiesInterval).toBe(5);
  });
});
