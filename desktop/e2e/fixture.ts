import { _electron, expect, test, type ElectronApplication } from "@playwright/test";
import { createHash, randomUUID } from "node:crypto";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, writeFile, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { call, coordinatorConfig, currentSID, endpoint, endpointPresent, waitForExit } from "../src/adad";
import type { Task, AttemptEvent } from "../../ui/src/client/api";

// The end-to-end fixture: a scratch state directory seeded by testseed, the fakeengine build of
// adad, and Electron windows launched against them. Shared by the spec files.

export const exec = promisify(execFile);
const bin = process.env.ADANEXT_E2E_BIN;
if (!bin) throw new Error("ADANEXT_E2E_BIN must name the directory built by scripts/test-e2e.sh");
export const executable = (name: string) => path.join(bin, name + (process.platform === "win32" ? ".exe" : ""));

export async function waitForBrowserCoordinator(child: ChildProcess) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  try {
    await once(child, "exit", { signal: AbortSignal.timeout(10_000) });
  } catch (err) {
    if (!(err instanceof Error) || err.name !== "AbortError") throw err;
    if (child.exitCode !== null || child.signalCode !== null) return;
    const exit = once(child, "exit", { signal: AbortSignal.timeout(10_000) });
    child.kill();
    await exit;
  }
}

export interface Holder { PID: number; Created: string }
export class Fixture {
  state = "";
  name = `e2e-${randomUUID()}`;
  socket = "";
  env: Record<string, string> = {};
  private browserCoordinator?: ChildProcess;
  apps: ElectronApplication[] = [];
  async init() {
    this.state = await mkdtemp(path.join(os.tmpdir(), "ada-e2e-"));
    await exec(executable("testseed"), ["seed", this.state]);
    const hash = createHash("sha256").update(await readFile(executable("fakeengine"))).digest("hex");
    this.env = Object.fromEntries(Object.entries(process.env).filter((entry): entry is [string, string] => entry[1] !== undefined && entry[0] !== "ELECTRON_RUN_AS_NODE" && entry[0] !== "ADA_ADAD_SOCKET"));
    Object.assign(this.env, { ADANEXT_ADAD: executable("adad"), ADANEXT_STATE_DIR: this.state, ADANEXT_NAME: this.name, ADANEXT_ENGINE: executable("fakeengine"), ADANEXT_ENGINE_SHA256: hash });
    this.socket = endpoint(this.config(), process.platform, process.platform === "win32" ? await currentSID() : undefined);
  }
  async serveBrowser(ui: string) {
    const { binary, args } = this.config();
    const child = spawn(binary, [...args.filter(arg => arg !== "-detach"), "-ui", ui], { env: this.env, windowsHide: true, stdio: "ignore" });
    this.browserCoordinator = child;
    await new Promise<void>((resolve, reject) => { child.once("spawn", resolve); child.once("error", reject); });
    await expect.poll(() => endpointPresent(this.socket)).toBe(true);
  }
  async grantRepositoryRoot() {
    const root = await mkdtemp(path.join(this.state, "artifacts", "folders-"));
    const repo = path.join(root, "repo");
    await mkdir(repo);
    const git = (...args: string[]) => exec("git", args, { cwd: repo, windowsHide: true });
    await git("init", "-q", "-b", "main");
    await git("config", "user.email", "fixture@example.invalid");
    await git("config", "user.name", "Fixture");
    await git("config", "core.autocrlf", "false");
    await writeFile(path.join(repo, "README.md"), "# fixture\n");
    await git("add", "-A");
    await git("commit", "-q", "-m", "start");
    await writeFile(path.join(repo, "notes.txt"), "snapshot selection\n");
    const grant = await call(this.socket, { method: "POST", path: "/v1/import-roots", body: { path: root, label: "Granted folders" }, idempotent: true }) as { id: string };
    await call(this.socket, { method: "PUT", path: `/v1/import-roots/${grant.id}/grants/local`, idempotent: true });
  }
  config() { return coordinatorConfig(this.env, bin!, os.homedir()); }
  async launch() {
    const profile = await mkdtemp(path.join(this.state, "electron-"));
    const app = await _electron.launch({ args: [path.resolve("."), `--user-data-dir=${profile}`], env: this.env });
    this.apps.push(app);
    const page = await app.firstWindow();
    await expect(page.getByRole("button", { name: "Create", exact: true })).toBeVisible();
    return { app, page };
  }
  async holder(): Promise<Holder | null> {
    const { stdout } = await exec(executable("testseed"), ["holder", this.state, this.name]);
    return JSON.parse(stdout);
  }
  async processCount(): Promise<number> {
    if (process.platform === "win32") {
      const script = "@(Get-CimInstance Win32_Process -Filter \"Name='adad.exe'\" | Where-Object { $_.ExecutablePath -eq $env:ADANEXT_ADAD -and $_.CommandLine.Contains($env:ADANEXT_STATE_DIR) }).Count";
      const { stdout } = await exec(path.join(process.env.SystemRoot!, "System32/WindowsPowerShell/v1.0/powershell.exe"), ["-NoProfile", "-NonInteractive", "-Command", script], { env: this.env, windowsHide: true });
      return Number(stdout.trim());
    }
    const { stdout } = await exec("ps", ["-eo", "args="]);
    return stdout.split("\n").filter((line) => line.startsWith(this.config().binary + " ") && line.includes(this.state)).length;
  }
  async tasks(): Promise<Task[]> { return await call(this.socket, { method: "GET", path: "/v1/tasks" }) as Task[]; }
  async events(id: string): Promise<AttemptEvent[]> {
    const all: AttemptEvent[] = [];
    for (;;) {
      const page = await call(this.socket, { method: "GET", path: `/v1/attempts/${id}/events?after=${all.at(-1)?.seq ?? 0}` }) as AttemptEvent[];
      all.push(...page);
      if (!page.length) return all;
    }
  }
  async closeWindow(app: ElectronApplication) {
    this.apps = this.apps.filter((a) => a !== app);
    if (app.process().exitCode !== null) return;
    const closed = app.waitForEvent("close", { timeout: 5000 });
    // A failed firstWindow may have no window or an already-exiting process.
    // Observe the promise even if evaluate fails, then let cleanup kill that child.
    void closed.catch(() => {});
    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]?.close());
    await closed;
  }
  async dispose() {
    // Close every window first, so no subscription can restart the test's coordinator.
    for (const app of this.apps.splice(0)) await this.closeWindow(app).catch(() => { app.process().kill(); });
    if (await endpointPresent(this.socket).catch(() => false)) {
      // The stop takes the count of other users' work as it stands; a moved count is read again.
      for (let tries = 0; tries < 3; tries++) {
        const counts = (await call(this.socket, { method: "GET", path: "/v1/coordinator/stop" }).catch(() => null)) as { others_running: number; others_queued: number } | null;
        if (counts === null) break;
        const stopped = await call(this.socket, { method: "POST", path: "/v1/coordinator/stop", body: { others: counts.others_running + counts.others_queued }, idempotent: true }).then(() => true, () => false);
        if (stopped) break;
      }
      await waitForExit(this.socket);
    }
    if (this.browserCoordinator) await waitForBrowserCoordinator(this.browserCoordinator);
    await expect.poll(() => this.holder(), { timeout: 30_000 }).toBeNull();
    // This is the fixture's own mkdtemp directory, after all owners have exited.
    await rm(this.state, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }
}

export const fixtureTest = test.extend<{ fixture: Fixture }>({ fixture: async ({}, use) => {
  const fixture = new Fixture();
  await fixture.init();
  try { await use(fixture); } finally { await fixture.dispose(); }
} });
