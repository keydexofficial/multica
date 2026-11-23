import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import enAgents from "../../locales/en/agents.json";
import { ChooseRuntimePage } from "./choose-runtime-page";

const state = vi.hoisted(() => ({
  push: vi.fn(),
  oma: {
    configured: true,
    available: true,
    name: "OMA",
    baseUrl: "http://oma.internal:38080/v1",
    environmentId: "env_test",
    hasToken: true,
    repositories: [],
    vaultIds: [],
    enabled: true,
    status: "online",
    lastError: "",
    maxConcurrency: 3,
    runTimeoutMinutes: 60,
    activeRuns: 0,
    queuedRuns: 0,
  },
  omaRuntime: { id: "oma-runtime", status: "online" } as
    | { id: string; status: string }
    | null,
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    const data: Record<string, unknown> = {
      runtimes: [],
      members: [{ user_id: "user-1", name: "Ada", role: "owner" }],
      qoder: null,
      claude: null,
      oma: state.oma,
      sessions: [],
    };
    return { data: data[queryKey[0] ?? ""], isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (value: unknown) => unknown) =>
    selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    agents: () => "/acme/agents",
    newAgentManual: () => "/acme/agents/new/manual",
    newAgentAiSession: (id: string) => `/acme/agents/new/ai/${id}`,
    runtimeDetail: (locator: string) => `/acme/runtimes/${locator}`,
  }),
}));
vi.mock("@multica/core/realtime", () => ({ useWSEvent: () => undefined }));
vi.mock("@multica/core/agents", () => ({
  agentBuilderSessionListOptions: () => ({ queryKey: ["sessions"] }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/runtimes/queries", () => ({
  runtimeKeys: { all: () => ["runtimes"] },
  runtimeListOptions: () => ({ queryKey: ["runtimes"] }),
}));
vi.mock("@multica/core/runtimes/qoder", () => ({
  qoderOptions: () => ({ queryKey: ["qoder"] }),
}));
vi.mock("@multica/core/runtimes/claude-managed", () => ({
  claudeManagedOptions: () => ({ queryKey: ["claude"] }),
}));
vi.mock("@multica/core/runtimes/oma", () => ({
  omaManagedOptions: () => ({ queryKey: ["oma"] }),
}));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    push: state.push,
    searchParams: new URLSearchParams(),
  }),
  useBackOrReplace: () => vi.fn(),
}));
vi.mock("../../runtimes/components/runtime-machines", () => ({
  buildRuntimeMachines: () => [],
}));
vi.mock("./runtime-choice", () => ({
  buildRuntimeChoiceGroups: () => ({
    local: null,
    remotes: [],
    hiddenRemoteCount: 0,
    cloud: [],
    qoderRuntime: null,
    claudeRuntime: null,
    omaRuntime: state.omaRuntime,
  }),
}));
vi.mock("./create-shell", () => ({
  AgentCreateShell: ({ children }: { children: ReactNode }) => (
    <div>{children}</div>
  ),
}));
vi.mock("./create-agent-footer", () => ({
  CreateFlowFooter: ({
    status,
    children,
  }: {
    status: ReactNode;
    children: ReactNode;
  }) => (
    <div>
      {status}
      {children}
    </div>
  ),
}));
vi.mock("./unfinished-drafts", () => ({
  UnfinishedDraftsBanner: () => null,
}));
vi.mock("../../runtimes/components/connect-remote-dialog", () => ({
  ConnectRemoteDialog: () => null,
}));
vi.mock("../../runtimes/components/remote-runtime-dialog", () => ({
  RemoteRuntimeDialog: () => null,
}));
vi.mock("../../runtimes/components/claude-managed-page", () => ({
  CLAUDE_MANAGED_LOCATOR: "claude-managed",
  ClaudeConnectDialog: () => null,
}));
vi.mock("../../runtimes/components/oma-managed-page", () => ({
  OMA_MANAGED_LOCATOR: "oma-managed",
  OMAConnectDialog: ({
    onConnected,
  }: {
    onConnected: () => void;
  }) => <button onClick={onConnected}>connected</button>,
}));
vi.mock("./runtime-choice-list", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("./runtime-choice-list")>();
  return {
    ...actual,
    RuntimeChoiceList: ({
      onSelectOMA,
      onConnectOMA,
      onManageOMA,
    }: {
      onSelectOMA: () => void;
      onConnectOMA: () => void;
      onManageOMA: () => void;
    }) => (
      <div>
        <button onClick={onSelectOMA}>select oma</button>
        <button onClick={onConnectOMA}>connect oma</button>
        <button onClick={onManageOMA}>manage oma</button>
      </div>
    ),
  };
});

afterEach(() => {
  state.push.mockClear();
  state.omaRuntime = { id: "oma-runtime", status: "online" };
});

function mount() {
  render(
    <I18nProvider locale="en" resources={{ en: { agents: enAgents } }}>
      <ChooseRuntimePage />
    </I18nProvider>,
  );
}

describe("ChooseRuntimePage OMA branch", () => {
  it("selects OMA after connecting and carries its runtime into the next step", async () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "connect oma" }));
    fireEvent.click(screen.getByRole("button", { name: "connected" }));
    expect(await screen.findByText("OMA Managed Agents")).toBeInTheDocument();
    const next = screen.getByRole("button", { name: "Next" });
    await waitFor(() => expect(next).toBeEnabled());
    fireEvent.click(next);
    expect(state.push).toHaveBeenCalledWith(
      "/acme/agents/new/manual?runtime=oma-runtime",
    );
  });

  it("keeps next disabled until the OMA runtime is online", () => {
    state.omaRuntime = null;
    mount();
    fireEvent.click(screen.getByRole("button", { name: "select oma" }));
    expect(screen.getByText("OMA Managed Agents")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
  });

  it("opens the OMA connection settings for management", () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "manage oma" }));
    expect(state.push).toHaveBeenCalledWith("/acme/runtimes/oma-managed");
  });
});
