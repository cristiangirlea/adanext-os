import type http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import type { StreamMessage } from "../../ui/src/client/api";
import { openEventStream, SSEParser, type SSEMessage } from "./sse";
import { listenFake, until, type FakeAdad } from "./test/fakeAdad";

const fakes: FakeAdad[] = [];
afterEach(async () => {
  for (const fake of fakes.splice(0)) await fake.close();
});

function parseAll(chunks: string[]): { messages: SSEMessage[]; retries: number[] } {
  const messages: SSEMessage[] = [];
  const retries: number[] = [];
  const parser = new SSEParser({ onMessage: (m) => messages.push(m), onRetry: (ms) => retries.push(ms) });
  for (const chunk of chunks) parser.feed(chunk);
  return { messages, retries };
}

describe("SSEParser", () => {
  it("reassembles id, event and data across chunk boundaries and line endings", () => {
    const { messages } = parseAll(["id: 1\nev", "ent: accepted\r\ndata: {\"a\":", "1}\n\n", "id: 2\nevent: progress\ndata: one\ndata: two\n\n"]);
    expect(messages).toEqual([
      { id: "1", event: "accepted", data: '{"a":1}' },
      { id: "2", event: "progress", data: "one\ntwo" },
    ]);
  });

  it("ignores comments, keeps retry out of messages and dispatches nothing without data", () => {
    const { messages, retries } = parseAll(["retry: 2000\n\n: ping\n\nid: 7\nevent: x\n\n", "data: alone\n\n", "retry: nope\n\n"]);
    expect(retries).toEqual([2000]);
    expect(messages).toEqual([{ id: undefined, event: "", data: "alone" }]);
  });

  it("drops an id holding a NUL and reads a field with no space after the colon", () => {
    const { messages } = parseAll(["id:\u00003\nevent:kind\ndata:x\n\n"]);
    expect(messages).toEqual([{ id: undefined, event: "kind", data: "x" }]);
  });
});

function sse(res: http.ServerResponse, retry?: number): void {
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
  if (retry !== undefined) res.write(`retry: ${retry}\n\n`);
}
function event(res: http.ServerResponse, seq: number, kind: string, payload: unknown): void {
  res.write(`id: ${seq}\nevent: ${kind}\ndata: ${JSON.stringify(payload)}\n\n`);
}
function lastEventId(req: http.IncomingMessage): string | undefined {
  const value = req.headers["last-event-id"];
  return Array.isArray(value) ? value.join(",") : value;
}

describe("openEventStream", () => {
  it("reconnects with Last-Event-ID after the server's retry delay and continues from there", async () => {
    const responses: http.ServerResponse[] = [];
    const fake = await listenFake((req, res) => {
      responses.push(res);
      sse(res, 20);
      if (responses.length === 1) {
        event(res, 1, "accepted", { ok: true });
        event(res, 2, "progress", "half");
        setTimeout(() => res.destroy(), 10);
      } else {
        event(res, 3, "result", null);
      }
    });
    fakes.push(fake);
    const messages: StreamMessage[] = [];
    const delays: number[] = [];
    const stream = openEventStream({
      socketPath: fake.socketPath,
      attemptId: "a1",
      onMessage: (m) => messages.push(m),
      schedule: (fn, ms) => {
        delays.push(ms);
        const timer = setTimeout(fn, ms);
        return () => clearTimeout(timer);
      },
    });
    await until(() => messages.some((m) => m.type === "event" && m.event.seq === 3), "the event after the reconnect");
    stream.close();
    expect(fake.requests.map(lastEventId)).toEqual([undefined, "2"]);
    expect(fake.requests.map((r) => r.url)).toEqual(["/v1/attempts/a1/events", "/v1/attempts/a1/events"]);
    expect(fake.requests[0]?.headers.accept).toBe("text/event-stream");
    expect(delays).toEqual([20]);
    expect(messages.filter((m) => m.type === "event").map((m) => (m.type === "event" ? [m.event.seq, m.event.kind, m.event.payload] : null))).toEqual([
      [1, "accepted", { ok: true }],
      [2, "progress", "half"],
      [3, "result", null],
    ]);
    expect(messages.filter((m) => m.type === "status").map((m) => (m.type === "status" ? m.status : null))).toEqual(["connected", "reconnecting", "connected"]);
  });

  it("backs off from 0.5 s doubling to 10 s without a retry line, then honours one", async () => {
    let count = 0;
    const fake = await listenFake((_req, res) => {
      count++;
      if (count <= 7) {
        sse(res);
        res.end();
      } else {
        sse(res, 7);
        res.end();
      }
    });
    fakes.push(fake);
    const delays: number[] = [];
    const stream = openEventStream({
      socketPath: fake.socketPath,
      attemptId: "a1",
      onMessage: () => {},
      schedule: (fn, ms) => {
        delays.push(ms);
        const timer = setTimeout(fn, 0);
        return () => clearTimeout(timer);
      },
    });
    await until(() => delays.length >= 9, "nine reconnects");
    stream.close();
    expect(delays.slice(0, 9)).toEqual([500, 1000, 2000, 4000, 8000, 10000, 10000, 7, 7]);
  });

  it("reports an unreachable coordinator and keeps trying", async () => {
    const delays: number[] = [];
    const messages: StreamMessage[] = [];
    const missing = process.platform === "win32" ? `\\\\.\\pipe\\ada-test-missing-${process.pid}` : "/nonexistent/ada-missing.sock";
    const stream = openEventStream({
      socketPath: missing,
      attemptId: "a1",
      onMessage: (m) => messages.push(m),
      schedule: (fn, ms) => {
        delays.push(ms);
        const timer = setTimeout(fn, 0);
        return () => clearTimeout(timer);
      },
    });
    await until(() => delays.length >= 2, "two attempts");
    stream.close();
    expect(messages[0]).toEqual({ type: "status", status: "unreachable", code: "coordinator_unreachable" });
    expect(delays.slice(0, 2)).toEqual([500, 1000]);
  });

  it("on cursor_ahead closes, discards the cursor and reopens from 0 at once without the header", async () => {
    let count = 0;
    const fake = await listenFake((_req, res) => {
      count++;
      sse(res, 20);
      if (count === 1) {
        event(res, 1, "accepted", {});
        event(res, 2, "progress", {});
        event(res, 3, "progress", {});
        setTimeout(() => res.destroy(), 10);
      } else if (count === 2) {
        res.write('event: cursor_ahead\ndata: {"last_sequence":1}\n\n');
        res.end();
      } else {
        event(res, 1, "accepted", { again: true });
      }
    });
    fakes.push(fake);
    const messages: StreamMessage[] = [];
    const delays: number[] = [];
    const stream = openEventStream({
      socketPath: fake.socketPath,
      attemptId: "a1",
      onMessage: (m) => messages.push(m),
      schedule: (fn, ms) => {
        delays.push(ms);
        const timer = setTimeout(fn, ms);
        return () => clearTimeout(timer);
      },
    });
    await until(() => fake.requests.length === 3 && messages.some((m) => m.type === "event" && m.event.seq === 1 && m.event.payload !== null && typeof m.event.payload === "object" && "again" in m.event.payload), "the replay from 0");
    stream.close();
    expect(fake.requests.map(lastEventId)).toEqual([undefined, "3", undefined]);
    expect(delays).toEqual([20]);
    const kinds = messages.map((m) => (m.type === "event" ? `event:${m.event.seq}` : m.type === "reset" ? `reset:${m.last_sequence}` : `status:${m.status}`));
    expect(kinds).toEqual(["status:connected", "event:1", "event:2", "event:3", "status:reconnecting", "status:connected", "reset:1", "status:connected", "event:1"]);
  });

  it("treats a refused open as a reconnect with the server's code", async () => {
    let count = 0;
    const fake = await listenFake((_req, res) => {
      count++;
      if (count === 1) {
        res.writeHead(429, { "Content-Type": "application/json" });
        res.end('{"error":"too_many_streams","message":"too many streams"}');
      } else {
        sse(res, 20);
        event(res, 1, "accepted", {});
      }
    });
    fakes.push(fake);
    const messages: StreamMessage[] = [];
    const stream = openEventStream({
      socketPath: fake.socketPath,
      attemptId: "a1",
      onMessage: (m) => messages.push(m),
      schedule: (fn) => {
        const timer = setTimeout(fn, 0);
        return () => clearTimeout(timer);
      },
    });
    await until(() => messages.some((m) => m.type === "event"), "an event after the refusal");
    stream.close();
    expect(messages[0]).toEqual({ type: "status", status: "reconnecting", code: "too_many_streams" });
  });

  it("stops reconnecting once closed", async () => {
    const fake = await listenFake((_req, res) => {
      sse(res, 5);
      res.end();
    });
    fakes.push(fake);
    const delays: number[] = [];
    const stream = openEventStream({
      socketPath: fake.socketPath,
      attemptId: "a1",
      onMessage: () => {},
      schedule: (fn, ms) => {
        delays.push(ms);
        const timer = setTimeout(fn, ms);
        return () => clearTimeout(timer);
      },
    });
    await until(() => delays.length >= 1, "one reconnect");
    stream.close();
    const seen = fake.requests.length;
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(fake.requests.length).toBeLessThanOrEqual(seen + 1);
    const after = fake.requests.length;
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(fake.requests.length).toBe(after);
  });
});
