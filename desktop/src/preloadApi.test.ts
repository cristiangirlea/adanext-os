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
