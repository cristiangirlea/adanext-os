import { contextBridge, ipcRenderer } from "electron";
import type { BridgeResult } from "../../ui/src/client/api";
import { createApi } from "./preloadApi";

// The preload exposes only the typed AdaClient methods (BridgeApi, built in ./preloadApi)
// through the context bridge. The document id scopes this document's subscription ids.

const api = createApi(
  {
    invoke: (method, args) => ipcRenderer.invoke("ada:call", method, args) as Promise<BridgeResult<unknown>>,
    onEvent: (listener) => {
      ipcRenderer.on("ada:event", (_event, subId: unknown, message: unknown) => listener(subId, message));
    },
    onQuitRequested: (listener) => {
      const receive = () => listener();
      ipcRenderer.on("ada:quit-requested", receive);
      return () => { ipcRenderer.removeListener("ada:quit-requested", receive); };
    },
  },
  crypto.randomUUID(),
);

contextBridge.exposeInMainWorld("ada", api);
