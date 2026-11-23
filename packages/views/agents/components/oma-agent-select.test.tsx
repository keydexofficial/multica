import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import enRuntimes from "../../locales/en/runtimes.json";
import { OMAAgentSelect } from "./oma-agent-select";

const state = vi.hoisted(() => ({
  agents: [] as { id: string; name: string }[],
  error: "",
  calls: 0,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-test" }));
vi.mock("@multica/core/runtimes/oma", () => ({
  omaAgentsOptions: () => ({
    queryKey: ["oma-agents-test"],
    queryFn: async () => {
      state.calls += 1;
      if (state.error) throw new Error(state.error);
      return state.agents;
    },
  }),
}));

afterEach(() => {
  state.agents = [];
  state.error = "";
  state.calls = 0;
});

function mount(value: string, onChange = vi.fn(), disabled = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { runtimes: enRuntimes } }}>
        <OMAAgentSelect value={value} onChange={onChange} disabled={disabled} />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { onChange, client };
}

describe("OMAAgentSelect", () => {
  it("shows the bound agent name and refreshes the catalog", async () => {
    state.agents = [{ id: "agent_a", name: "Reviewer" }];
    const { client } = mount("agent_a");
    const trigger = await screen.findByRole("combobox");
    await waitFor(() => expect(trigger).toHaveTextContent("Reviewer"));
    expect(trigger).not.toHaveTextContent("agent_a");
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(state.calls).toBe(2));
    client.clear();
  });

  it("warns when the bound agent disappeared from the catalog", async () => {
    state.agents = [{ id: "agent_a", name: "Reviewer" }];
    const { client } = mount("agent_missing");
    expect(
      await screen.findByText(
        "The linked OMA agent is no longer available.",
      ),
    ).toBeInTheDocument();
    client.clear();
  });

  it("disables selection when the catalog is empty", async () => {
    const { client } = mount("");
    expect(await screen.findByText("No OMA agents found.")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toBeDisabled();
    client.clear();
  });

  it("surfaces catalog errors", async () => {
    state.error = "OMA is unreachable";
    const { client } = mount("");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "OMA is unreachable",
    );
    client.clear();
  });

  it("does not allow refreshing while the viewer cannot edit", async () => {
    state.agents = [{ id: "agent_a", name: "Reviewer" }];
    const { client } = mount("agent_a", vi.fn(), true);
    expect(
      await screen.findByRole("button", { name: "Refresh" }),
    ).toBeDisabled();
    client.clear();
  });
});
