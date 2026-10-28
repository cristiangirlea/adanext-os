import { randomUUID } from "node:crypto";
import path from "node:path";
import type { BridgeResult, CheckDefinition, ExecutionPolicy, NewTask, StreamMessage } from "../../ui/src/client/api";
import { BadResponse, call, callText, isUnreachable, listingLimit, Refusal } from "./adad";
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
  finishQuit?: () => Promise<void>;
  // Opens the native folder dialog for the calling window: the chosen path, or null. A path
  // enters only this way (decisions log, 2026-10-20).
  chooseFolder?: (event: BridgeEvent) => Promise<string | null>;
  now?: () => number;
}

export interface Bridge {
  handle(event: BridgeEvent, method: unknown, args: unknown): Promise<BridgeResult<unknown>>;
  close(): void;
}

const idPattern = /^[A-Za-z0-9_-]{1,128}$/;
const commitPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const modes = new Set<ExecutionPolicy["mode"]>(["readonly", "auto", "yolo"]);
// A chosen folder's handle lives this long, for the inspect, import, snapshot and create calls.
const folderTtlMs = 15 * 60 * 1000;
// The most of a check log the renderer is sent; the whole log stays an artifact in adad.
export const checkLogLimit = 1 << 20;
const changesLimit = 32 << 20;
const filesLimit = 4 << 20;
const maxFinding = 16 << 10;
const maxCommitPath = 4096;
const maxSelected = 1000;

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
function isStrings(value: unknown, max: number): value is string[] {
  return Array.isArray(value) && value.length <= max && value.every((v) => typeof v === "string");
}
// The shape of a declared check; adad checks its limits (M4.2).
function isCheck(value: unknown): value is CheckDefinition {
  return (
    isRecord(value) &&
    hasKeys(value, ["argv"], ["name", "workdir", "timeout", "max_output_bytes", "env"]) &&
    isStrings(value.argv, 256) &&
    value.argv.length > 0 &&
    (value.name === undefined || typeof value.name === "string") &&
    (value.workdir === undefined || typeof value.workdir === "string") &&
    (value.timeout === undefined || typeof value.timeout === "string") &&
    (value.max_output_bytes === undefined || (typeof value.max_output_bytes === "number" && Number.isSafeInteger(value.max_output_bytes))) &&
    (value.env === undefined || isStrings(value.env, 64))
  );
}
function isNewTask(value: unknown): value is NewTask {
  return (
    isRecord(value) &&
    hasKeys(value, ["project", "objective"], ["criteria", "snapshot", "checks"]) &&
    typeof value.project === "string" &&
    value.project !== "" &&
    typeof value.objective === "string" &&
    value.objective !== "" &&
    (value.criteria === undefined || typeof value.criteria === "string") &&
    (value.snapshot === undefined || isId(value.snapshot)) &&
    (value.checks === undefined || (Array.isArray(value.checks) && value.checks.length <= 16 && value.checks.every(isCheck)))
  );
}
function isSelection(value: unknown): value is { untracked: string[]; include_sensitive: string[]; include_changes?: boolean } {
  return (
    isRecord(value) &&
    hasKeys(value, ["untracked", "include_sensitive"], ["include_changes"]) &&
    isStrings(value.untracked, maxSelected) &&
    isStrings(value.include_sensitive, maxSelected) &&
    (value.include_changes === undefined || typeof value.include_changes === "boolean")
  );
}
function isRefine(value: unknown): boolean {
  if (!isRecord(value) || typeof value.acknowledge_provider_changes !== "boolean") return false;
  const policy: Record<string, unknown> = { ...value };
  delete policy.acknowledge_provider_changes;
  return isPolicy(policy);
}
// A path inside a commit, as the files route takes it: never a host path, which it cannot reach.
function isCommitPath(value: unknown): value is string {
  return typeof value === "string" && value.length <= maxCommitPath && !value.includes("\u0000");
}
// A folder from the dialog: an absolute local path. A UNC or device path is refused, as adad
// refuses it.
function isLocalFolder(value: unknown): value is string {
  if (typeof value !== "string" || value === "" || value.includes("\u0000")) return false;
  if (process.platform === "win32") return /^[A-Za-z]:[\\/]/.test(value);
  return path.posix.isAbsolute(value);
}
// carriesPath finds a `path` field in a call's arguments: the renderer never sends one.
function carriesPath(args: unknown[]): boolean {
  return args.some((arg) => arg !== null && typeof arg === "object" && !Array.isArray(arg) && Object.prototype.hasOwnProperty.call(arg, "path"));
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

interface Folder {
  path: string;
  senderId: number;
  expires: number;
  project?: string;
  snapshot?: string;
}

export function createBridge(options: BridgeOptions): Bridge {
  const now = options.now ?? Date.now;
  const folders = new Map<string, Folder>();
  const channel = options.eventChannel ?? "ada:event";
  const openStream = options.openStream ?? openEventStream;
  const streams = new Map<string, { stream: EventStream; senderId: number }>();
  const watched = new Set<number>();
  let closed = false;
  let stopAccepted = false;
  let stopRequest: Promise<BridgeResult<unknown>> | undefined;

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

  // send runs one request to adad and maps every failure the same way: adad's refusal with its
  // request id and details, an unreadable answer, an unreachable coordinator.
  const send = async (request: (socketPath: string) => Promise<unknown>): Promise<BridgeResult<unknown>> => {
    if (options.socketPath === undefined) return refused("coordinator_unreachable", "the shell was not told adad's pipe or socket");
    try {
      return { ok: true, value: await request(options.socketPath) };
    } catch (error) {
      if (error instanceof Refusal) {
        const refusal: BridgeResult<never> = { ok: false, error: { code: error.code, status: error.status, message: error.message } };
        if (error.requestId !== undefined) refusal.error.request_id = error.requestId;
        if (error.details !== undefined) refusal.error.details = error.details;
        return refusal;
      }
      if (error instanceof BadResponse) return refused(error.code, error.message);
      if (isUnreachable(error)) return refused("coordinator_unreachable", `adad is not reachable at ${options.socketPath}`);
      return refused("internal", error instanceof Error ? error.message : String(error));
    }
  };
  const forward = (request: Parameters<typeof call>[1]): Promise<BridgeResult<unknown>> => send((socketPath) => call(socketPath, request));
  const forwardText = (urlPath: string): Promise<BridgeResult<unknown>> => send((socketPath) => callText(socketPath, urlPath, checkLogLimit));

  // The folder a handle stands for, to the window that chose it, while the handle lives.
  const folder = (event: BridgeEvent, handle: string): Folder | undefined => {
    const entry = folders.get(handle);
    if (!entry) return undefined;
    if (entry.expires <= now()) {
      folders.delete(handle);
      return undefined;
    }
    return entry.senderId === event.sender.id ? entry : undefined;
  };
  const expired = (): BridgeResult<never> => refused("folder_expired", "choose the folder again: this choice has expired or belongs to another window");

  const chooseFolder = async (event: BridgeEvent): Promise<BridgeResult<unknown>> => {
    if (!options.chooseFolder) return refused("malformed_call", "this shell has no folder dialog");
    const chosen = await options.chooseFolder(event);
    if (chosen === null) return { ok: true, value: null };
    if (!isLocalFolder(chosen)) return refused("malformed_call", "the folder must be a local absolute path");
    for (const [h, entry] of folders) if (entry.expires <= now()) folders.delete(h);
    const handle = randomUUID();
    folders.set(handle, { path: chosen, senderId: event.sender.id, expires: now() + folderTtlMs });
    return { ok: true, value: { handle, path: chosen } };
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
      if (carriesPath(args)) return refused("path_refused", "a folder enters only through the desktop's folder dialog");
      switch (method) {
        case "createTask": {
          if (args.length !== 1 || !isNewTask(args[0])) return bad();
          const input = args[0];
          const result = await forward({ method: "POST", path: "/v1/tasks", body: input, idempotent: true });
          // A folder's handle has done its work once a task holds its snapshot.
          if (result.ok && input.snapshot !== undefined) {
            for (const [h, entry] of folders) if (entry.snapshot === input.snapshot && entry.senderId === event.sender.id) folders.delete(h);
          }
          return result;
        }
        case "chooseFolder":
          if (args.length !== 0) return bad();
          return chooseFolder(event);
        case "inspectFolder": {
          if (args.length !== 1 || !isId(args[0])) return bad();
          const entry = folder(event, args[0]);
          if (!entry) return expired();
          return forward({ method: "POST", path: "/v1/projects/inspect", body: { path: entry.path }, limit: changesLimit });
        }
        case "importFolder": {
          if (args.length !== 2 || !isId(args[0]) || typeof args[1] !== "string" || args[1].length > 200) return bad();
          const entry = folder(event, args[0]);
          if (!entry) return expired();
          const result = await forward({ method: "POST", path: "/v1/projects", body: { path: entry.path, name: args[1] }, idempotent: true });
          const project = result.ok ? (result.value as { id?: unknown } | null)?.id : result.error.code === "already_imported" ? result.error.details?.project : undefined;
          if (isId(project)) entry.project = project;
          return result;
        }
        case "snapshot": {
          if (args.length !== 2 || !isId(args[0]) || !isSelection(args[1])) return bad();
          const entry = folder(event, args[0]);
          if (!entry) return expired();
          if (entry.project === undefined) return refused("malformed_call", "import the folder before taking its snapshot");
          const result = await forward({ method: "POST", path: `/v1/projects/${entry.project}/snapshots`, body: args[1], idempotent: true });
          const id = result.ok ? (result.value as { id?: unknown } | null)?.id : undefined;
          if (isId(id)) entry.snapshot = id;
          return result;
        }
        case "capabilities":
          if (args.length !== 0) return bad();
          return forward({ method: "GET", path: "/v1/capabilities" });
        case "changes":
        case "checks":
        case "findings":
          if (args.length !== 1 || !isId(args[0])) return bad();
          return forward({ method: "GET", path: `/v1/tasks/${args[0]}/${method}`, limit: changesLimit });
        case "files":
          if (args.length !== 2 || !isId(args[0]) || !isCommitPath(args[1])) return bad();
          return forward({ method: "GET", path: `/v1/tasks/${args[0]}/files?path=${encodeURIComponent(args[1])}`, limit: filesLimit });
        case "checkLog":
          if (args.length !== 2 || !isId(args[0]) || !isId(args[1])) return bad();
          return forwardText(`/v1/tasks/${args[0]}/checks/${args[1]}/log`);
        case "addFinding":
          if (args.length !== 2 || !isId(args[0]) || typeof args[1] !== "string" || args[1].trim() === "" || args[1].length > maxFinding) return bad();
          return forward({ method: "POST", path: `/v1/tasks/${args[0]}/findings`, body: { body: args[1] }, idempotent: true });
        case "accept":
          if (args.length !== 4 || !isId(args[0]) || !isRevision(args[1]) || typeof args[2] !== "string" || !commitPattern.test(args[2]) || !isRevision(args[3])) return bad();
          return forward({ method: "POST", path: `/v1/tasks/${args[0]}/accept`, revision: args[1], body: { commit: args[2], generation: args[3] }, idempotent: true });
        case "refine":
          if (args.length !== 3 || !isId(args[0]) || !isRevision(args[1]) || !isRefine(args[2])) return bad();
          return forward({ method: "POST", path: `/v1/tasks/${args[0]}/refine`, revision: args[1], body: args[2], idempotent: true });
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
          // Repeated clicks share the accepted stop; a refusal permits a retry.
          stopRequest ??= (async () => {
            const result = await forward({ method: "POST", path: "/v1/coordinator/stop", idempotent: true });
            if (result.ok) stopAccepted = true;
            else stopRequest = undefined;
            return result;
          })();
          return stopRequest;
        }
        case "finishQuit":
          if (args.length !== 0 || !stopAccepted || !options.finishQuit) return bad();
          try {
            await options.finishQuit();
            return { ok: true, value: null };
          } catch (error) {
            return refused("stop_timeout", error instanceof Error ? error.message : String(error));
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
