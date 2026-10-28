import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AdaError, type AdaClient } from "../client/api";
import { FakeClient, makeTask } from "../test/fakeClient";
import { CommandPalette, type QuickAction } from "./CommandPalette";

afterEach(cleanup);

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

// The methods AdaClient has today; the palette may call none but these.
const clientMethods: (keyof AdaClient)[] = ["createTask", "listTasks", "getTask", "ready", "start", "cancel", "subscribeEvents", "stopCoordinator", "finishQuit", "onQuitRequested"];

function setup(options: { actions?: QuickAction[]; client?: FakeClient } = {}) {
  const client =
    options.client ??
    new FakeClient([
      makeTask({ id: "t1", project: "alpha", objective: "fix the build" }),
      makeTask({ id: "t2", project: "beta", objective: "write the docs" }),
      makeTask({ id: "t3", project: "alpha", objective: "ship it" }),
    ]);
  const onSelectTask = vi.fn();
  const run = vi.fn();
  const actions = options.actions ?? [{ id: "theme", label: "Switch to the workshop theme", run }];
  render(<CommandPalette client={client} actions={actions} onSelectTask={onSelectTask} />);
  return { client, onSelectTask, run };
}

function press(key: string, init: KeyboardEventInit = {}): boolean {
  return fireEvent.keyDown(window, { key, ...init });
}

const combobox = () => screen.getByRole("combobox");
const optionTexts = () => screen.queryAllByRole("option").map((o) => o.textContent);
const status = () => screen.getByTestId("palette-status").textContent;

describe("CommandPalette", () => {
  it("opens on Ctrl+P and prevents the browser's print default", async () => {
    setup();
    expect(screen.queryByRole("dialog")).toBeNull();
    const notPrevented = press("p", { ctrlKey: true });
    expect(notPrevented).toBe(false);
    await flush();
    expect(screen.getByRole("dialog", { name: "Command palette" })).toBeTruthy();
    expect(document.activeElement).toBe(combobox());
  });

  it("also prevents the default of Cmd+P and of Ctrl+P while open, which closes it", async () => {
    setup();
    expect(press("P", { metaKey: true })).toBe(false);
    await flush();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(press("p", { ctrlKey: true })).toBe(false);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("calls nothing until a scope needs it, then only listTasks, once", async () => {
    const { client } = setup();
    expect(client.calls).toEqual([]);
    press("p", { ctrlKey: true, shiftKey: true });
    await flush();
    expect(screen.getByRole("button", { name: "Actions" }).getAttribute("aria-pressed")).toBe("true");
    expect(client.calls).toEqual([]);
    expect(optionTexts()).toEqual(["Switch to the workshop theme"]);
    fireEvent.click(screen.getByRole("button", { name: "Tasks" }));
    await flush();
    expect(client.calls).toEqual(["listTasks"]);
    expect(optionTexts().map((t) => t?.replace(/(alpha|beta)$/, ""))).toEqual(["fix the build", "write the docs", "ship it"]);
    fireEvent.click(screen.getByRole("button", { name: "Projects" }));
    await flush();
    expect(optionTexts()).toEqual(["alpha", "beta"]);
    fireEvent.click(screen.getByRole("button", { name: "All" }));
    await flush();
    expect(client.calls).toEqual(["listTasks"]);
    for (const call of client.calls) expect(clientMethods).toContain(call);
  });

  it("loads tasks when it opens in the all scope, and fresh on the next open", async () => {
    const { client } = setup();
    press("p", { ctrlKey: true });
    await flush();
    expect(client.calls).toEqual(["listTasks"]);
    expect(screen.getByRole("group", { name: "Quick actions" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Tasks" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Projects" })).toBeTruthy();
    fireEvent.keyDown(combobox(), { key: "Escape" });
    press("p", { ctrlKey: true });
    await flush();
    expect(client.calls).toEqual(["listTasks", "listTasks"]);
  });

  it("moves through the options with the arrow keys through aria-activedescendant, wrapping at the ends", async () => {
    setup();
    press("p", { ctrlKey: true });
    await flush();
    const input = combobox();
    const listbox = screen.getByRole("listbox");
    expect(input.getAttribute("aria-controls")).toBe(listbox.id);
    expect(input.getAttribute("aria-expanded")).toBe("true");
    expect(input.getAttribute("aria-autocomplete")).toBe("list");
    const options = screen.getAllByRole("option");
    const active = () => document.getElementById(input.getAttribute("aria-activedescendant") ?? "");
    expect(active()).toBe(options[0]);
    expect(options[0]!.getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(active()).toBe(options[1]);
    expect(options[0]!.getAttribute("aria-selected")).toBe("false");
    fireEvent.keyDown(input, { key: "ArrowUp" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(active()).toBe(options[options.length - 1]);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(active()).toBe(options[0]);
    // Keys repeating faster than renders still move one option each.
    act(() => {
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
      input.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    });
    expect(active()).toBe(options[2]);
    fireEvent.keyDown(input, { key: "End", ctrlKey: true });
    expect(active()).toBe(options[options.length - 1]);
    fireEvent.keyDown(input, { key: "Home", ctrlKey: true });
    expect(active()).toBe(options[0]);
    expect(document.activeElement).toBe(input);
  });

  it("chooses a task with Enter, closes, and lists it under Recent next time", async () => {
    const { onSelectTask } = setup();
    press("p", { ctrlKey: true });
    await flush();
    expect(screen.queryByRole("group", { name: "Recent" })).toBeNull();
    fireEvent.change(combobox(), { target: { value: "docs" } });
    expect(optionTexts()).toEqual(["write the docsbeta"]);
    fireEvent.keyDown(combobox(), { key: "Enter" });
    expect(onSelectTask).toHaveBeenCalledWith("t2");
    expect(screen.queryByRole("dialog")).toBeNull();
    press("p", { ctrlKey: true });
    await flush();
    const recent = screen.getByRole("group", { name: "Recent" });
    expect(within(recent).getByRole("option").textContent).toContain("write the docs");
  });

  it("runs a quick action on click", async () => {
    const { run } = setup();
    press("p", { ctrlKey: true });
    await flush();
    fireEvent.click(screen.getByRole("option", { name: "Switch to the workshop theme" }));
    expect(run).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("narrows to a project's tasks when a project is chosen", async () => {
    setup();
    press("p", { ctrlKey: true });
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Projects" }));
    fireEvent.click(screen.getByRole("option", { name: "alpha" }));
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Tasks" }).getAttribute("aria-pressed")).toBe("true");
    expect(optionTexts()).toEqual(["fix the buildalpha", "ship italpha"]);
  });

  it("filters a chosen project's tasks by its exact id, not by text, until the filter is cleared", async () => {
    const client = new FakeClient([
      makeTask({ id: "t1", project: "alpha", objective: "fix the build" }),
      makeTask({ id: "t2", project: "alpha-2", objective: "write the docs" }),
      makeTask({ id: "t3", project: "beta", objective: "port the alpha release" }),
    ]);
    setup({ client });
    press("p", { ctrlKey: true });
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Projects" }));
    fireEvent.click(screen.getByRole("option", { name: "alpha" }));
    expect(optionTexts()).toEqual(["fix the buildalpha"]);
    expect(combobox()).toHaveProperty("value", "");
    fireEvent.click(screen.getByRole("button", { name: "Show all tasks" }));
    expect(optionTexts()).toEqual(["fix the buildalpha", "write the docsalpha-2", "port the alpha releasebeta"]);
  });

  it("says what it shows on a live status line", async () => {
    const client = new FakeClient([makeTask({ id: "t1", objective: "fix the build" })]);
    let release = () => {};
    const listTasks = client.listTasks.bind(client);
    client.listTasks = () => new Promise((resolve) => { release = () => resolve(listTasks()); });
    setup({ client });
    press("p", { ctrlKey: true });
    await flush();
    const line = screen.getByTestId("palette-status");
    expect(line.getAttribute("role")).toBe("status");
    expect(line.getAttribute("aria-live")).toBe("polite");
    expect(status()).toBe("Loading tasks");
    expect(screen.getByRole("listbox").getAttribute("aria-busy")).toBe("true");
    await act(async () => release());
    await flush();
    expect(status()).toBe("3 results");
    fireEvent.change(combobox(), { target: { value: "build" } });
    expect(status()).toBe("1 result");
    fireEvent.change(combobox(), { target: { value: "nothing like it" } });
    expect(status()).toBe('No results for "nothing like it"');
    expect(combobox().hasAttribute("aria-activedescendant")).toBe(false);
  });

  it("says when tasks could not be loaded", async () => {
    const client = new FakeClient();
    client.refuse.listTasks = new AdaError("coordinator_unreachable", 0, "no pipe");
    setup({ client });
    press("p", { ctrlKey: true, shiftKey: true });
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Tasks" }));
    await flush();
    expect(status()).toBe("Tasks could not be loaded: coordinator_unreachable");
  });

  it("closes on Escape and gives focus back", async () => {
    setup();
    const before = document.createElement("button");
    document.body.appendChild(before);
    before.focus();
    press("p", { ctrlKey: true });
    await flush();
    fireEvent.keyDown(combobox(), { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(before);
    before.remove();
  });

  it("closes the modal dialog before giving focus back to the opener", async () => {
    // jsdom has no modal dialog; these stand in for the browser's, recording the order.
    const order: string[] = [];
    const proto = HTMLDialogElement.prototype as unknown as Record<string, unknown>;
    proto.showModal = function (this: HTMLDialogElement) {
      this.setAttribute("open", "");
    };
    proto.close = function (this: HTMLDialogElement) {
      order.push("close");
      this.removeAttribute("open");
    };
    try {
      setup();
      const opener = document.createElement("button");
      document.body.appendChild(opener);
      opener.focus();
      const focus = opener.focus.bind(opener);
      opener.focus = () => {
        order.push("focus");
        focus();
      };
      press("p", { ctrlKey: true });
      await flush();
      expect(screen.getByRole("dialog").hasAttribute("open")).toBe(true);
      fireEvent.keyDown(combobox(), { key: "Escape" });
      expect(order).toEqual(["close", "focus"]);
      expect(document.activeElement).toBe(opener);
      opener.remove();
    } finally {
      delete proto.showModal;
      delete proto.close;
    }
  });

  it("shows the key hints and labelled scope chips", async () => {
    setup();
    press("p", { ctrlKey: true });
    await flush();
    const chips = screen.getByRole("group", { name: "Scope" });
    expect(within(chips).getAllByRole("button").map((b) => b.textContent)).toEqual(["All", "Projects", "Tasks", "Actions"]);
    const hints = screen.getByTestId("palette-hints").textContent ?? "";
    for (const hint of ["move", "choose", "close"]) expect(hints).toContain(hint);
  });
});
