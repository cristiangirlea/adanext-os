import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { AdaError } from "./client/api";
import { FakeClient, makeTask } from "./test/fakeClient";

afterEach(cleanup);

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

describe("App", () => {
  it("lists every task with its state", async () => {
    const client = new FakeClient([
      makeTask({ id: "t1", objective: "fix the build", state: "draft" }),
      makeTask({ id: "t2", objective: "write the docs", state: "running", revision: 2 }),
      makeTask({ id: "t3", objective: "ship it", state: "awaiting_review", revision: 4 }),
    ]);
    render(<App client={client} />);
    await flush();
    const rows = screen.getAllByRole("row").filter((row) => row.getAttribute("data-task"));
    expect(rows.map((row) => row.getAttribute("data-task"))).toEqual(["t1", "t2", "t3"]);
    expect(within(rows[0]!).getByText("draft")).toBeTruthy();
    expect(within(rows[1]!).getByText("running")).toBeTruthy();
    expect(within(rows[2]!).getByText("awaiting_review")).toBeTruthy();
    expect(screen.getByText("fix the build")).toBeTruthy();
  });

  it("streams the selected task's attempt events in order and drops them on a reset", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "ready", revision: 1 })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await flush();
    expect(client.calls).toContain("start:claude:readonly");
    expect(client.calls).toContain("subscribe:attempt-t1");
    const push = client.listeners.get("attempt-t1")!;
    act(() => {
      push({ type: "status", status: "connected" });
      push({ type: "event", event: { seq: 1, kind: "accepted", payload: { ok: true } } });
      push({ type: "event", event: { seq: 2, kind: "progress", payload: "half" } });
      push({ type: "event", event: { seq: 3, kind: "result", payload: null } });
    });
    const items = screen.getAllByRole("listitem");
    expect(items.map((item) => item.getAttribute("data-seq"))).toEqual(["1", "2", "3"]);
    expect(items.map((item) => within(item).getByText(/^(accepted|progress|result)$/).textContent)).toEqual(["accepted", "progress", "result"]);
    act(() => {
      push({ type: "reset", last_sequence: 0 });
    });
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
    act(() => {
      push({ type: "event", event: { seq: 1, kind: "accepted", payload: {} } });
    });
    expect(screen.getAllByRole("listitem").map((item) => item.getAttribute("data-seq"))).toEqual(["1"]);
  });

  it("unsubscribes when another task is selected", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "ready", revision: 1 }), makeTask({ id: "t2" })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await flush();
    fireEvent.click(screen.getByText("objective t2"));
    expect(client.unsubscribed).toEqual(["attempt-t1"]);
  });

  it("shows a refusal as the server's error code", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "draft" })]);
    client.refuse.ready = new AdaError("invalid_transition", 409, "invalid transition");
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await flush();
    expect(screen.getByRole("alert").textContent).toContain("invalid_transition");
    expect(client.tasks[0]!.state).toBe("draft");
  });

  it("says the coordinator is unreachable when the client cannot reach it", async () => {
    const client = new FakeClient();
    client.refuse.listTasks = new AdaError("coordinator_unreachable", 0, "no pipe");
    render(<App client={client} />);
    await flush();
    expect(screen.getByRole("alert").textContent).toContain("coordinator unreachable");
  });

  it("creates a task and lists it", async () => {
    const client = new FakeClient();
    render(<App client={client} />);
    await flush();
    fireEvent.change(screen.getByLabelText("Project"), { target: { value: "repo" } });
    fireEvent.change(screen.getByLabelText("Objective"), { target: { value: "add a test" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await flush();
    expect(client.calls).toContain("createTask");
    const row = screen.getAllByRole("row").find((r) => r.getAttribute("data-task") === "t1")!;
    expect(within(row).getByText("add a test")).toBeTruthy();
    expect(within(row).getByText("draft")).toBeTruthy();
  });
});
