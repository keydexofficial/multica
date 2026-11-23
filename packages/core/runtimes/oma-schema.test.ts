// @vitest-environment node
import { describe, it, expect } from "vitest";
import { parseOMACatalog, parseOMAConnection } from "./oma-schema";

describe("OMA Managed Agents response boundary", () => {
  it("disables configuration for malformed responses", () => {
    for (const input of [
      null,
      {},
      { available: "true" },
      { repositories: "bad" },
    ]) {
      expect(parseOMAConnection(input).available).toBe(false);
    }
  });

  it("keeps valid fields when optional fields are missing", () => {
    expect(
      parseOMAConnection({
        available: true,
        configured: true,
        name: "OMA",
        base_url: "http://oma.internal:38080/v1",
        repositories: null,
        vault_ids: null,
      }),
    ).toMatchObject({
      available: true,
      configured: true,
      name: "OMA",
      baseUrl: "http://oma.internal:38080/v1",
      repositories: [],
      vaultIds: [],
      enabled: false,
    });
  });

  it("maps fields and drops unexpected secrets", () => {
    const result = parseOMAConnection({
      configured: true,
      available: true,
      name: "OMA",
      base_url: "http://oma.internal:38080/v1",
      environment_id: "env_a",
      has_token: true,
      repositories: ["https://github.com/a/b"],
      vault_ids: ["vlt_a"],
      enabled: true,
      status: "online",
      last_error: "",
      api_token: "must-not-surface",
      github_tokens: { "https://github.com/a/b": "must-not-surface" },
    });
    expect(result.baseUrl).toBe("http://oma.internal:38080/v1");
    expect(result.environmentId).toBe("env_a");
    expect(result.vaultIds).toEqual(["vlt_a"]);
    expect(result.hasToken).toBe(true);
    expect(JSON.stringify(result)).not.toContain("must-not-surface");
  });

  it("maps execution limits and usage, defaulting malformed values", () => {
    expect(
      parseOMAConnection({
        available: true,
        max_concurrency: 5,
        run_timeout_minutes: 90,
        active_runs: 2,
        queued_runs: 1,
      }),
    ).toMatchObject({
      maxConcurrency: 5,
      runTimeoutMinutes: 90,
      activeRuns: 2,
      queuedRuns: 1,
    });
    expect(
      parseOMAConnection({
        available: true,
        max_concurrency: 0,
        run_timeout_minutes: "60",
        active_runs: -1,
      }),
    ).toMatchObject({
      available: true,
      maxConcurrency: 3,
      runTimeoutMinutes: 60,
      activeRuns: 0,
      queuedRuns: 0,
    });
  });
});

describe("OMA catalog boundary", () => {
  it("keeps identity and name only", () => {
    expect(
      parseOMACatalog({
        items: [{ id: "env_a", name: "Web", config: { token: "secret" } }],
      }),
    ).toEqual([{ id: "env_a", name: "Web" }]);
  });

  it("falls back to an empty catalog for malformed responses", () => {
    for (const malformed of [
      null,
      {},
      { items: "bad" },
      { items: [{ id: 1, name: "bad" }] },
      { items: [{ id: "", name: "empty id" }] },
    ]) {
      expect(parseOMACatalog(malformed)).toEqual([]);
    }
  });
});
