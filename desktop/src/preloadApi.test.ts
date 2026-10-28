import { describe, expect, it } from "vitest";
import type { BridgeResult, StreamMessage } from "../../ui/src/client/api";
import { createApi, type Ipc } from "./preloadApi";

// A fake ipcRenderer: records every invoke, answers each from `answers`, and hands the test the
// event listener so it can push messages the way main does.
function ipc(answers: Record<string, BridgeResult<unknown>> = {}): Ipc & { calls: [string, unknown[]][]; push: (subId: unknown, message: unknown) => void } {
  let listener: ((subId: unknown, message: unknown) => void) | undefined;
  const self = {
    calls: [] as [string, unknown[]][],
    invoke: async (method: string, args: unknown[]): Promise<BridgeResult<unknown>> => {
      self.calls.push([method, args]);
      return answers[method] ?? { ok: true, value: null };
    },
    onEvent: (fn: (subId: unknown, message: unknown) => void): void => {
      listener = fn;
    },
    push: (subId: unknown, message: unknown): void => listener?.(subId, message),
  };
  return self;
}

async function settled(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("preload api", () => {
  it("scopes subscription ids to the document, so a reloaded page never reuses the old one's", () => {
    const first = ipc();
    const second = ipc();
    createApi(first, "doc-a").subscribeEvents("a1", () => {});
    createApi(second, "doc-b").subscribeEvents("a1", () => {});
    const [idA] = first.calls[0]![1] as [string, string];
    const [idB] = second.calls[0]![1] as [string, string];
    expect(idA).not.toBe(idB);
    expect(idA).toMatch(/^[A-Za-z0-9_-]{1,128}$/);
    expect(idB).toMatch(/^[A-Za-z0-9_-]{1,128}$/);
  });

  it("delivers a subscription's messages to its callback and unsubscribes once", async () => {
    const fake = ipc();
    const received: StreamMessage[] = [];
    const unsubscribe = createApi(fake, "doc").subscribeEvents("a1", (message) => received.push(message));
    const [subId] = fake.calls[0]![1] as [string, string];
    fake.push(subId, { type: "status", status: "connected" });
    fake.push("someone-else", { type: "reset", last_sequence: 0 });
    expect(received).toEqual([{ type: "status", status: "connected" }]);
    unsubscribe();
    unsubscribe();
    expect(fake.calls.filter(([method]) => method === "unsubscribe")).toEqual([["unsubscribe", [subId]]]);
    fake.push(subId, { type: "status", status: "connected" });
    expect(received).toHaveLength(1);
  });

  it("forwards each workspace method under its own name with its typed arguments, and none takes a host path", async () => {
    const fake = ipc();
    const api = createApi(fake, "doc");
    const policy = { backend: "claude", model: "", effort: "", mode: "readonly" as const, acknowledge_provider_changes: false };
    await api.capabilities();
    await api.chooseFolder();
    await api.inspectFolder("h1");
    await api.importFolder("h1", "repo");
    await api.snapshot("h1", { untracked: ["a"], include_sensitive: [] });
    await api.changes("t1");
    await api.files("t1", "src");
    await api.checks("t1");
    await api.checkLog("t1", "c1");
    await api.findings("t1");
    await api.addFinding("t1", "note");
    await api.accept("t1", 3, "a".repeat(40), 1);
    await api.refine("t1", 3, policy);
    expect(fake.calls).toEqual([
      ["capabilities", []],
      ["chooseFolder", []],
      ["inspectFolder", ["h1"]],
      ["importFolder", ["h1", "repo"]],
      ["snapshot", ["h1", { untracked: ["a"], include_sensitive: [] }]],
      ["changes", ["t1"]],
      ["files", ["t1", "src"]],
      ["checks", ["t1"]],
      ["checkLog", ["t1", "c1"]],
      ["findings", ["t1"]],
      ["addFinding", ["t1", "note"]],
      ["accept", ["t1", 3, "a".repeat(40), 1]],
      ["refine", ["t1", 3, policy]],
    ]);
    // The dialog's method takes nothing: whatever the page passes, no argument reaches main.
    await (api.chooseFolder as (...args: unknown[]) => Promise<unknown>)("C:\\elsewhere");
    expect(fake.calls.at(-1)).toEqual(["chooseFolder", []]);
  });

  it("leaves no listener behind a refused subscription", async () => {
    const fake = ipc({ subscribe: { ok: false, error: { code: "coordinator_unreachable", status: 0, message: "no socket" } } });
    const received: StreamMessage[] = [];
    const unsubscribe = createApi(fake, "doc").subscribeEvents("a1", (message) => received.push(message));
    await settled();
    expect(received).toEqual([{ type: "status", status: "unreachable", code: "coordinator_unreachable" }]);
    const [subId] = fake.calls[0]![1] as [string, string];
    fake.push(subId, { type: "status", status: "connected" });
    expect(received).toHaveLength(1);
    unsubscribe();
    expect(fake.calls.map(([method]) => method)).toEqual(["subscribe"]);
  });
});
