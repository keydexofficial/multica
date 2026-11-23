import { vi, it, expect, beforeEach, afterEach, describe } from "vitest";
import {
  render,
  screen,
  fireEvent,
  cleanup,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { isManagedOMARuntime } from "@multica/core/runtimes/oma";
import type { AgentRuntime } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enLayout from "../../locales/en/layout.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { OMAManagedPage, isOMAManagedLocator } from "./oma-managed-page";

const { mocks, nav } = vi.hoisted(() => ({
  mocks: {
    getOMAConnection: vi.fn(),
    listMembers: vi.fn(),
    listRuntimes: vi.fn(),
    listAgents: vi.fn(),
    stopOMAConnection: vi.fn(),
    saveOMAConnection: vi.fn(),
    resumeOMAConnection: vi.fn(),
    disconnectOMAConnection: vi.fn(),
    listOMAEnvironments: vi.fn(),
    checkOMAConnection: vi.fn(),
  },
  nav: { push: vi.fn(), replace: vi.fn() },
}));
vi.mock("@multica/core/api", () => ({ api: mocks }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-test" }));
vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "owner" } };
  return {
    useAuthStore: Object.assign(
      (selector: (s: typeof state) => unknown) => selector(state),
      { getState: () => state },
    ),
  };
});
vi.mock("@multica/core/agents", () => ({
  useWorkspacePresenceMap: () => ({ byAgent: new Map() }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    runtimes: () => "/acme/runtimes",
    newAgentManual: () => "/acme/agents/new/manual",
    agentDetail: (id: string) => `/acme/agents/${id}`,
  }),
}));
vi.mock("../../navigation", () => ({
  useNavigation: () => nav,
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("./runtime-detail", () => ({
  ServingAgentList: ({ agents }: { agents: { id: string; name: string }[] }) => (
    <ul aria-label="serving agents">
      {agents.map((agent) => (
        <li key={agent.id}>{agent.name}</li>
      ))}
    </ul>
  ),
}));

const connection = {
  available: true,
  configured: true,
  name: "OMA Managed Agents",
  baseUrl: "http://oma.internal:38080/v1",
  environmentId: "env_one",
  hasToken: true,
  repositories: ["https://github.com/acme/web"],
  vaultIds: ["vlt_one"],
  enabled: true,
  status: "online",
  lastError: "",
  maxConcurrency: 3,
  runTimeoutMinutes: 60,
  activeRuns: 1,
  queuedRuns: 0,
};

const managedRuntime = {
  id: "rt_oma",
  workspace_id: "ws-test",
  provider: "oma_cloud",
  daemon_id: "oma-managed-ws-test",
  status: "online",
  visibility: "public",
};

beforeEach(() => {
  mocks.getOMAConnection.mockResolvedValue(connection);
  mocks.listMembers.mockResolvedValue([{ user_id: "owner", role: "owner" }]);
  mocks.listRuntimes.mockResolvedValue([managedRuntime]);
  mocks.listAgents.mockResolvedValue([
    { id: "a1", name: "Reviewer", runtime_id: "rt_oma", archived_at: null },
    { id: "a2", name: "Local helper", runtime_id: "rt_local", archived_at: null },
  ]);
  mocks.listOMAEnvironments.mockResolvedValue([
    { id: "env_one", name: "Web environment" },
    { id: "env_two", name: "Build environment" },
  ]);
  mocks.checkOMAConnection.mockResolvedValue(undefined);
  mocks.saveOMAConnection.mockResolvedValue(connection);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function mount() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, layout: enLayout, runtimes: enRuntimes } }}
    >
      <QueryClientProvider client={client}>
        <OMAManagedPage />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return client;
}

describe("OMA Managed Agents runtime locators", () => {
  const runtimes = [
    managedRuntime,
    {
      id: "rt_standalone",
      workspace_id: "ws-test",
      provider: "oma_cloud",
      daemon_id: "oma-bridge-abc",
    },
    { id: "rt_local", workspace_id: "ws-test", provider: "claude", daemon_id: "laptop" },
  ] as AgentRuntime[];

  it("recognises only the runtime the server manages", () => {
    expect(isManagedOMARuntime(runtimes[0]!)).toBe(true);
    expect(isManagedOMARuntime(runtimes[1]!)).toBe(false);
    expect(isManagedOMARuntime(runtimes[2]!)).toBe(false);
  });

  it("accepts the page locator and the managed runtime, not a standalone bridge", () => {
    expect(isOMAManagedLocator("oma-managed", runtimes)).toBe(true);
    expect(isOMAManagedLocator("rt_oma", runtimes)).toBe(true);
    expect(isOMAManagedLocator("rt_standalone", runtimes)).toBe(false);
    expect(isOMAManagedLocator("rt_local", runtimes)).toBe(false);
  });
});

it("shows the connection with its agents and no machine-level controls", async () => {
  const client = mount();
  expect(
    await screen.findByRole("heading", { name: "OMA Managed Agents" }),
  ).toBeInTheDocument();
  const serving = await screen.findByRole("list", { name: "serving agents" });
  expect(within(serving).getByText("Reviewer")).toBeInTheDocument();
  expect(within(serving).queryByText("Local helper")).not.toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Delete runtime" }),
  ).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create an agent" }));
  expect(nav.push).toHaveBeenCalledWith(
    "/acme/agents/new/manual?runtime=rt_oma",
  );
  client.clear();
});

it("explains when the server cannot run OMA Managed Agents", async () => {
  mocks.getOMAConnection.mockResolvedValue({ ...connection, available: false });
  const client = mount();
  expect(
    await screen.findByText(
      "Set MULTICA_VCS_SECRET_KEY on the server to connect OMA Managed Agents.",
    ),
  ).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create an agent" })).toBeNull();
  client.clear();
});

it("pauses the connection", async () => {
  mocks.stopOMAConnection.mockResolvedValue({ ...connection, enabled: false });
  const client = mount();
  fireEvent.click(await screen.findByRole("button", { name: "Pause" }));
  await waitFor(() => expect(mocks.stopOMAConnection).toHaveBeenCalledWith("ws-test"));
  client.clear();
});

it("resumes with the saved key and base URL", async () => {
  mocks.getOMAConnection.mockResolvedValue({
    ...connection,
    enabled: false,
    status: "offline",
  });
  mocks.resumeOMAConnection.mockResolvedValue(connection);
  const client = mount();
  fireEvent.click(await screen.findByRole("button", { name: "Resume" }));
  await waitFor(() => expect(mocks.resumeOMAConnection).toHaveBeenCalledTimes(1));
  expect(mocks.saveOMAConnection).not.toHaveBeenCalled();
  expect(mocks.resumeOMAConnection.mock.calls[0]![1]).toMatchObject({
    baseUrl: "http://oma.internal:38080/v1",
    environmentId: "env_one",
    apiToken: "",
    vaultIds: ["vlt_one"],
    maxConcurrency: 3,
    runTimeoutMinutes: 60,
  });
  client.clear();
});

it("disconnects after confirming the agents to unbind", async () => {
  mocks.disconnectOMAConnection.mockResolvedValue({
    ...connection,
    configured: false,
  });
  const client = mount();
  fireEvent.click(await screen.findByRole("button", { name: "Disconnect" }));
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).getByText("Reviewer")).toBeInTheDocument();
  expect(within(dialog).queryByText("Local helper")).not.toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect" }));
  await waitFor(() =>
    expect(mocks.disconnectOMAConnection).toHaveBeenCalledWith("ws-test", ["a1"]),
  );
  await waitFor(() => expect(nav.replace).toHaveBeenCalledWith("/acme/runtimes"));
  client.clear();
});

describe("connection form", () => {
  it("connects with a base URL and the only environment the new key can see", async () => {
    mocks.listOMAEnvironments.mockResolvedValue([
      { id: "env_only", name: "Only environment" },
    ]);
    mocks.getOMAConnection.mockResolvedValue({
      ...connection,
      configured: false,
      baseUrl: "",
      hasToken: false,
      environmentId: "",
      repositories: [],
      vaultIds: [],
    });
    const client = mount();
    const save = await screen.findByRole("button", { name: "Connect" });
    expect(save).toBeDisabled();
    fireEvent.change(screen.getByLabelText("API base URL"), {
      target: { value: "http://oma.internal:38080/v1" },
    });
    fireEvent.change(screen.getByLabelText("OMA API key"), {
      target: { value: "oma-secret" },
    });
    await waitFor(
      () => expect(mocks.listOMAEnvironments).toHaveBeenCalled(),
      { timeout: 3000 },
    );
    expect(mocks.listOMAEnvironments.mock.calls[0]![1]).toBe(
      "http://oma.internal:38080/v1",
    );
    expect(mocks.listOMAEnvironments.mock.calls[0]![2]).toBe("oma-secret");
    await waitFor(() => expect(save).toBeEnabled());
    fireEvent.click(save);
    await waitFor(() => expect(mocks.saveOMAConnection).toHaveBeenCalledTimes(1));
    expect(mocks.saveOMAConnection.mock.calls[0]![1]).toMatchObject({
      baseUrl: "http://oma.internal:38080/v1",
      environmentId: "env_only",
      apiToken: "oma-secret",
      vaultIds: [],
    });
    client.clear();
  });

  it("does not send the saved key to a changed base URL", async () => {
    const client = mount();
    const base = await screen.findByLabelText("API base URL");
    await waitFor(() => expect(mocks.listOMAEnvironments).toHaveBeenCalled());
    mocks.listOMAEnvironments.mockClear();
    fireEvent.change(base, {
      target: { value: "http://changed-oma:38080/v1" },
    });
    await new Promise((resolve) => setTimeout(resolve, 800));
    expect(mocks.listOMAEnvironments).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("OMA API key"), {
      target: { value: "new-key" },
    });
    await waitFor(
      () => expect(mocks.listOMAEnvironments).toHaveBeenCalled(),
      { timeout: 3000 },
    );
    expect(mocks.listOMAEnvironments.mock.calls.at(-1)![1]).toBe(
      "http://changed-oma:38080/v1",
    );
    expect(mocks.listOMAEnvironments.mock.calls.at(-1)![2]).toBe("new-key");
    client.clear();
  });

  it("tests the connection without saving it", async () => {
    const client = mount();
    const check = await screen.findByRole("button", { name: "Test connection" });
    await waitFor(() => expect(check).toBeEnabled());
    fireEvent.click(check);
    await waitFor(() => expect(mocks.checkOMAConnection).toHaveBeenCalledTimes(1));
    expect(mocks.saveOMAConnection).not.toHaveBeenCalled();
    expect(await screen.findByText("Connection works")).toBeInTheDocument();
    client.clear();
  });

  it("keeps the saved key when only the limits change", async () => {
    const client = mount();
    const concurrency = await screen.findByLabelText("Concurrent runs");
    fireEvent.change(concurrency, { target: { value: "5" } });
    const save = screen.getByRole("button", { name: "Save" });
    await waitFor(() => expect(save).toBeEnabled());
    fireEvent.click(save);
    await waitFor(() => expect(mocks.saveOMAConnection).toHaveBeenCalledTimes(1));
    expect(mocks.saveOMAConnection.mock.calls[0]![1]).toMatchObject({
      baseUrl: "http://oma.internal:38080/v1",
      apiToken: "",
      maxConcurrency: 5,
      environmentId: "env_one",
    });
    client.clear();
  });
});
