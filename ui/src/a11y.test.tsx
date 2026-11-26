import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { FakeClient, makeAccount, makeTask } from "./test/fakeClient";
import { AdaError } from "./client/api";
import { Root } from "./browser/Gate";

// The accessibility pass of M4.4a: every control has a name and a role, the keyboard reaches
// every control, focus is always visible, and the palette keeps focus in its input. Contrast is
// checked by scripts/contrast.mjs (styles/contrast.test.ts).

afterEach(cleanup);

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

function nameOf(el: Element): string {
  const labelledBy = el.getAttribute("aria-labelledby");
  if (labelledBy) return labelledBy.split(/\s+/).map((id) => document.getElementById(id)?.textContent ?? "").join(" ").trim();
  const label = el.getAttribute("aria-label");
  if (label) return label;
  if (el instanceof HTMLInputElement || el instanceof HTMLSelectElement || el instanceof HTMLTextAreaElement) return el.labels?.[0]?.textContent?.trim() ?? "";
  return el.textContent?.trim() ?? "";
}

const controls = 'button, input, select, [role="separator"], [role="option"], [role="listbox"], [role="combobox"], dialog, [tabindex]';

async function renderWithPalette() {
  const client = new FakeClient([makeTask({ id: "t1", objective: "fix the build", state: "running", attempt: { id: "a1", state: "running" } })]);
  render(<App client={client} />);
  await flush();
  fireEvent.click(screen.getByText("fix the build"));
  await flush();
  fireEvent.keyDown(window, { key: "p", ctrlKey: true });
  await flush();
}

describe("accessibility", () => {
  it("labels the import root settings and folder dialog action", async () => {
    render(<App client={new FakeClient()} />);
    fireEvent.click(screen.getByRole("button", { name: "Import roots" }));
    await flush();
    expect(screen.getByRole("dialog", { name: "Import roots" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add folder" })).toBeTruthy();
    expect(Array.from(document.querySelectorAll(controls)).filter(el => !nameOf(el))).toEqual([]);
  });

  it("names every control", async () => {
    await renderWithPalette();
    const unnamed = Array.from(document.querySelectorAll(controls)).filter((el) => nameOf(el) === "");
    expect(unnamed.map((el) => el.outerHTML.slice(0, 80))).toEqual([]);
  });

  it("lets the keyboard reach the resize handle, the scope chips and the input; options stay out of the tab order", async () => {
    await renderWithPalette();
    const tabbable = (el: HTMLElement) => el.tabIndex >= 0 && !(el as HTMLButtonElement).disabled;
    expect(tabbable(screen.getByRole("separator", { name: "Resize the task list" }))).toBe(true);
    for (const chip of ["All", "Projects", "Tasks", "Actions"]) expect(tabbable(screen.getByRole("button", { name: chip }))).toBe(true);
    for (const option of screen.getAllByRole("option")) expect(option.hasAttribute("tabindex")).toBe(false);
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: /^Search/ }));
  });

  it("names the palette dialog and labels each group of results", async () => {
    await renderWithPalette();
    expect(screen.getByRole("dialog", { name: "Command palette" })).toBeTruthy();
    for (const group of screen.getAllByRole("group").filter((g) => g.closest('[role="listbox"]'))) expect(nameOf(group)).not.toBe("");
  });

  it("shows focus on everything with the focus token, outside the control", () => {
    const base = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "styles", "base.css"), "utf8");
    expect(base).toMatch(/:focus-visible\s*\{[^}]*outline:\s*2px solid var\(--focus\)[^}]*outline-offset:\s*2px/);
    const all = ["base", "controls", "app", "palette", "resize", "motion", "workspace", "review", "fonts"].map((n) => readFileSync(join(dirname(fileURLToPath(import.meta.url)), "styles", `${n}.css`), "utf8")).join("\n");
    expect(all).not.toMatch(/outline:\s*(none|0)\b/);
  });

  it("names every control of the workspace: navigation, policy picker, review, work area tabs and the import dialog", async () => {
    const commit = "c".repeat(40);
    const client = new FakeClient([
      makeTask({ id: "t1", objective: "ready one", state: "ready", revision: 1 }),
      makeTask({ id: "t2", objective: "review me", state: "awaiting_review", revision: 3, snapshot: "s1", attempt: { id: "a2", state: "ended_ok", tested_commit: commit } }),
    ]);
    client.refuse.changes = new AdaError("no_changes", 409, "none yet");
    render(<App client={client} />);
    await flush();
    const check = () => {
      const unnamed = Array.from(document.querySelectorAll(controls + ', textarea, [role="tab"], [role="tabpanel"], [role="radio"], input[type="radio"], input[type="checkbox"]')).filter((el) => nameOf(el) === "");
      expect(unnamed.map((el) => el.outerHTML.slice(0, 100))).toEqual([]);
    };
    check();
    fireEvent.click(screen.getByText("ready one"));
    await flush();
    check();
    fireEvent.click(screen.getByText("review me"));
    await flush();
    check();
    // The tabs follow the tabs pattern: one tab in the tab order, the panel labelled by it.
    const tabs = screen.getAllByRole("tab");
    expect(tabs.filter((t) => t.tabIndex === 0)).toHaveLength(1);
    expect(screen.getByRole("tabpanel").getAttribute("aria-labelledby")).toBe(tabs.find((t) => t.getAttribute("aria-selected") === "true")!.id);
    fireEvent.click(screen.getByRole("button", { name: "Import a repository" }));
    fireEvent.click(screen.getByRole("button", { name: "Choose a folder" }));
    await flush();
    await flush();
    check();
    expect(screen.getByRole("dialog", { name: "Import a repository" })).toBeTruthy();
  });

  it("names every control of the accounts view, its confirmation and its code", async () => {
    const client = new FakeClient();
    client.accountList = [makeAccount({ id: "local", name: "owner", owner: true }), makeAccount({ id: "u2", name: "sam" })];
    client.sessionList = [{ id: "s1", account: "u2", created: "2026-11-20T08:00:00Z", last_used: "2026-11-20T09:00:00Z", address: "192.0.2.4" }];
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    await flush();
    await flush();
    const check = () => {
      const unnamed = Array.from(document.querySelectorAll(controls + ', [role="group"], input[type="checkbox"]')).filter((el) => nameOf(el) === "");
      expect(unnamed.map((el) => el.outerHTML.slice(0, 100))).toEqual([]);
    };
    check();
    expect(screen.getByRole("dialog", { name: "Accounts" })).toBeTruthy();
    // Each table's columns have headers; the row buttons say whose account they act on.
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toContain("Actions");
    fireEvent.click(screen.getByRole("button", { name: "Remove sam" }));
    check();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "New code for sam" }));
    await flush();
    await flush();
    check();
  });

  it("names every control of the network access view, with and without a certificate, and its refusal", async () => {
    const client = new FakeClient();
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Network access" }));
    await flush();
    await flush();
    const check = () => {
      const unnamed = Array.from(document.querySelectorAll(controls + ', [role="group"], [role="region"], [role="status"], [role="note"], input[type="checkbox"], input[type="text"]')).filter((el) => nameOf(el) === "");
      expect(unnamed.map((el) => el.outerHTML.slice(0, 100))).toEqual([]);
    };
    check();
    expect(screen.getByRole("dialog", { name: "Network access" })).toBeTruthy();
    for (const label of ["Enabled", "Address", "Name"]) expect(screen.getByLabelText(label)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Choose certificate and key…" }));
    await flush();
    await flush();
    check();
    client.refuse.setListener = new AdaError("name_invalid", 400, "the name must be a lowercase host name");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await flush();
    await flush();
    check();
  });

  it("lets the keyboard select a task in the navigation", async () => {
    const client = new FakeClient([makeTask({ id: "t1", objective: "by keyboard" })]);
    render(<App client={client} />);
    await flush();
    const row = screen.getAllByRole("row").find((r) => r.getAttribute("data-task") === "t1")!;
    expect(row.tabIndex).toBe(0);
    fireEvent.keyDown(row, { key: "Enter" });
    expect(screen.getByRole("heading", { level: 2, name: "by keyboard" })).toBeTruthy();
  });

  it("names every control of the browser's sign-in, enrollment and account pages, and the browser workspace", async () => {
    const check = () => {
      const unnamed = Array.from(document.querySelectorAll(controls + ', form, [role="group"], [role="region"], input')).filter((el) => nameOf(el) === "");
      expect(unnamed.map((el) => el.outerHTML.slice(0, 100))).toEqual([]);
    };
    const client = new FakeClient([makeTask({ id: "t1", objective: "remote task" })], "network");
    const sam = { id: "u1", name: "sam", owner: false, password_allowed: true };
    client.enrollCodes = { CODE: sam };
    client.ownSessionList = [{ id: "s1", created: "2026-11-24T10:00:00Z", last_used: "2026-11-24T10:00:00Z", address: "192.0.2.1", current: true }];
    client.credentialList = [{ id: "c1", kind: "passkey", name: "Laptop", created: "2026-11-24T10:00:00Z", last_used: "" }];
    render(<Root client={client} />);
    await flush();
    await flush();
    expect(screen.getByRole("main", { name: "Sign in" })).toBeTruthy();
    check();
    fireEvent.click(screen.getByRole("button", { name: "Enroll with a code" }));
    check();
    fireEvent.change(screen.getByLabelText("Enrollment code"), { target: { value: "CODE" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    await flush();
    await flush();
    expect(screen.getByRole("main", { name: "Enroll with a code" })).toBeTruthy();
    check();
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "a long enough secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Set the password and sign in" }));
    await flush();
    await flush();
    check();
    fireEvent.click(screen.getByRole("button", { name: "Your account" }));
    await flush();
    await flush();
    expect(screen.getByRole("dialog", { name: "Your account: sam" })).toBeTruthy();
    check();
  });
});

describe("accessibility of the decision and the stop", () => {
  it("names the decision's region and buttons, and the stop's group, with focus on the way out", async () => {
    const client = new FakeClient([
      makeTask({ id: "t1", objective: "edit the parser", state: "preparing", attempt: { id: "a1", state: "pending", backend: "claude", mode: "yolo", decision: { actor: "local", required: true, scope: { backend: "claude", mode: "yolo", workspace: "w1" }, project_default: "auto", allowed: false, denied: false } } }),
    ]);
    client.projectList = [{ id: "p1", name: "repo", role: "owner", created: "", default_mode: "auto" }];
    render(<App client={client} />);
    await flush();
    fireEvent.click(screen.getByText("edit the parser"));
    await flush();
    await flush();
    const region = screen.getByRole("region", { name: "Waiting for your decision" });
    for (const button of Array.from(region.querySelectorAll("button"))) expect(nameOf(button)).not.toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Quit Ada and stop work" }));
    await flush();
    const group = screen.getByRole("group", { name: "Stop work" });
    for (const button of Array.from(group.querySelectorAll("button"))) expect(nameOf(button)).not.toBe("");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Keep working" }));
    const unnamed = Array.from(document.querySelectorAll(controls)).filter((el) => nameOf(el) === "");
    expect(unnamed.map((el) => el.outerHTML.slice(0, 80))).toEqual([]);
  });
});
