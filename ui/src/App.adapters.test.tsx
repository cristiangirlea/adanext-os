import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import type { AdaClient, BridgeApi } from "./client/api";
import { createBridgeClient } from "./client/adapters/bridge";
import { createHttpsClient } from "./client/adapters/https";
import { FakeAdad } from "./test/fakeAdad";
import { makeTask } from "./test/fakeClient";

// The workspace's component tests against both adapters with one fake adad (decisions log,
// 2026-10-14, #5): the desktop adapter through a fake preload bridge, the browser adapter through
// a fetch and EventSource stub serving the same state.

const shells: Record<string, (adad: FakeAdad) => AdaClient> = {
  bridge: (adad) => {
    (window as { ada?: BridgeApi }).ada = adad.bridge();
    return createBridgeClient();
  },
  https: (adad) => createHttpsClient({ fetch: adad.fetch, EventSource: adad.EventSource }),
};

afterEach(() => {
  cleanup();
  delete (window as { ada?: BridgeApi }).ada;
});

function seeded(): FakeAdad {
  const adad = new FakeAdad([
    makeTask({ id: "draft", project: "p1", objective: "a draft task" }),
    makeTask({ id: "run", project: "p1", objective: "a running task", state: "running", attempt: { id: "a1", state: "running" } }),
  ]);
  adad.projectList = [
    { id: "p1", name: "repo", role: "owner", created: "2026-11-24T00:00:00Z", default_mode: "readonly" },
    { id: "p2", name: "empty project", role: "member", created: "2026-11-24T00:00:00Z", default_mode: "readonly" },
  ];
  return adad;
}

describe.each(Object.keys(shells))("the workspace through the %s adapter", (shell) => {
  it("navigates every project from adad's list, the one with no task included, with each task under its project", async () => {
    const adad = seeded();
    render(<App client={shells[shell]!(adad)} />);
    const nav = screen.getByRole("navigation", { name: "Projects and tasks" });
    await within(nav).findByText("empty project");
    const tables = within(nav).getAllByRole("table");
    expect(tables.map((t) => t.querySelector("caption .project-name")?.textContent)).toEqual(["repo", "empty project"]);
    expect(within(tables[0]!).getByText("a draft task")).toBeTruthy();
    expect(within(tables[1]!).getByText("No tasks yet.")).toBeTruthy();
    expect(adad.requests.some((r) => r.url === "/v1/projects")).toBe(true);
  });

  it("shows the selected task", async () => {
    render(<App client={shells[shell]!(seeded())} />);
    fireEvent.click(await screen.findByText("a draft task"));
    const detail = screen.getByRole("main", { name: "Selected task" });
    expect(within(detail).getByRole("heading", { name: "a draft task" })).toBeTruthy();
    expect(within(detail).getByText("draft", { selector: ".state" })).toBeTruthy();
  });

  it("streams the selected attempt's events", async () => {
    const adad = seeded();
    render(<App client={shells[shell]!(adad)} />);
    fireEvent.click(await screen.findByText("a running task"));
    await waitFor(() => expect(adad.requests.some((r) => r.url.startsWith("/v1/attempts/a1/events"))).toBe(true));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      adad.emit("a1", 1, "text", "from the fake adad");
    });
    expect(await screen.findByText(/from the fake adad/)).toBeTruthy();
  });

  it("shows adad's refusal with its code and message", async () => {
    const adad = seeded();
    adad.refusals["POST /v1/tasks/draft/ready"] = { status: 403, body: { error: "not_on_network", message: "not available on the network yet" } };
    render(<App client={shells[shell]!(adad)} />);
    fireEvent.click(await screen.findByText("a draft task"));
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("not_on_network");
    expect(alert.textContent).toContain("not available in the browser yet");
    const sent = adad.requests.find((r) => r.method === "POST")!;
    expect(sent.headers["if-match"]).toBe('"0"');
    expect(sent.headers["idempotency-key"]).toBeTruthy();
  });
});
