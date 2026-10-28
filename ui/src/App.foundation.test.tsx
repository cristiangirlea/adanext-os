import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { themeKey } from "./theme/theme";
import { motionKey } from "./motion/effects";
import { FakeClient, makeTask } from "./test/fakeClient";

// How the foundation of M4.4a is reachable from today's App: the theme switch, the motion switch,
// the palette, a resize handle, the loading skeleton and the busy button.

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

function reset(): void {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  delete document.body.dataset.motion;
}
beforeEach(reset);
afterEach(() => {
  cleanup();
  reset();
});

describe("App foundation", () => {
  it("switches the theme from the status bar and saves the choice", async () => {
    render(<App client={new FakeClient()} />);
    await flush();
    const select = screen.getByLabelText("Theme") as HTMLSelectElement;
    expect(select.value).toBe("light");
    fireEvent.change(select, { target: { value: "workshop" } });
    expect(document.documentElement.dataset.theme).toBe("workshop");
    expect(localStorage.getItem(themeKey)).toBe("workshop");
  });

  it("opens the palette with Ctrl+P and selects the chosen task", async () => {
    const client = new FakeClient([makeTask({ id: "t1", objective: "fix the build" }), makeTask({ id: "t2", objective: "write the docs" })]);
    render(<App client={client} />);
    await flush();
    expect(fireEvent.keyDown(window, { key: "p", ctrlKey: true })).toBe(false);
    await flush();
    fireEvent.change(screen.getByRole("combobox", { name: /^Search/ }), { target: { value: "docs" } });
    fireEvent.keyDown(screen.getByRole("combobox", { name: /^Search/ }), { key: "Enter" });
    expect(screen.getByRole("heading", { level: 2, name: "write the docs" })).toBeTruthy();
  });

  it("turns motion off and on from the palette's quick actions", async () => {
    render(<App client={new FakeClient()} />);
    await flush();
    fireEvent.keyDown(window, { key: "p", ctrlKey: true, shiftKey: true });
    await flush();
    fireEvent.click(screen.getByRole("option", { name: "Turn motion off" }));
    expect(document.body.dataset.motion).toBe("off");
    expect(localStorage.getItem(motionKey)).toBe("off");
    fireEvent.keyDown(window, { key: "p", ctrlKey: true, shiftKey: true });
    await flush();
    fireEvent.click(within(screen.getByRole("group", { name: "Quick actions" })).getByRole("option", { name: "Turn motion on" }));
    expect(document.body.dataset.motion).toBeUndefined();
  });

  it("resizes the task list from the keyboard", async () => {
    render(<App client={new FakeClient()} />);
    await flush();
    const handle = screen.getByRole("separator", { name: "Resize the task list" });
    const before = Number(handle.getAttribute("aria-valuenow"));
    fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(Number(handle.getAttribute("aria-valuenow"))).toBeGreaterThan(before);
    fireEvent.keyDown(handle, { key: "End" });
    expect(handle.getAttribute("aria-valuenow")).toBe(handle.getAttribute("aria-valuemax"));
    expect(document.querySelector(".ada")!.getAttribute("style")).toContain(`--tasks-width: ${handle.getAttribute("aria-valuemax")}px`);
  });

  it("shows a skeleton while the task list loads, not an empty state", async () => {
    const client = new FakeClient([makeTask({ id: "t1" })]);
    let release = () => {};
    const listTasks = client.listTasks.bind(client);
    client.listTasks = () => new Promise((resolve) => { release = () => resolve(listTasks()); });
    render(<App client={client} />);
    await flush();
    expect(screen.getByRole("status", { name: "Loading tasks" })).toBeTruthy();
    expect(screen.queryByText("No tasks yet.")).toBeNull();
    await act(async () => release());
    await flush();
    expect(screen.queryByRole("status", { name: "Loading tasks" })).toBeNull();
    expect(screen.getByText("objective t1")).toBeTruthy();
  });

  it("marks the button just pressed busy until its request answers", async () => {
    const client = new FakeClient();
    let release = () => {};
    const createTask = client.createTask.bind(client);
    client.createTask = (input) => new Promise((resolve) => { release = () => resolve(createTask(input)); });
    render(<App client={client} />);
    await flush();
    fireEvent.change(screen.getByLabelText("Project"), { target: { value: "repo" } });
    fireEvent.change(screen.getByLabelText("Objective"), { target: { value: "add a test" } });
    const create = screen.getByRole("button", { name: "Create" });
    expect(create.getAttribute("aria-busy")).toBeNull();
    fireEvent.click(create);
    expect(create.getAttribute("aria-busy")).toBe("true");
    expect(create.querySelector('[data-effect="spinner"]')).not.toBeNull();
    await act(async () => release());
    await flush();
    expect(create.getAttribute("aria-busy")).toBeNull();
  });
});

describe("package.json", () => {
  it("adds no dependency", () => {
    const pkg = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "package.json"), "utf8")) as Record<string, Record<string, string>>;
    expect(Object.keys(pkg.dependencies ?? {}).sort()).toEqual(["react", "react-dom"]);
    expect(Object.keys(pkg.devDependencies ?? {}).sort()).toEqual([
      "@testing-library/dom",
      "@testing-library/react",
      "@types/node",
      "@types/react",
      "@types/react-dom",
      "@vitejs/plugin-react",
      "jsdom",
      "typescript",
      "vite",
      "vitest",
    ]);
  });
});
