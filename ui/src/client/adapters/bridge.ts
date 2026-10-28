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
  throw new AdaError(result.error.code, result.error.status, result.error.message, result.error.request_id, result.error.details);
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
    stopCoordinator: async () => unwrap(await bridge().stopCoordinator()),
    finishQuit: async () => { unwrap(await bridge().finishQuit()); },
    onQuitRequested: (listener) => bridge().onQuitRequested(listener),
    capabilities: async () => unwrap(await bridge().capabilities()),
    chooseFolder: async () => unwrap(await bridge().chooseFolder()),
    inspectFolder: async (handle) => unwrap(await bridge().inspectFolder(handle)),
    importFolder: async (handle, name) => unwrap(await bridge().importFolder(handle, name)),
    snapshot: async (handle, selection) => unwrap(await bridge().snapshot(handle, selection)),
    changes: async (taskId) => unwrap(await bridge().changes(taskId)),
    files: async (taskId, path) => unwrap(await bridge().files(taskId, path)),
    checks: async (taskId) => unwrap(await bridge().checks(taskId)),
    checkLog: async (taskId, checkId) => unwrap(await bridge().checkLog(taskId, checkId)),
    findings: async (taskId) => unwrap(await bridge().findings(taskId)),
    addFinding: async (taskId, body) => unwrap(await bridge().addFinding(taskId, body)),
    accept: async (taskId, revision, commit, generation) => unwrap(await bridge().accept(taskId, revision, commit, generation)),
    refine: async (taskId, revision, request) => unwrap(await bridge().refine(taskId, revision, request)),
  };
}
