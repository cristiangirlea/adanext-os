import { afterEach, describe, expect, it } from "vitest";
import { AdaError, type BridgeApi, type StreamMessage, type Task } from "../api";
import { createBridgeClient } from "./bridge";

const task: Task = { id: "t1", project: "p", created_by: "local", objective: "o", criteria: "", state: "draft", revision: 0, created: "", updated: "" };

function fakeBridge(overrides: Partial<BridgeApi> = {}): BridgeApi {
  return {
    createTask: async () => ({ ok: true, value: task }),
    listTasks: async () => ({ ok: true, value: [task] }),
    getTask: async () => ({ ok: true, value: task }),
    ready: async () => ({ ok: true, value: task }),
    start: async () => ({ ok: true, value: { ...task, attempt_id: "a1" } }),
    cancel: async () => ({ ok: true, value: task }),
    subscribeEvents: () => () => {},
    stopCoordinator: async () => ({ ok: true, value: null }),
    ...overrides,
  };
}

function install(bridge?: BridgeApi): void {
  (window as { ada?: BridgeApi }).ada = bridge;
}

afterEach(() => install(undefined));

describe("bridge adapter", () => {
  it("returns the bridge's values", async () => {
    install(fakeBridge());
    const client = createBridgeClient();
    expect(await client.listTasks()).toEqual([task]);
    expect((await client.start("t1", 0, { backend: "claude", model: "", effort: "", mode: "readonly" })).attempt_id).toBe("a1");
  });

  it("throws adad's refusal with its code and status", async () => {
    install(fakeBridge({ ready: async () => ({ ok: false, error: { code: "stale_revision", status: 409, message: "stale revision" } }) }));
    const error = await createBridgeClient().ready("t1", 0).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(AdaError);
    expect((error as AdaError).code).toBe("stale_revision");
    expect((error as AdaError).status).toBe(409);
  });

  it("keeps the server's request id on an internal error", async () => {
    install(fakeBridge({ getTask: async () => ({ ok: false, error: { code: "internal", status: 500, message: "internal error", request_id: "req-7" } }) }));
    const error = await createBridgeClient().getTask("t1").catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("internal");
    expect((error as AdaError).requestId).toBe("req-7");
  });

  it("passes stream messages through and hands back the unsubscribe", () => {
    let unsubscribed = false;
    let sink: ((message: StreamMessage) => void) | undefined;
    install(
      fakeBridge({
        subscribeEvents: (attemptId, onMessage) => {
          expect(attemptId).toBe("a1");
          sink = onMessage;
          return () => {
            unsubscribed = true;
          };
        },
      }),
    );
    const received: StreamMessage[] = [];
    const unsubscribe = createBridgeClient().subscribeEvents("a1", (message) => received.push(message));
    sink?.({ type: "event", event: { seq: 1, kind: "accepted", payload: {} } });
    expect(received).toEqual([{ type: "event", event: { seq: 1, kind: "accepted", payload: {} } }]);
    unsubscribe();
    expect(unsubscribed).toBe(true);
  });

  it("reports a missing bridge as bridge_unavailable", async () => {
    const error = await createBridgeClient().listTasks().catch((e: unknown) => e);
    expect((error as AdaError).code).toBe("bridge_unavailable");
  });
});
