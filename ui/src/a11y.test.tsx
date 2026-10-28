import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { FakeClient, makeTask } from "./test/fakeClient";

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
  if (el instanceof HTMLInputElement || el instanceof HTMLSelectElement) return el.labels?.[0]?.textContent?.trim() ?? "";
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
    const all = ["base", "controls", "app", "palette", "resize", "motion"].map((n) => readFileSync(join(dirname(fileURLToPath(import.meta.url)), "styles", `${n}.css`), "utf8")).join("\n");
    expect(all).not.toMatch(/outline:\s*(none|0)\b/);
  });
});
