import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import type { AdaClient, AttemptDecision, BridgeApi, SignedInAccount, Task } from "./client/api";
import { createBridgeClient } from "./client/adapters/bridge";
import { createHttpsClient } from "./client/adapters/https";
import { FakeAdad } from "./test/fakeAdad";
import { FakeClient, makeTask } from "./test/fakeClient";
import { AdaError } from "./client/api";

// The permission decision on the task view and the owner's confirm-first stop, through both
// adapters with one fake adad. On the desktop the viewer is the owner account; in the browser the
// signed-in account. Who may allow comes from the decision's actor, never from the task's creator.

const owner: SignedInAccount = { id: "u1", name: "sam", owner: true, password_allowed: false };
const other: SignedInAccount = { id: "u2", name: "kim", owner: false, password_allowed: false };

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

const waiting: AttemptDecision = { actor: "u1", required: true, scope: { backend: "claude", mode: "auto", workspace: "w1" }, project_default: "readonly", allowed: false, denied: false };

function pending(fields: Partial<Task> = {}, decision: AttemptDecision | null = waiting): Task {
  return makeTask({ id: "t1", objective: "edit the parser", state: "preparing", revision: 3, created_by: "u9", attempt: { id: "a1", state: "pending", backend: "claude", model: "", effort: "", mode: "auto", decision }, ...fields });
}

function seeded(task: Task, role = "owner"): FakeAdad {
  const adad = new FakeAdad([task]);
  adad.projectList = [{ id: "p1", name: "repo", role, created: "", default_mode: "readonly" }];
  adad.accountList = [{ id: "u1", name: "sam", owner: true, disabled: false, password_allowed: false, credentials: 1, sessions: 0, created: "" }];
  return adad;
}

// The viewer is the owner account on both shells unless an account is named.
function show(shell: string, adad: FakeAdad, account: SignedInAccount = owner) {
  return render(<App client={shells[shell]!(adad)} account={shell === "https" ? account : undefined} />);
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

describe.each(Object.keys(shells))("a permission decision through the %s adapter", (shell) => {
  it("shows the actor the scope with Allow and Deny, and an allow is sent and read back", async () => {
    const adad = seeded(pending());
    show(shell, adad);
    fireEvent.click(await screen.findByText("edit the parser"));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    expect(panel.textContent).toContain("claude");
    expect(panel.textContent).toContain("auto");
    expect(panel.textContent).toContain("readonly");
    fireEvent.click(await within(panel).findByRole("button", { name: "Allow" }));
    await waitFor(() => expect(adad.requests.some((r) => r.url === "/v1/attempts/a1/decision")).toBe(true));
    const sent = adad.requests.find((r) => r.url === "/v1/attempts/a1/decision")!;
    expect(sent.method).toBe("POST");
    expect(JSON.parse(sent.body)).toEqual({ decision: "allow" });
    expect(sent.headers["idempotency-key"]).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("region", { name: "Waiting for your decision" })).toBeNull());
  });

  it("offers a project owner who is not the actor only Deny, which cancels the attempt", async () => {
    const adad = seeded(pending({ created_by: "u1" }, { ...waiting, actor: "u7" }));
    show(shell, adad);
    fireEvent.click(await screen.findByText("edit the parser"));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    await settle();
    expect(within(panel).queryByRole("button", { name: "Allow" })).toBeNull();
    fireEvent.click(within(panel).getByRole("button", { name: "Deny" }));
    await waitFor(() => expect(adad.tasks[0]!.state).toBe("canceled"));
    expect(JSON.parse(adad.requests.find((r) => r.url === "/v1/attempts/a1/decision")!.body)).toEqual({ decision: "deny" });
    await waitFor(() => expect(screen.queryByRole("region", { name: "Waiting for your decision" })).toBeNull());
  });

  it("shows nothing when adad sends no decision for the viewer", async () => {
    show(shell, seeded(pending({}, null)));
    fireEvent.click(await screen.findByText("edit the parser"));
    await settle();
    expect(screen.queryByRole("region", { name: "Waiting for your decision" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Allow" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
  });

  it("shows adad's refusal of a decision", async () => {
    const adad = seeded(pending());
    adad.refusals["POST /v1/attempts/a1/decision"] = { status: 409, body: { error: "invalid_transition", message: "invalid transition" } };
    show(shell, adad);
    fireEvent.click(await screen.findByText("edit the parser"));
    fireEvent.click(within(await screen.findByRole("region", { name: "Waiting for your decision" })).getByRole("button", { name: "Deny" }));
    expect((await screen.findByRole("alert")).textContent).toContain("invalid_transition");
  });
});

// The network takes decisions from the coordinator's owner account only, until M4b.6.
describe("another account's decision in the browser", () => {
  it("shows a project owner that is not the coordinator's owner the decision with no controls", async () => {
    const adad = seeded(pending({ created_by: "u2" }, { ...waiting, actor: "u7" }), "owner");
    show("https", adad, other);
    fireEvent.click(await screen.findByText("edit the parser"));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    await settle();
    expect(within(panel).queryByRole("button", { name: "Allow" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: "Deny" })).toBeNull();
  });

  it("shows a member who is the attempt's actor the decision with no controls", async () => {
    const adad = seeded(pending({}, { ...waiting, actor: "u2" }), "member");
    show("https", adad, other);
    fireEvent.click(await screen.findByText("edit the parser"));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    await settle();
    expect(within(panel).queryByRole("button", { name: "Allow" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: "Deny" })).toBeNull();
  });
});

const stopLabel: Record<string, string> = { bridge: "Quit Ada and stop work", https: "Stop Ada's work" };
const confirmLabel: Record<string, string> = { bridge: "Stop work and quit", https: "Stop work" };

describe.each(Object.keys(shells))("the owner's stop through the %s adapter", (shell) => {
  it("shows the counts first, stops only once confirmed, with the count the owner saw", async () => {
    const adad = seeded(pending());
    adad.stopCounts = { running: 2, queued: 1, others_running: 1, others_queued: 1 };
    show(shell, adad);
    fireEvent.click(await screen.findByRole("button", { name: stopLabel[shell] }));
    const ask = await screen.findByRole("group", { name: "Stop work" });
    expect(ask.textContent).toContain("1 task of other users is running and 1 is queued.");
    expect(ask.textContent).toContain("coordinator stopped by the owner");
    expect(adad.requests.filter((r) => r.url === "/v1/coordinator/stop").map((r) => r.method)).toEqual(["GET"]);
    expect(adad.stopped).toBe(false);
    fireEvent.click(within(ask).getByRole("button", { name: confirmLabel[shell] }));
    await waitFor(() => expect(adad.stopped).toBe(true));
    const post = adad.requests.find((r) => r.method === "POST" && r.url === "/v1/coordinator/stop")!;
    expect(JSON.parse(post.body)).toEqual({ others: 2 });
    expect(post.headers["idempotency-key"]).toBeTruthy();
    expect(await screen.findByText(/2 running cancellation requested, 1 queued canceled/)).toBeTruthy();
    if (shell === "bridge") await waitFor(() => expect(adad.finished).toBe(true));
    else expect(adad.finished).toBe(false);
  });

  it("stops nothing when the owner keeps working", async () => {
    const adad = seeded(pending());
    show(shell, adad);
    fireEvent.click(await screen.findByRole("button", { name: stopLabel[shell] }));
    fireEvent.click(within(await screen.findByRole("group", { name: "Stop work" })).getByRole("button", { name: "Keep working" }));
    expect(screen.queryByRole("group", { name: "Stop work" })).toBeNull();
    await settle();
    expect(adad.requests.some((r) => r.method === "POST" && r.url === "/v1/coordinator/stop")).toBe(false);
    expect(adad.stopped).toBe(false);
  });

  it("shows the new counts and asks again when they changed before the stop", async () => {
    const adad = seeded(pending());
    adad.stopCounts = { running: 0, queued: 0, others_running: 0, others_queued: 0 };
    show(shell, adad);
    fireEvent.click(await screen.findByRole("button", { name: stopLabel[shell] }));
    const ask = await screen.findByRole("group", { name: "Stop work" });
    expect(ask.textContent).toContain("No task of other users is running or queued.");
    adad.stopCounts = { running: 3, queued: 0, others_running: 2, others_queued: 0 };
    fireEvent.click(within(ask).getByRole("button", { name: confirmLabel[shell] }));
    await waitFor(() => expect(screen.getByRole("group", { name: "Stop work" }).textContent).toContain("2 tasks of other users are running and 0 are queued."));
    expect(screen.getByRole("group", { name: "Stop work" }).textContent).toContain("changed");
    expect(adad.stopped).toBe(false);
    expect(adad.finished).toBe(false);
    fireEvent.click(within(screen.getByRole("group", { name: "Stop work" })).getByRole("button", { name: confirmLabel[shell] }));
    await waitFor(() => expect(adad.stopped).toBe(true));
    expect(adad.requests.filter((r) => r.method === "POST" && r.url === "/v1/coordinator/stop").map((r) => JSON.parse(r.body))).toEqual([{ others: 0 }, { others: 2 }]);
  });
});

describe("the browser for the owner account", () => {
  it("shows the create-task and start controls to the owner account", async () => {
    const adad = seeded(makeTask({ id: "t2", objective: "a ready task", state: "ready" }));
    show("https", adad, owner);
    expect(await screen.findByRole("button", { name: "New task" })).toBeTruthy();
    fireEvent.click(await screen.findByText("a ready task"));
    expect(screen.getByRole("button", { name: "Start" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop Ada's work" })).toBeTruthy();
  });

  it("keeps them hidden from every other account", async () => {
    const adad = seeded(makeTask({ id: "t2", objective: "a ready task", state: "ready" }), "member");
    show("https", adad, other);
    fireEvent.click(await screen.findByText("a ready task"));
    for (const name of ["New task", "Start", "Stop Ada's work", "Quit Ada and stop work"]) expect(screen.queryByRole("button", { name })).toBeNull();
  });
});

describe("the browser owner's new task on a snapshot", () => {
  it("takes a snapshot of the project, creates the task on it, readies it and starts it, with the decision when above the default", async () => {
    const adad = seeded(makeTask({ id: "t0", objective: "an older task", state: "accepted" }));
    adad.signedIn = owner;
    adad.refusals["GET /v1/projects/p1/untracked"] = { status: 200, body: ["notes.txt", ".env"] };
    show("https", adad, owner);
    fireEvent.click(await screen.findByRole("button", { name: "New task" }));
    const take = await screen.findByRole("group", { name: "Snapshot" });
    fireEvent.change(within(take).getByLabelText("Project"), { target: { value: "p1" } });
    const credential = await within(take).findByRole("checkbox", { name: /\.env/ });
    expect((credential as HTMLInputElement).checked).toBe(false);
    fireEvent.click(credential);
    fireEvent.click(within(take).getByRole("button", { name: "Take a snapshot" }));
    await waitFor(() => expect(adad.requests.some((r) => r.url === "/v1/projects/p1/snapshots")).toBe(true));
    const snap = adad.requests.find((r) => r.url === "/v1/projects/p1/snapshots")!;
    expect(snap.method).toBe("POST");
    expect(JSON.parse(snap.body)).toEqual({ untracked: ["notes.txt", ".env"], include_sensitive: [".env"], include_changes: true });
    expect(snap.headers["idempotency-key"]).toBeTruthy();
    const form = await screen.findByRole("form", { name: "New task" });
    expect(form.textContent).toContain("from snapshot snap-1");
    fireEvent.change(within(form).getByLabelText("Objective"), { target: { value: "fix the parser" } });
    fireEvent.click(within(form).getByRole("radio", { name: /^auto/ }));
    fireEvent.click(within(form).getByRole("button", { name: "Create" }));
    await screen.findByRole("heading", { name: "fix the parser" });
    expect(JSON.parse(adad.requests.find((r) => r.method === "POST" && r.url === "/v1/tasks")!.body)).toMatchObject({ project: "p1", objective: "fix the parser", snapshot: "snap-1" });
    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await waitFor(() => expect((screen.getByRole("button", { name: "Start" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    expect(within(panel).getByRole("button", { name: "Allow" })).toBeTruthy();
    expect(JSON.parse(adad.requests.find((r) => r.url.endsWith("/start"))!.body)).toMatchObject({ mode: "auto" });
  });

  it("offers the browser owner both import and snapshot", async () => {
    show("https", seeded(makeTask({ id: "t0", objective: "an older task", state: "accepted" })), owner);
    fireEvent.click(await screen.findByRole("button", { name: "New task" }));
    const start = (await screen.findByRole("heading", { name: "New task" })).parentElement!;
    expect(start.textContent).toContain("Choose a project");
    expect(screen.getByRole("button", { name: "Import a repository" })).toBeTruthy();
  });

  it("shows a refused snapshot with its blockers and creates nothing", async () => {
    const adad = seeded(makeTask({ id: "t0", objective: "an older task", state: "accepted" }));
    adad.refusals["POST /v1/projects/p1/snapshots"] = { status: 409, body: { error: "import_blocked", message: "the repository cannot be captured as it stands", blockers: [{ code: "unmerged_paths", paths: ["src/a.go"], count: 1 }] } };
    show("https", adad, owner);
    fireEvent.click(await screen.findByRole("button", { name: "New task" }));
    const take = await screen.findByRole("group", { name: "Snapshot" });
    fireEvent.click(within(take).getByRole("button", { name: "Take a snapshot" }));
    expect((await screen.findByRole("alert")).textContent).toContain("import_blocked");
    expect(screen.getByRole("group", { name: "Blockers" }).textContent).toContain("src/a.go");
    expect(screen.queryByRole("form", { name: "New task" })).toBeNull();
    expect(adad.requests.some((r) => r.method === "POST" && r.url === "/v1/tasks")).toBe(false);
  });

  it("the desktop adapter has no project snapshot: the folder handle stays its way in", async () => {
    const adad = seeded(makeTask({ id: "t0" }));
    const client = shells.bridge!(adad);
    await expect(client.snapshotProject("p1", { untracked: [], include_sensitive: [] })).rejects.toMatchObject({ code: "network_only" });
    expect(adad.requests).toEqual([]);
  });
});

describe("the desktop's own-account lookup", () => {
  it("shows a failed lookup, keeps no id from it, and tries again", async () => {
    const client = new FakeClient([pending()]);
    client.projectList = [{ id: "p1", name: "repo", role: "owner", created: "", default_mode: "readonly" }];
    client.accountList = [{ id: "u1", name: "sam", owner: true, disabled: false, password_allowed: false, credentials: 1, sessions: 0, created: "" }];
    client.refuse.listAccounts = new AdaError("coordinator_unreachable", 0, "adad is not reachable");
    render(<App client={client} />);
    fireEvent.click(await screen.findByText("edit the parser"));
    const panel = await screen.findByRole("region", { name: "Waiting for your decision" });
    expect(await within(panel).findByText(/coordinator_unreachable/)).toBeTruthy();
    expect(within(panel).queryByRole("button", { name: "Allow" })).toBeNull();
    delete client.refuse.listAccounts;
    fireEvent.click(within(panel).getByRole("button", { name: "Try again" }));
    expect(await within(panel).findByRole("button", { name: "Allow" })).toBeTruthy();
    expect(client.calls.filter((c) => c === "listAccounts")).toHaveLength(2);
    expect(within(panel).queryByText(/coordinator_unreachable/)).toBeNull();
  });
});
