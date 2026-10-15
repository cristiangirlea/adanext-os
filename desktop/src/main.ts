import { app, BrowserWindow, ipcMain, Menu } from "electron";
import path from "node:path";
import os from "node:os";
import { pathToFileURL } from "node:url";
import { createBridge } from "./bridge";
import { coordinatorConfig, currentSID, endpoint, findOrStart, waitForExit } from "./adad";

// The Electron main process: one window loading the React app built in ../ui, a typed bridge to
// adad over its per-user pipe or socket. adad is detached, with no inherited stdio;
// closing all windows only closes this process and its subscriptions.

function socketFromArgs(argv: string[], env: NodeJS.ProcessEnv): string | undefined {
  for (const arg of argv) {
    if (arg.startsWith("--adad-socket=")) return arg.slice("--adad-socket=".length) || undefined;
  }
  return env.ADA_ADAD_SOCKET || undefined;
}

const indexPath = path.join(__dirname, "..", "..", "ui", "dist", "index.html");
const pageUrl = pathToFileURL(indexPath).href;
let bridge: ReturnType<typeof createBridge> | undefined;

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

void app.whenReady().then(async () => {
  const config = coordinatorConfig(process.env, path.dirname(app.getPath("exe")), os.homedir());
  const suppliedSocket = socketFromArgs(process.argv, process.env);
  const socketPath = suppliedSocket ?? endpoint(config, process.platform, process.platform === "win32" ? await currentSID() : undefined);
  bridge = createBridge({ socketPath, pageUrl, finishQuit: async () => {
    await waitForExit(socketPath);
    // Keep the returned counts and reason visible before the window disappears.
    await new Promise((resolve) => setTimeout(resolve, 1500));
    app.quit();
  } });
  ipcMain.handle("ada:call", (event, method: unknown, args: unknown) => bridge!.handle(event, method, args));
  Menu.setApplicationMenu(Menu.buildFromTemplate([{ label: "Ada", submenu: [{ label: "Quit Ada and stop work", click: () => {
    const window = BrowserWindow.getFocusedWindow() ?? BrowserWindow.getAllWindows()[0];
    window?.webContents.send("ada:quit-requested");
  } }] }]));
  // Only an app launch starts adad. Existing windows reconnect without restarting
  // it, so another window cannot undo the owner's explicit stop.
  try { if (!suppliedSocket) await findOrStart(config, socketPath); } catch (error) { console.error("ada:", error); }
  createWindow();
  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow();
  });
}).catch((error: unknown) => { console.error("ada:", error); app.quit(); });

// Closing the window stops nothing: adad runs on its own and keeps the work going. Only
// "Quit Ada and stop work" on the page asks adad to stop.
app.on("window-all-closed", () => {
  bridge?.close();
  app.quit();
});
