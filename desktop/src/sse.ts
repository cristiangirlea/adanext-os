import http from "node:http";
import type { StreamMessage } from "../../ui/src/client/api";
import { errorCode, readJSON } from "./adad";

// Electron's main process has no EventSource, so it reads adad's event stream over the pipe or
// socket with its own parser (decisions log, 2026-10-14, #4): it keeps the last id:, reconnects
// with Last-Event-ID after 0.5 s doubling to 10 s or after the server's retry:, and on
// event: cursor_ahead closes, discards its cursor, tells the renderer to discard its view, and
// reopens from 0 without the header. The renderer keeps no cursor.

export interface SSEMessage {
  id: string | undefined;
  event: string;
  data: string;
}

export class SSEParser {
  private buffer = "";
  private id: string | undefined;
  private event = "";
  private data: string[] = [];

  constructor(private readonly sink: { onMessage: (message: SSEMessage) => void; onRetry: (ms: number) => void }) {}

  feed(chunk: string): void {
    this.buffer += chunk;
    for (;;) {
      const match = /\r\n|\n|\r/.exec(this.buffer);
      if (!match) return;
      // A lone \r at the end of the buffer may be the first half of \r\n: wait for more.
      if (match[0] === "\r" && match.index === this.buffer.length - 1) return;
      const line = this.buffer.slice(0, match.index);
      this.buffer = this.buffer.slice(match.index + match[0].length);
      this.line(line);
    }
  }

  private line(line: string): void {
    if (line === "") {
      this.dispatch();
      return;
    }
    if (line.startsWith(":")) return;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "event":
        this.event = value;
        break;
      case "data":
        this.data.push(value);
        break;
      case "id":
        if (!value.includes("\u0000")) this.id = value;
        break;
      case "retry":
        if (/^[0-9]+$/.test(value)) this.sink.onRetry(Number(value));
        break;
      default:
        break;
    }
  }

  private dispatch(): void {
    const { id, event, data } = this;
    this.id = undefined;
    this.event = "";
    this.data = [];
    if (data.length === 0) return;
    this.sink.onMessage({ id, event, data: data.join("\n") });
  }
}

export interface StreamOptions {
  socketPath: string;
  attemptId: string;
  onMessage: (message: StreamMessage) => void;
  initialDelayMs?: number;
  maxDelayMs?: number;
  // Tests inject the timer to observe the delays; the default is setTimeout.
  schedule?: (fn: () => void, ms: number) => () => void;
}

export interface EventStream {
  close(): void;
}

const defaultSchedule: NonNullable<StreamOptions["schedule"]> = (fn, ms) => {
  const timer = setTimeout(fn, ms);
  return () => clearTimeout(timer);
};

export function openEventStream(options: StreamOptions): EventStream {
  const initial = options.initialDelayMs ?? 500;
  const max = options.maxDelayMs ?? 10_000;
  const schedule = options.schedule ?? defaultSchedule;
  const path = `/v1/attempts/${encodeURIComponent(options.attemptId)}/events`;
  let lastId: string | null = null;
  let retryMs: number | undefined;
  let backoff = initial;
  let closed = false;
  let cancelTimer: (() => void) | undefined;
  let request: http.ClientRequest | undefined;

  const reconnect = (): void => {
    if (closed) return;
    const delay = retryMs ?? backoff;
    if (retryMs === undefined) backoff = Math.min(backoff * 2, max);
    cancelTimer = schedule(connect, delay);
  };

  const connect = (): void => {
    if (closed) return;
    cancelTimer = undefined;
    const headers: http.OutgoingHttpHeaders = { Accept: "text/event-stream" };
    if (lastId !== null) headers["Last-Event-ID"] = lastId;
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      fn();
    };
    const req = http.request({ socketPath: options.socketPath, path, method: "GET", headers }, (res) => {
      if (res.statusCode !== 200) {
        // The refusal's code is wanted; a body that cannot be read leaves the status's own.
        void readJSON(res, 64 * 1024).then((body) => body, () => undefined).then((body) => {
          settle(() => {
            options.onMessage({ type: "status", status: "reconnecting", code: errorCode(body, res.statusCode ?? 0) });
            reconnect();
          });
        });
        return;
      }
      options.onMessage({ type: "status", status: "connected" });
      let reopening = false;
      const parser = new SSEParser({
        onRetry: (ms) => {
          retryMs = ms;
        },
        onMessage: (message) => {
          if (reopening) return;
          // A delivered message is the success that resets the backoff; a stream that opens
          // and dies before delivering anything keeps doubling.
          backoff = initial;
          if (message.event === "cursor_ahead") {
            reopening = true;
            lastId = null;
            let last = 0;
            try {
              const parsed: unknown = JSON.parse(message.data);
              if (parsed !== null && typeof parsed === "object" && typeof (parsed as { last_sequence?: unknown }).last_sequence === "number") {
                last = (parsed as { last_sequence: number }).last_sequence;
              }
            } catch {
              // The reset carries whatever adad said; a malformed body still resets to 0.
            }
            settle(() => {
              res.destroy();
              options.onMessage({ type: "reset", last_sequence: last });
              connect();
            });
            return;
          }
          if (message.id === undefined) return;
          lastId = message.id;
          let payload: unknown;
          try {
            payload = JSON.parse(message.data);
          } catch {
            payload = message.data;
          }
          options.onMessage({ type: "event", event: { seq: Number(message.id), kind: message.event, payload } });
        },
      });
      res.setEncoding("utf8");
      res.on("data", (chunk: string) => parser.feed(chunk));
      res.on("error", () => {});
      res.on("close", () =>
        settle(() => {
          if (closed) return;
          options.onMessage({ type: "status", status: "reconnecting" });
          reconnect();
        }),
      );
    });
    request = req;
    req.on("error", () =>
      settle(() => {
        if (closed) return;
        options.onMessage({ type: "status", status: "unreachable", code: "coordinator_unreachable" });
        reconnect();
      }),
    );
    req.end();
  };

  connect();
  return {
    close() {
      if (closed) return;
      closed = true;
      cancelTimer?.();
      request?.destroy();
    },
  };
}
