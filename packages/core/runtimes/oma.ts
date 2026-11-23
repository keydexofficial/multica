import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { api } from "../api";
import { runtimeKeys } from "./queries";
import { workspaceKeys } from "../workspace/queries";
import { agentTaskSnapshotKeys } from "../agents/queries";
import type { OMAConnection, OMAInput } from "./oma-schema";
import type { AgentRuntime } from "../types";
export type { OMAConnection, OMAInput } from "./oma-schema";

export const OMA_PROVIDER = "oma_cloud";
export const omaManagedDaemonId = (wsId: string) => `oma-managed-${wsId}`;

/** Whether a runtime is the one the server runs for the workspace's connection. */
export function isManagedOMARuntime(runtime: AgentRuntime): boolean {
  return (
    runtime.provider === OMA_PROVIDER &&
    runtime.daemon_id === omaManagedDaemonId(runtime.workspace_id)
  );
}

export const omaManagedKey = (wsId: string) => ["oma-managed", wsId] as const;
export const omaManagedOptions = (wsId: string) =>
  queryOptions({
    queryKey: omaManagedKey(wsId),
    queryFn: () => api.getOMAConnection(wsId),
    refetchInterval: 5000,
  });

function useConnectionMutation<T>(
  wsId: string,
  mutationFn: (input: T) => Promise<OMAConnection>,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn,
    onSuccess: (data) => {
      qc.setQueryData(omaManagedKey(wsId), data);
      void qc.invalidateQueries({ queryKey: ["oma-managed", wsId, "agents"] });
      void qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
    },
  });
}

export function useSaveOMA(wsId: string) {
  return useConnectionMutation(wsId, (input: OMAInput) =>
    api.saveOMAConnection(wsId, input),
  );
}
export function useResumeOMA(wsId: string) {
  return useConnectionMutation(wsId, (input: OMAInput) =>
    api.resumeOMAConnection(wsId, input),
  );
}
export function useStopOMA(wsId: string) {
  return useConnectionMutation(wsId, () => api.stopOMAConnection(wsId));
}
export function useCheckOMA(wsId: string) {
  return useMutation({
    mutationFn: (input: OMAInput) => api.checkOMAConnection(wsId, input),
  });
}

export function useDisconnectOMA(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (expectedActiveAgentIds: string[]) =>
      api.disconnectOMAConnection(wsId, expectedActiveAgentIds),
    onSuccess: (data) => {
      qc.setQueryData(omaManagedKey(wsId), data);
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: omaManagedKey(wsId) });
      void qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
      void qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) });
      void qc.invalidateQueries({ queryKey: agentTaskSnapshotKeys.all(wsId) });
    },
  });
}

export const omaAgentsOptions = (wsId: string) =>
  queryOptions({
    queryKey: ["oma-managed", wsId, "agents"],
    queryFn: () => api.listOMAAgents(wsId),
    staleTime: 30_000,
  });

export function useOMAEnvironments(wsId: string) {
  return useMutation({
    mutationFn: ({
      baseUrl,
      token,
      signal,
    }: {
      baseUrl: string;
      token: string;
      signal?: AbortSignal;
    }) => api.listOMAEnvironments(wsId, baseUrl, token, signal),
    gcTime: 0,
  });
}
