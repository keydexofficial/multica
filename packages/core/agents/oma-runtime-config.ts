export function omaAgentId(config: unknown): string {
  if (!config || typeof config !== "object" || !("oma_agent_id" in config))
    return "";
  return typeof config.oma_agent_id === "string" ? config.oma_agent_id : "";
}
