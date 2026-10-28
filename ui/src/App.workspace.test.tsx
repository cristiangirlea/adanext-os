import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { AdaError, type Changes } from "./client/api";
import { FakeClient, makeTask } from "./test/fakeClient";

// The workspace of M4.4b through the App: the import flow, the work area, the review and the
// status bar, against the fake client.

afterEach(cleanup);

async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

function dialog(): HTMLElement {
  return screen.getByRole("dialog", { name: "Import a repository" });
}

describe("the import flow", () => {
  it("chooses through the shell's dialog, inspects, offers the changes with credentials marked, snapshots, then creates the task readonly", async () => {
    const client = new FakeClient();
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
    const d = dialog();
    // The page has no field for a path anywhere in the flow.
    expect(within(d).queryByLabelText(/path|folder/i)).toBeNull();
    fireEvent.click(within(d).getByRole("button", { name: "Choose a folder" }));
    await flush();
    expect(client.calls.slice(-2)).toEqual(["chooseFolder", "inspectFolder"]);
    expect(client.args.inspectFolder).toEqual([["h1"]]);

    expect(within(d).getByRole("group", { name: "Provider configuration" }).textContent).toContain(".claude/settings.json");
    expect(within(d).getByText("Include these changes in the new feature branch")).toBeTruthy();
    const box = (path: string) => within(d).getByText(path).closest("label")!.querySelector("input") as HTMLInputElement;
    expect(box("notes.txt").checked).toBe(true);
    expect(box(".env").checked).toBe(false);
    expect(box("keys/id_ed25519").checked).toBe(false);
    expect(within(d).getAllByText("looks like a credential")).toHaveLength(2);
    fireEvent.click(box(".env"));

    fireEvent.click(within(d).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    expect(client.args.importFolder).toEqual([["h1", "repo"]]);
    expect(client.args.snapshot).toEqual([["h1", { untracked: ["notes.txt", ".env"], include_sensitive: [".env"], include_changes: true }]]);

    expect((within(d).getByRole("radio", { name: /readonly/ }) as HTMLInputElement).checked).toBe(true);
    expect(within(d).getByText("Checks are not sandboxed.")).toBeTruthy();
    fireEvent.change(within(d).getByLabelText("Objective"), { target: { value: "fix the build" } });
    fireEvent.change(within(d).getByLabelText("Acceptance criteria"), { target: { value: "tests pass" } });
    fireEvent.click(within(d).getByRole("button", { name: "Add a check" }));
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "unit" } });
    fireEvent.change(within(d).getByLabelText("Command"), { target: { value: 'go test "./..."' } });
    fireEvent.click(within(d).getByRole("button", { name: "Create" }));
    await flush();
    expect(client.args.createTask).toEqual([[{ project: "p-new", objective: "fix the build", criteria: "tests pass", snapshot: "snap1", checks: [{ argv: ["go", "test", "./..."], name: "unit" }] }]]);
    expect(screen.queryByRole("dialog", { name: "Import a repository" })).toBeNull();
    expect(screen.getByRole("heading", { level: 2, name: "fix the build" })).toBeTruthy();
    // The navigation shows the project under the name it was imported with.
    expect(screen.getByText("repo")).toBeTruthy();
    expect(screen.getByText("ada/t1")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Ready" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Start" }));
    await flush();
    expect(client.args.start![0]![2]).toEqual({ backend: "claude", model: "", effort: "", mode: "readonly" });
    expect(screen.getByText("claude, default model, readonly")).toBeTruthy();
  });

  it("shows the blockers with their paths and offers no import", async () => {
    const client = new FakeClient();
    client.inspection = { ...client.inspection, blockers: [{ code: "escaping_link", paths: ["vendor/out", "x\u202ey"], count: 3 }] };
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Choose a folder" }));
    await flush();
    const blockers = within(dialog()).getByRole("group", { name: "Blockers" });
    expect(within(blockers).getByText("A link points outside the repository")).toBeTruthy();
    expect(within(blockers).getByText("vendor/out")).toBeTruthy();
    expect(within(blockers).getByText("U+202E")).toBeTruthy();
    expect(within(blockers).getByText("and 1 more")).toBeTruthy();
    expect(within(dialog()).queryByRole("button", { name: "Import and take the snapshot" })).toBeNull();
  });

  it("continues with the project already holding the repository", async () => {
    const client = new FakeClient();
    client.refuse.importFolder = new AdaError("already_imported", 409, "this repository is already imported", undefined, { project: "p-old" });
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Choose a folder" }));
    await flush();
    fireEvent.click(within(dialog()).getByRole("button", { name: "Import and take the snapshot" }));
    await flush();
    expect(within(dialog()).getByText(/already imported as project p-old/)).toBeTruthy();
    expect(client.args.snapshot).toHaveLength(1);
    fireEvent.change(within(dialog()).getByLabelText("Objective"), { target: { value: "more" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Create" }));
    await flush();
    expect(client.args.createTask![0]![0]).toMatchObject({ project: "p-old", snapshot: "snap1" });
  });

  it("does nothing more when the dialog is closed without a folder", async () => {
    const client = new FakeClient();
    client.folder = null;
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Choose a folder" }));
    await flush();
    expect(client.calls).not.toContain("inspectFolder");
    expect(within(dialog()).getByRole("button", { name: "Choose a folder" })).toBeTruthy();
  });
});

function reviewable(client: FakeClient, changes: Partial<Changes> = {}): void {
  const commit = "c".repeat(40);
  client.tasks = [makeTask({ id: "t1", objective: "fix it", state: "awaiting_review", revision: 7, snapshot: "s1", attempt: { id: "a1", state: "ended_ok", tested_commit: commit } })];
  client.changeSets.t1 = {
    task: "t1",
    attempt: "a1",
    base: "b".repeat(40),
    commit,
    tip: commit,
    stale: false,
    generation: 4,
    files: [{ path: "index.html", status: "M", old_mode: "100644", new_mode: "100644", binary: false, added: 1, deleted: 1, patch: "@@ -1 +1 @@\n-<p>a</p>\n+<p onclick=x>b</p>\n", patch_truncated: false, provider: false }],
    total: 1,
    files_truncated: false,
    bytes_truncated: false,
    total_capped: false,
    provider_changes: null,
    provider_flagged: false,
    checks: [],
    ...changes,
  };
}

describe("the review", () => {
  it("accepts the commit and generation of the changes read in the work area", async () => {
    const client = new FakeClient();
    reviewable(client);
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("fix it"));
    await flush();
    const panel = screen.getByRole("tabpanel");
    expect(panel.querySelector(".patch-add")?.textContent).toBe("+<p onclick=x>b</p>");
    expect(panel.querySelector("p[onclick]")).toBeNull();
    fireEvent.change(screen.getByLabelText("Finding"), { target: { value: "rename the class" } });
    fireEvent.click(screen.getByRole("button", { name: "Add the finding" }));
    await flush();
    expect(within(screen.getByRole("list", { name: "Findings" })).getByText("rename the class")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Accept" }));
    await flush();
    expect(client.args.accept).toEqual([["t1", 7, "c".repeat(40), 4]]);
    expect(screen.getByText("Accepted. An accepted task is not refined.")).toBeTruthy();
  });

  it("does not accept changes it has not read, or stale ones", async () => {
    const client = new FakeClient();
    reviewable(client, { stale: true });
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("fix it"));
    await flush();
    expect((screen.getByRole("button", { name: "Accept" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows provider changes before a refine and sends it only once they are acknowledged", async () => {
    const client = new FakeClient();
    reviewable(client);
    client.tasks = client.tasks.map((t) => ({ ...t, attempt: { ...t.attempt!, provider_flagged: true, provider_changes: { changes: [{ path: ".claude/hooks/x.sh", change: "added" }], truncated: false } } }));
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("fix it"));
    await flush();
    fireEvent.click(screen.getByText("Refine: a new attempt with the findings"));
    const refine = screen.getByRole("button", { name: "Refine" }) as HTMLButtonElement;
    expect(refine.disabled).toBe(true);
    expect(screen.getAllByText(".claude/hooks/x.sh").length).toBeGreaterThan(0);
    expect((screen.getByRole("radio", { name: /readonly/ }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByLabelText(/I have read these changes/));
    fireEvent.click(refine);
    await flush();
    expect(client.args.refine).toEqual([["t1", 7, { backend: "claude", model: "", effort: "", mode: "readonly", acknowledge_provider_changes: true }]]);
    expect(client.calls).toContain("subscribe:attempt-t1-2");
  });
});

describe("the work area and the status bar", () => {
  it("shows checks marked unsandboxed and the files of the agent-changes commit, tab by tab from the keyboard", async () => {
    const client = new FakeClient();
    reviewable(client);
    client.checkResults.t1 = [{ id: "c1", ord: 0, name: "unit", definition: { argv: ["go", "test"] }, state: "passed", exit_code: 0, invalidated: false, output_size: 0, truncated: false, new_failure: false, baseline_failure: false, env_keys: [], duration_ms: 10 }];
    client.fileViews[""] = { task: "t1", commit: "c".repeat(40), path: "", type: "tree", mode: "040000", size: 0, truncated: false, entries: [{ name: "index.html", path: "index.html", type: "blob", mode: "100644", size: 3 }] };
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("fix it"));
    await flush();
    const changesTab = screen.getByRole("tab", { name: "Changes" });
    expect(screen.getByRole("tab", { name: /Checks/ }).textContent).toContain("unsandboxed");
    fireEvent.keyDown(changesTab, { key: "ArrowRight" });
    await flush();
    expect(screen.getByRole("tab", { name: /Checks/ }).getAttribute("aria-selected")).toBe("true");
    expect(within(screen.getByRole("tabpanel")).getByText("Checks are not sandboxed.")).toBeTruthy();
    expect(within(screen.getByRole("tabpanel")).getByText("passed")).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("tab", { name: /Checks/ }), { key: "End" });
    await flush();
    expect(within(screen.getByRole("tabpanel")).getByRole("button", { name: "index.html" })).toBeTruthy();
  });

  it("shows branch, executor, agent and task state, and Stop cancels the running task", async () => {
    const client = new FakeClient([makeTask({ id: "t1", state: "running", revision: 2, snapshot: "s1", attempt: { id: "a1", state: "running" } })]);
    render(<App client={client} />);
    await flush();
    const status = screen.getByRole("contentinfo");
    expect(within(status).getByText("none selected")).toBeTruthy();
    fireEvent.click(screen.getByText("objective t1"));
    await flush();
    expect(within(status).getByText("ada/t1")).toBeTruthy();
    expect(within(status).getByText("Executor").parentElement!.textContent).toContain("local");
    expect(within(status).getByText("not recorded")).toBeTruthy();
    fireEvent.click(within(status).getByRole("button", { name: "Stop" }));
    await flush();
    expect(client.tasks[0]!.state).toBe("cancel_requested");
  });
});
