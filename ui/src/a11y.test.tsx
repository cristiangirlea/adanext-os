import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { FakeClient, makeTask } from "./test/fakeClient";
import { AdaError } from "./client/api";

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

  it("lets the keyboard select a task in the navigation", async () => {
    const client = new FakeClient([makeTask({ id: "t1", objective: "by keyboard" })]);
    render(<App client={client} />);
    await flush();
    const row = screen.getAllByRole("row").find((r) => r.getAttribute("data-task") === "t1")!;
    expect(row.tabIndex).toBe(0);
    fireEvent.keyDown(row, { key: "Enter" });
    expect(screen.getByRole("heading", { level: 2, name: "by keyboard" })).toBeTruthy();
  });
});
