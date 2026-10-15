import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
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
  it("shows queue position and allows withdrawal", async () => {
    const client = new FakeClient([makeTask({ id: "q", state: "preparing", revision: 2, attempt: { id: "aq", state: "pending", queue_position: 2 } })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective q"));
    expect(screen.getByText("Queue position 2")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await flush();
    expect(client.calls).toContain("cancel");
  });
  it("shows the stop result before asking the shell to wait and quit", async () => {
    const client = new FakeClient();
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Quit Ada and stop work" }));
    await flush();
    expect(screen.getByText(/coordinator stopped by the owner/)).toBeTruthy();
    expect(client.calls.indexOf("finishQuit")).toBeGreaterThan(client.calls.indexOf("stopCoordinator"));
  });
  it("subscribes on selecting a task started before this window opened", async () => {
    const client = new FakeClient([makeTask({ id: "old", state: "running", attempt: { id: "old-attempt", state: "running" } })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective old"));
    await flush();
    expect(client.calls).toContain("subscribe:old-attempt");
    act(() => client.listeners.get("old-attempt")!({ type: "event", event: { seq: 1, kind: "text", payload: "before opening" } }));
    expect(screen.getByText(/before opening/)).toBeTruthy();
    expect(client.calls.some((call) => call.startsWith("start:"))).toBe(false);
  });
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

  it("refreshes a started task from the coordinator as its attempt delivers events, so Cancel acts on the executor's revision", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "ready", revision: 1 })]);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await flush();
    const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
    expect(cancel.disabled).toBe(true);
    // The executor moves the task on its own; the UI only hears an engine event.
    client.tasks = client.tasks.map((t) => ({ ...t, state: "running", revision: 3 }));
    act(() => {
      client.listeners.get("attempt-t1")!({ type: "event", event: { seq: 1, kind: "accepted", payload: {} } });
    });
    const row = screen.getAllByRole("row").find((r) => r.getAttribute("data-task") === "t1")!;
    await waitFor(() => expect(within(row).getByText("running")).toBeTruthy());
    expect(client.calls).toContain("getTask");
    expect(screen.getByText(/revision 3/)).toBeTruthy();
    expect(cancel.disabled).toBe(false);
    fireEvent.click(cancel);
    await flush();
    expect(client.tasks[0]).toMatchObject({ state: "cancel_requested", revision: 4 });
    expect(within(row).getByText("cancel_requested")).toBeTruthy();
  });

  it("polls an active task until it reaches a terminal state, which the table then shows", async () => {
    vi.useFakeTimers();
    try {
      const client = new FakeClient([makeTask({ id: "t1", state: "running", revision: 2 }), makeTask({ id: "t2", state: "draft" })]);
      render(<App client={client} />);
      await flush();
      const polls = () => client.calls.filter((c) => c === "getTask").length;
      expect(polls()).toBe(0);
      client.tasks = client.tasks.map((t) => (t.id === "t1" ? { ...t, state: "failed", revision: 3 } : t));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2000);
      });
      const row = screen.getAllByRole("row").find((r) => r.getAttribute("data-task") === "t1")!;
      expect(within(row).getByText("failed")).toBeTruthy();
      const seen = polls();
      expect(seen).toBe(1);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(6000);
      });
      expect(polls()).toBe(seen);
    } finally {
      vi.useRealTimers();
    }
  });

  it("refreshes the selected task after a stale-revision refusal and when its stream reconnects", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "draft", revision: 0 })]);
    client.refuse.ready = new AdaError("stale_revision", 409, "stale revision");
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    client.tasks = [makeTask({ id: "t1", state: "ready", revision: 1 })];
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await waitFor(() => expect(screen.getByText(/revision 1/)).toBeTruthy());
    expect(screen.getByRole("alert").textContent).toContain("stale_revision");
    delete client.refuse.ready;
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await flush();
    expect(screen.getByText(/revision 2/)).toBeTruthy();
    client.tasks = client.tasks.map((t) => ({ ...t, state: "running", revision: 3 }));
    act(() => {
      client.listeners.get("attempt-t1")!({ type: "status", status: "connected" });
    });
    await waitFor(() => expect(screen.getByText(/revision 3/)).toBeTruthy());
  });

  it("shows the request id of an internal error so it can be matched to adad's log", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "draft" })]);
    client.refuse.ready = new AdaError("internal", 500, "internal error", "req-42");
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("objective t1"));
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await flush();
    expect(screen.getByRole("alert").textContent).toContain("req-42");
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
