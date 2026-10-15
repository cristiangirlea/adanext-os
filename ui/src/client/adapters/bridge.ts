import { AdaError, type AdaClient, type BridgeApi, type BridgeResult } from "../api";

// The desktop adapter: the preload bridge exposes window.ada (BridgeApi) and Electron's main
// process forwards each call to adad over the pipe or socket. This is the only module that may
// name window.ada (ui/src/boundary.test.ts enforces it).

declare global {
  interface Window {
    ada?: BridgeApi;
  }
}

function bridge(): BridgeApi {
  const api = window.ada;
  if (!api) throw new AdaError("bridge_unavailable", 0, "this page is not running in the Ada desktop shell");
  return api;
}

function unwrap<T>(result: BridgeResult<T>): T {
  if (result.ok) return result.value;
  throw new AdaError(result.error.code, result.error.status, result.error.message);
}

export function createBridgeClient(): AdaClient {
  return {
    createTask: async (input) => unwrap(await bridge().createTask(input)),
    listTasks: async () => unwrap(await bridge().listTasks()),
    getTask: async (id) => unwrap(await bridge().getTask(id)),
    ready: async (id, revision) => unwrap(await bridge().ready(id, revision)),
    start: async (id, revision, policy) => unwrap(await bridge().start(id, revision, policy)),
    cancel: async (id, revision) => unwrap(await bridge().cancel(id, revision)),
    subscribeEvents: (attemptId, onMessage) => bridge().subscribeEvents(attemptId, onMessage),
    stopCoordinator: async () => {
      unwrap(await bridge().stopCoordinator());
    },
  };
}
