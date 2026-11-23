// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { AgentRuntime } from "../types";
import { isManagedOMARuntime, omaManagedDaemonId } from "./oma";

function runtime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt_oma",
    workspace_id: "ws-test",
    provider: "oma_cloud",
    daemon_id: omaManagedDaemonId("ws-test"),
    ...overrides,
  } as AgentRuntime;
}

describe("OMA managed runtime identity", () => {
  it("matches only the managed daemon", () => {
    expect(isManagedOMARuntime(runtime())).toBe(true);
    expect(isManagedOMARuntime(runtime({ daemon_id: "oma-bridge-abc" }))).toBe(false);
    expect(isManagedOMARuntime(runtime({ provider: "claude_managed" }))).toBe(false);
  });
});
