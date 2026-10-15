import { app, BrowserWindow, ipcMain } from "electron";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { createBridge } from "./bridge";

// The Electron main process: one window loading the React app built in ../ui, a typed bridge to
// adad over its pipe or socket, and nothing else. Finding or starting adad is M2.4c: until then
// the shell is told the pipe or socket by ADA_ADAD_SOCKET or --adad-socket=<path>, and without
// one every call answers coordinator_unreachable, which the page shows.

function socketFromArgs(argv: string[], env: NodeJS.ProcessEnv): string | undefined {
  for (const arg of argv) {
    if (arg.startsWith("--adad-socket=")) return arg.slice("--adad-socket=".length) || undefined;
  }
  return env.ADA_ADAD_SOCKET || undefined;
}

const indexPath = path.join(__dirname, "..", "..", "ui", "dist", "index.html");
const pageUrl = pathToFileURL(indexPath).href;
const socketPath = socketFromArgs(process.argv, process.env);
if (socketPath === undefined) {
  console.error("ada: no coordinator pipe or socket given (ADA_ADAD_SOCKET or --adad-socket=); the page will say the coordinator is unreachable");
}

const bridge = createBridge({ socketPath, pageUrl });
ipcMain.handle("ada:call", (event, method: unknown, args: unknown) => bridge.handle(event, method, args));

function createWindow(): void {
  const window = new BrowserWindow({
    width: 1100,
    height: 720,
    title: "Ada",
    webPreferences: {
      preload: path.join(__dirname, "preload.cjs"),
      contextIsolation: true,
      sandbox: true,
      nodeIntegration: false,
      webSecurity: true,
      allowRunningInsecureContent: false,
    },
  });
  // The page is local and loads nothing remote: no navigation away from it, no new windows.
  window.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  window.webContents.on("will-navigate", (event) => event.preventDefault());
  void window.loadFile(indexPath);
}

app.on("web-contents-created", (_event, contents) => {
  contents.on("will-attach-webview", (event) => event.preventDefault());
});

void app.whenReady().then(() => {
  createWindow();
  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow();
  });
});

// Closing the window stops nothing: adad runs on its own and keeps the work going. Only
// "Quit Ada and stop work" on the page asks adad to stop.
app.on("window-all-closed", () => {
  bridge.close();
  app.quit();
});
