import { contextBridge, ipcRenderer } from "electron";
import type { BridgeApi, BridgeResult, StreamMessage } from "../../ui/src/client/api";

// The preload exposes only the typed AdaClient methods (BridgeApi) through the context bridge.
// Each one is forwarded to main on one channel as a method name and an argument list; main
// checks the sender frame and the arguments, then talks to adad. Events come back on ada:event
// keyed by a subscription id this side hands out; the renderer keeps no cursor.

const listeners = new Map<string, (message: StreamMessage) => void>();
let next = 0;

ipcRenderer.on("ada:event", (_event, subId: unknown, message: unknown) => {
  if (typeof subId !== "string") return;
  listeners.get(subId)?.(message as StreamMessage);
});

function invoke<T>(method: string, args: unknown[]): Promise<BridgeResult<T>> {
  return ipcRenderer.invoke("ada:call", method, args) as Promise<BridgeResult<T>>;
}

const api: BridgeApi = {
  createTask: (input) => invoke("createTask", [input]),
  listTasks: () => invoke("listTasks", []),
  getTask: (id) => invoke("getTask", [id]),
  ready: (id, revision) => invoke("ready", [id, revision]),
  start: (id, revision, policy) => invoke("start", [id, revision, policy]),
  cancel: (id, revision) => invoke("cancel", [id, revision]),
  subscribeEvents: (attemptId, onMessage) => {
    const subId = `s${++next}`;
    listeners.set(subId, onMessage);
    void invoke<null>("subscribe", [subId, attemptId]).then((result) => {
      if (!result.ok && listeners.has(subId)) onMessage({ type: "status", status: "unreachable", code: result.error.code });
    });
    return () => {
      if (!listeners.delete(subId)) return;
      void invoke("unsubscribe", [subId]);
    };
  },
  stopCoordinator: () => invoke("stopCoordinator", []),
};

contextBridge.exposeInMainWorld("ada", api);
