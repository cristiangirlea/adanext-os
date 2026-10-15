import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import type { StreamMessage } from "../../ui/src/client/api";
import { createBridge, type Bridge, type BridgeEvent } from "./bridge";
import { json, listenFake, readBody, until, type FakeAdad } from "./test/fakeAdad";

const pageUrl = "file:///E:/ada/ui/dist/index.html";
const fakes: FakeAdad[] = [];
const bridges: Bridge[] = [];
afterEach(async () => {
  for (const bridge of bridges.splice(0)) bridge.close();
  for (const fake of fakes.splice(0)) await fake.close();
});

interface FakeSender {
  id: number;
  sent: unknown[][];
  destroyed: boolean;
  destroy(): void;
  event(frame?: { url: string; parent: unknown } | null): BridgeEvent;
}

function sender(id = 1): FakeSender {
  const listeners: (() => void)[] = [];
  const self: FakeSender = {
    id,
    sent: [],
    destroyed: false,
    destroy() {
      self.destroyed = true;
      for (const fn of listeners) fn();
    },
    event(frame = { url: pageUrl, parent: null }) {
      return {
        senderFrame: frame,
        sender: {
          id,
          send: (...args: unknown[]) => self.sent.push(args),
          once: (_name: "destroyed", fn: () => void) => {
            listeners.push(fn);
          },
          isDestroyed: () => self.destroyed,
        },
      };
    },
  };
  return self;
}

async function bridgeTo(handler: (req: http.IncomingMessage, res: http.ServerResponse) => void): Promise<{ fake: FakeAdad; bridge: Bridge }> {
  const fake = await listenFake(handler);
  fakes.push(fake);
  const bridge = createBridge({ socketPath: fake.socketPath, pageUrl });
  bridges.push(bridge);
  return { fake, bridge };
}

const task = { id: "t1", project: "p", created_by: "local", objective: "o", criteria: "", state: "draft", revision: 0, created: "", updated: "" };

describe("bridge", () => {
  it("refuses a call from an unexpected frame before touching adad", async () => {
    const { fake, bridge } = await bridgeTo((_req, res) => json(res, 200, []));
    const s = sender();
    const refused = [
      await bridge.handle(s.event(null), "listTasks", []),
      await bridge.handle(s.event({ url: "file:///E:/elsewhere/index.html", parent: null }), "listTasks", []),
      await bridge.handle(s.event({ url: "https://example.invalid/", parent: null }), "listTasks", []),
      await bridge.handle(s.event({ url: pageUrl, parent: {} }), "listTasks", []),
    ];
    for (const result of refused) {
      expect(result).toEqual({ ok: false, error: { code: "unexpected_sender", status: 0, message: expect.any(String) } });
    }
    expect(fake.requests).toHaveLength(0);
    expect(await bridge.handle(s.event(), "listTasks", [])).toEqual({ ok: true, value: [] });
    expect(fake.requests).toHaveLength(1);
  });

  it("forwards typed calls to /v1 with the idempotency key and If-Match", async () => {
    const bodies: string[] = [];
    const { fake, bridge } = await bridgeTo((req, res) => {
      void readBody(req).then((body) => {
        bodies.push(body);
        if (req.method === "POST" && req.url === "/v1/tasks") json(res, 201, { ...task, project: "repo", objective: "add a test" });
        else if (req.method === "GET" && req.url === "/v1/tasks/t1") json(res, 200, task);
        else if (req.method === "POST" && req.url === "/v1/tasks/t1/start") json(res, 200, { ...task, state: "preparing", revision: 2, attempt_id: "a1" });
        else if (req.method === "POST" && req.url === "/v1/coordinator/stop") json(res, 202, {});
        else json(res, 404, { error: "not_found", message: "not found" });
      });
    });
    const s = sender();
    expect(await bridge.handle(s.event(), "createTask", [{ project: "repo", objective: "add a test" }])).toEqual({ ok: true, value: { ...task, project: "repo", objective: "add a test" } });
    expect(await bridge.handle(s.event(), "getTask", ["t1"])).toEqual({ ok: true, value: task });
    const policy = { backend: "claude", model: "", effort: "", mode: "readonly" };
    expect(await bridge.handle(s.event(), "start", ["t1", 1, policy])).toEqual({ ok: true, value: { ...task, state: "preparing", revision: 2, attempt_id: "a1" } });
    expect(await bridge.handle(s.event(), "stopCoordinator", [])).toEqual({ ok: true, value: null });
    expect(bodies[0]).toBe(JSON.stringify({ project: "repo", objective: "add a test" }));
    expect(bodies[2]).toBe(JSON.stringify(policy));
    const start = fake.requests[2]!;
    expect(start.headers["if-match"]).toBe('"1"');
    expect(start.headers["content-type"]).toBe("application/json");
    const key = start.headers["idempotency-key"];
    expect(typeof key).toBe("string");
    expect(key).toMatch(/^[\x21-\x7e]{1,255}$/);
    const stop = fake.requests[3]!;
    expect(stop.headers["idempotency-key"]).toMatch(/^[\x21-\x7e]{1,255}$/);
    expect(stop.headers["idempotency-key"]).not.toBe(key);
    expect(fake.requests[0]?.headers["if-match"]).toBeUndefined();
  });

  it("hands back adad's refusal with its code and status", async () => {
    const { fake, bridge } = await bridgeTo((req, res) => {
      void readBody(req).then(() => json(res, 409, { error: "stale_revision", message: "stale revision" }));
    });
    const result = await bridge.handle(sender().event(), "ready", ["t1", 3]);
    expect(result).toEqual({ ok: false, error: { code: "stale_revision", status: 409, message: "stale revision" } });
    expect(fake.requests[0]?.headers["if-match"]).toBe('"3"');
    expect(fake.requests[0]?.url).toBe("/v1/tasks/t1/ready");
  });

  it("refuses malformed calls without reaching adad", async () => {
    const { fake, bridge } = await bridgeTo((_req, res) => json(res, 200, task));
    const s = sender();
    const calls: [string, unknown[]][] = [
      ["ready", ["t1", -1]],
      ["ready", ["t1", 1.5]],
      ["ready", ["t1", "1"]],
      ["ready", ["../tasks", 1]],
      ["getTask", [""]],
      ["getTask", ["t1", "extra"]],
      ["createTask", [{ project: "p", objective: "o", surprise: true }]],
      ["createTask", [{ project: "p" }]],
      ["start", ["t1", 1, { backend: "claude", model: "", effort: "", mode: "sudo" }]],
      ["start", ["t1", 1, { backend: "claude", model: "", mode: "auto" }]],
      ["subscribe", ["s1", "a/1"]],
      ["unsubscribe", [1]],
      ["listTasks", ["unexpected"]],
      ["evaluate", []],
      ["__proto__", []],
    ];
    for (const [method, args] of calls) {
      const result = await bridge.handle(s.event(), method, args);
      expect(result, `${method} ${JSON.stringify(args)}`).toEqual({ ok: false, error: { code: "malformed_call", status: 0, message: expect.any(String) } });
    }
    expect(fake.requests).toHaveLength(0);
  });

  it("answers coordinator_unreachable without a socket path or with a dead one", async () => {
    const none = createBridge({ socketPath: undefined, pageUrl });
    bridges.push(none);
    expect(await none.handle(sender().event(), "listTasks", [])).toEqual({ ok: false, error: { code: "coordinator_unreachable", status: 0, message: expect.any(String) } });
    const missing = process.platform === "win32" ? `\\\\.\\pipe\\ada-test-missing-${process.pid}` : "/nonexistent/ada-missing.sock";
    const dead = createBridge({ socketPath: missing, pageUrl });
    bridges.push(dead);
    expect(await dead.handle(sender().event(), "listTasks", [])).toEqual({ ok: false, error: { code: "coordinator_unreachable", status: 0, message: expect.any(String) } });
  });

  it("pushes a subscription's messages to its sender and closes the stream on unsubscribe or when the sender is destroyed", async () => {
    const closed: number[] = [];
    let count = 0;
    const { fake, bridge } = await bridgeTo((_req, res) => {
      const n = ++count;
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("retry: 2000\n\n");
      res.write('id: 1\nevent: accepted\ndata: {"ok":true}\n\n');
      res.on("close", () => closed.push(n));
    });
    const s = sender(7);
    expect(await bridge.handle(s.event(), "subscribe", ["s1", "a1"])).toEqual({ ok: true, value: null });
    await until(() => s.sent.length >= 2, "the first messages");
    expect(s.sent).toEqual([
      ["ada:event", "s1", { type: "status", status: "connected" }],
      ["ada:event", "s1", { type: "event", event: { seq: 1, kind: "accepted", payload: { ok: true } } }],
    ]);
    expect(fake.requests[0]?.url).toBe("/v1/attempts/a1/events");

    const other = sender(8);
    expect(await bridge.handle(other.event(), "unsubscribe", ["s1"])).toEqual({ ok: false, error: { code: "malformed_call", status: 0, message: expect.any(String) } });
    expect(closed).toEqual([]);
    expect(await bridge.handle(s.event(), "unsubscribe", ["s1"])).toEqual({ ok: true, value: null });
    await until(() => closed.includes(1), "the stream to close");

    expect(await bridge.handle(s.event(), "subscribe", ["s2", "a1"])).toEqual({ ok: true, value: null });
    await until(() => count === 2, "a second stream");
    const before = s.sent.length;
    s.destroy();
    await until(() => closed.includes(2), "the stream to close with its sender");
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(s.sent.length).toBe(before);
    const messages = s.sent.map((args) => args[2] as StreamMessage);
    expect(messages.every((m) => m.type === "status" || m.type === "event")).toBe(true);
  });

  it("refuses a subscription id already in use by the same sender", async () => {
    const { bridge } = await bridgeTo((_req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("retry: 2000\n\n");
    });
    const s = sender();
    expect(await bridge.handle(s.event(), "subscribe", ["s1", "a1"])).toEqual({ ok: true, value: null });
    expect(await bridge.handle(s.event(), "subscribe", ["s1", "a2"])).toEqual({ ok: false, error: { code: "malformed_call", status: 0, message: expect.any(String) } });
  });
});
