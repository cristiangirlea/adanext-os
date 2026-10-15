import type { BridgeResult, ExecutionPolicy, NewTask, StreamMessage } from "../../ui/src/client/api";
import { BadResponse, call, isUnreachable, listingLimit, Refusal } from "./adad";
import { openEventStream, type EventStream, type StreamOptions } from "./sse";

// The typed bridge between the renderer and adad. Every call is checked to come from the Ada
// page's top frame, its arguments are checked for shape, and then it is forwarded as the
// matching /v1 request; adad's refusal comes back as adad's code. The bridge decides no
// permission of its own (decisions log, 2026-10-14, #5).

// The subset of Electron's WebFrameMain, WebContents and IpcMainInvokeEvent the bridge reads;
// tests pass plain objects of the same shape.
export interface FrameLike {
  url: string;
  parent: unknown;
}
export interface NavigationLike {
  isMainFrame: boolean;
  isSameDocument: boolean;
}
export interface SenderLike {
  id: number;
  send(channel: string, ...args: unknown[]): void;
  on(event: "did-start-navigation", listener: (details: NavigationLike) => void): unknown;
  on(event: "render-process-gone", listener: () => void): unknown;
  once(event: "destroyed", listener: () => void): unknown;
  isDestroyed(): boolean;
}
export interface BridgeEvent {
  senderFrame: FrameLike | null;
  sender: SenderLike;
}

export interface BridgeOptions {
  // The pipe or socket adad listens on; undefined when the shell was not told one.
  socketPath: string | undefined;
  // The Ada page's URL; only its top frame may call.
  pageUrl: string;
  // The channel events are pushed on. The preload listens on it.
  eventChannel?: string;
  openStream?: (options: StreamOptions) => EventStream;
}

export interface Bridge {
  handle(event: BridgeEvent, method: unknown, args: unknown): Promise<BridgeResult<unknown>>;
  close(): void;
}

const idPattern = /^[A-Za-z0-9_-]{1,128}$/;
const modes = new Set<ExecutionPolicy["mode"]>(["readonly", "auto", "yolo"]);

function refused(code: string, message: string): BridgeResult<never> {
  return { ok: false, error: { code, status: 0, message } };
}

function isId(value: unknown): value is string {
  return typeof value === "string" && idPattern.test(value);
}
function isRevision(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}
function isRecord(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const proto: unknown = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}
function hasKeys(value: Record<string, unknown>, required: string[], optional: string[] = []): boolean {
  const keys = Object.keys(value);
  return required.every((k) => keys.includes(k)) && keys.every((k) => required.includes(k) || optional.includes(k));
}
function isNewTask(value: unknown): value is NewTask {
  return (
    isRecord(value) &&
    hasKeys(value, ["project", "objective"], ["criteria"]) &&
    typeof value.project === "string" &&
    value.project !== "" &&
    typeof value.objective === "string" &&
    value.objective !== "" &&
    (value.criteria === undefined || typeof value.criteria === "string")
  );
}
function isPolicy(value: unknown): value is ExecutionPolicy {
  return (
    isRecord(value) &&
    hasKeys(value, ["backend", "model", "effort", "mode"]) &&
    typeof value.backend === "string" &&
    typeof value.model === "string" &&
    typeof value.effort === "string" &&
    typeof value.mode === "string" &&
    modes.has(value.mode as ExecutionPolicy["mode"])
  );
}

function sameTopFrame(frame: FrameLike | null, pageUrl: string): boolean {
  if (frame === null || frame.parent !== null || typeof frame.url !== "string") return false;
  try {
    const url = new URL(frame.url);
    url.hash = "";
    return url.href === pageUrl;
  } catch {
    return false;
  }
}

export function createBridge(options: BridgeOptions): Bridge {
  const channel = options.eventChannel ?? "ada:event";
  const openStream = options.openStream ?? openEventStream;
  const streams = new Map<string, { stream: EventStream; senderId: number }>();
  const watched = new Set<number>();
  let closed = false;

  const key = (senderId: number, subId: string): string => `${senderId}:${subId}`;

  // A subscription belongs to the document that opened it. The streams of a sender close when
  // its top-frame document is replaced (a reload recreates the preload, which starts its ids
  // over), when its renderer process is gone, and when the WebContents is destroyed.
  const dropStreams = (senderId: number): void => {
    for (const [k, entry] of streams) {
      if (entry.senderId === senderId) {
        entry.stream.close();
        streams.delete(k);
      }
    }
  };
  const watch = (sender: SenderLike): void => {
    if (watched.has(sender.id)) return;
    watched.add(sender.id);
    sender.on("did-start-navigation", (details) => {
      if (details.isMainFrame && !details.isSameDocument) dropStreams(sender.id);
    });
    sender.on("render-process-gone", () => dropStreams(sender.id));
    sender.once("destroyed", () => {
      dropStreams(sender.id);
      watched.delete(sender.id);
    });
  };

  const forward = async (request: Parameters<typeof call>[1]): Promise<BridgeResult<unknown>> => {
    if (options.socketPath === undefined) return refused("coordinator_unreachable", "the shell was not told adad's pipe or socket");
    try {
      return { ok: true, value: await call(options.socketPath, request) };
    } catch (error) {
      if (error instanceof Refusal) {
        const refusal: BridgeResult<never> = { ok: false, error: { code: error.code, status: error.status, message: error.message } };
        if (error.requestId !== undefined) refusal.error.request_id = error.requestId;
        return refusal;
      }
      if (error instanceof BadResponse) return refused(error.code, error.message);
      if (isUnreachable(error)) return refused("coordinator_unreachable", `adad is not reachable at ${options.socketPath}`);
      return refused("internal", error instanceof Error ? error.message : String(error));
    }
  };

  const subscribe = (event: BridgeEvent, subId: string, attemptId: string): BridgeResult<null> => {
    const sender = event.sender;
    const k = key(sender.id, subId);
    if (streams.has(k)) return refused("malformed_call", `subscription ${subId} is already open`);
    if (options.socketPath === undefined) return refused("coordinator_unreachable", "the shell was not told adad's pipe or socket");
    const push = (message: StreamMessage): void => {
      if (closed || !streams.has(k) || sender.isDestroyed()) return;
      sender.send(channel, subId, message);
    };
    const stream = openStream({ socketPath: options.socketPath, attemptId, onMessage: push });
    streams.set(k, { stream, senderId: sender.id });
    watch(sender);
    return { ok: true, value: null };
  };

  const unsubscribe = (event: BridgeEvent, subId: string): BridgeResult<null> => {
    const k = key(event.sender.id, subId);
    const entry = streams.get(k);
    if (!entry) return refused("malformed_call", `no subscription ${subId} for this sender`);
    entry.stream.close();
    streams.delete(k);
    return { ok: true, value: null };
  };

  return {
    async handle(event, method, args) {
      if (closed) return refused("internal", "the bridge is closed");
      if (!sameTopFrame(event.senderFrame, options.pageUrl)) return refused("unexpected_sender", "the call did not come from the Ada page's top frame");
      if (typeof method !== "string" || !Array.isArray(args)) return refused("malformed_call", "a method name and an argument list are required");
      const bad = (): BridgeResult<never> => refused("malformed_call", `malformed arguments for ${method}`);
      switch (method) {
        case "createTask":
          if (args.length !== 1 || !isNewTask(args[0])) return bad();
          return forward({ method: "POST", path: "/v1/tasks", body: args[0], idempotent: true });
        case "listTasks":
          if (args.length !== 0) return bad();
          return forward({ method: "GET", path: "/v1/tasks", limit: listingLimit });
        case "getTask":
          if (args.length !== 1 || !isId(args[0])) return bad();
          return forward({ method: "GET", path: `/v1/tasks/${args[0]}` });
        case "ready":
        case "cancel":
          if (args.length !== 2 || !isId(args[0]) || !isRevision(args[1])) return bad();
          return forward({ method: "POST", path: `/v1/tasks/${args[0]}/${method}`, revision: args[1], idempotent: true });
        case "start":
          if (args.length !== 3 || !isId(args[0]) || !isRevision(args[1]) || !isPolicy(args[2])) return bad();
          return forward({ method: "POST", path: `/v1/tasks/${args[0]}/start`, revision: args[1], body: args[2], idempotent: true });
        case "stopCoordinator": {
          if (args.length !== 0) return bad();
          const result = await forward({ method: "POST", path: "/v1/coordinator/stop", idempotent: true, emptyOk: true });
          return result.ok ? { ok: true, value: null } : result;
        }
        case "subscribe":
          if (args.length !== 2 || !isId(args[0]) || !isId(args[1])) return bad();
          return subscribe(event, args[0], args[1]);
        case "unsubscribe":
          if (args.length !== 1 || !isId(args[0])) return bad();
          return unsubscribe(event, args[0]);
        default:
          return refused("malformed_call", `unknown method ${method}`);
      }
    },
    close() {
      closed = true;
      for (const entry of streams.values()) entry.stream.close();
      streams.clear();
    },
  };
}
