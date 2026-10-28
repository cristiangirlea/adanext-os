import type { BridgeApi, BridgeResult, StreamMessage } from "../../ui/src/client/api";

// The preload's api (BridgeApi) over an injected ipcRenderer, so it can be tested without
// Electron. Each method is forwarded to main on one channel as a method name and an argument
// list; main checks the sender frame and the arguments, then talks to adad. Events come back
// keyed by a subscription id this side hands out; the renderer keeps no cursor.

export interface Ipc {
  invoke(method: string, args: unknown[]): Promise<BridgeResult<unknown>>;
  onEvent(listener: (subId: unknown, message: unknown) => void): void;
  onQuitRequested?(listener: () => void): () => void;
}

// documentId scopes the subscription ids to this document: a reload recreates the preload and
// its counter while main may still hold the old document's streams, so a bare counter would
// hand out an id main knows and the old attempt's events would reach the new page.
export function createApi(ipc: Ipc, documentId: string): BridgeApi {
  const listeners = new Map<string, (message: StreamMessage) => void>();
  let next = 0;

  ipc.onEvent((subId, message) => {
    if (typeof subId !== "string") return;
    listeners.get(subId)?.(message as StreamMessage);
  });

  const invoke = <T>(method: string, args: unknown[]): Promise<BridgeResult<T>> => ipc.invoke(method, args) as Promise<BridgeResult<T>>;

  return {
    createTask: (input) => invoke("createTask", [input]),
    listTasks: () => invoke("listTasks", []),
    getTask: (id) => invoke("getTask", [id]),
    ready: (id, revision) => invoke("ready", [id, revision]),
    start: (id, revision, policy) => invoke("start", [id, revision, policy]),
    cancel: (id, revision) => invoke("cancel", [id, revision]),
    subscribeEvents: (attemptId, onMessage) => {
      const subId = `s${documentId}-${++next}`;
      listeners.set(subId, onMessage);
      void invoke<null>("subscribe", [subId, attemptId]).then((result) => {
        // A refused subscription has no stream behind it: the listener goes, so nothing is
        // delivered to it later and the unsubscribe has nothing to tell main.
        if (!result.ok && listeners.delete(subId)) onMessage({ type: "status", status: "unreachable", code: result.error.code });
      });
      return () => {
        if (!listeners.delete(subId)) return;
        void invoke("unsubscribe", [subId]);
      };
    },
    stopCoordinator: () => invoke("stopCoordinator", []),
    finishQuit: () => invoke("finishQuit", []),
    onQuitRequested: (listener) => ipc.onQuitRequested?.(listener) ?? (() => {}),
    capabilities: () => invoke("capabilities", []),
    // Main opens the dialog; nothing the page passes here reaches it.
    chooseFolder: () => invoke("chooseFolder", []),
    inspectFolder: (handle) => invoke("inspectFolder", [handle]),
    importFolder: (handle, name) => invoke("importFolder", [handle, name]),
    snapshot: (handle, selection) => invoke("snapshot", [handle, selection]),
    changes: (taskId) => invoke("changes", [taskId]),
    files: (taskId, path) => invoke("files", [taskId, path]),
    checks: (taskId) => invoke("checks", [taskId]),
    checkLog: (taskId, checkId) => invoke("checkLog", [taskId, checkId]),
    findings: (taskId) => invoke("findings", [taskId]),
    addFinding: (taskId, body) => invoke("addFinding", [taskId, body]),
    accept: (taskId, revision, commit, generation) => invoke("accept", [taskId, revision, commit, generation]),
    refine: (taskId, revision, request) => invoke("refine", [taskId, revision, request]),
  };
}
