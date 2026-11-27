import { describe, expect, it } from "vitest";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ConfigError, loadConfig, validateConfig } from "../src/config.ts";

const validRaw = {
  mintUrl: "https://mint.example.com/",
  relays: ["wss://relay.example.com"],
  committeeGroupPubkey: "02" + "ab".repeat(32),
  validatorRoster: ["cd".repeat(32)],
  pollIntervalMs: 3000,
  clockToleranceSec: 45,
};

describe("validateConfig", () => {
  it("accepts a valid config and normalises the group pubkey to x-only", () => {
    const cfg = validateConfig(validRaw);
    expect(cfg.committeeGroupPubkey).toBe("ab".repeat(32));
    expect(cfg.validatorRoster).toEqual(["cd".repeat(32)]);
    expect(cfg.mintUrl).toBe("https://mint.example.com");
    expect(cfg.pollIntervalMs).toBe(3000);
    expect(cfg.clockToleranceSec).toBe(45);
  });

  it("applies defaults for the timing knobs", () => {
    const cfg = validateConfig({
      mintUrl: "http://127.0.0.1:3338",
      relays: ["ws://127.0.0.1:7000"],
      committeeGroupPubkey: "02" + "ab".repeat(32),
      validatorRoster: ["cd".repeat(32)],
    });
    expect(cfg.pollIntervalMs).toBeGreaterThan(0);
    expect(cfg.fallbackMarginSec).toBeGreaterThan(0);
  });

  it("refuses a missing mint, a bad relay, and a bad pubkey", () => {
    expect(() => validateConfig({ ...validRaw, mintUrl: "" })).toThrow(ConfigError);
    expect(() => validateConfig({ ...validRaw, relays: ["https://nope"] })).toThrow(ConfigError);
    expect(() => validateConfig({ ...validRaw, committeeGroupPubkey: "zz" })).toThrow(ConfigError);
    expect(() => validateConfig({ ...validRaw, validatorRoster: [] })).toThrow(ConfigError);
  });

  it("keeps secrets out of the schema: an unknown secret field is ignored", () => {
    const cfg = validateConfig({ ...validRaw, courierNsec: "nsec1leaked" });
    expect(Object.keys(cfg)).not.toContain("courierNsec");
  });
});

describe("loadConfig", () => {
  it("reads and validates a file, and names a bad file", async () => {
    const dir = await mkdtemp(join(tmpdir(), "sf-"));
    const good = join(dir, "good.json");
    await writeFile(good, JSON.stringify(validRaw));
    const cfg = await loadConfig(good);
    expect(cfg.relays).toEqual(["wss://relay.example.com"]);

    const bad = join(dir, "bad.json");
    await writeFile(bad, "{ not json");
    await expect(loadConfig(bad)).rejects.toThrow(ConfigError);
    await expect(loadConfig(join(dir, "missing.json"))).rejects.toThrow(ConfigError);
  });
});
