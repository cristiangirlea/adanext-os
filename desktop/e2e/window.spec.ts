import { _electron, expect, test, type ElectronApplication, type Page } from "@playwright/test";
import { createHash, randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { call, coordinatorConfig, currentSID, endpoint, endpointPresent, findOrStart, waitForExit } from "../src/adad";
import { openEventStream } from "../src/sse";
import type { Task, AttemptEvent } from "../../ui/src/client/api";

const exec = promisify(execFile);
const bin = process.env.ADANEXT_E2E_BIN;
if (!bin) throw new Error("ADANEXT_E2E_BIN must name the directory built by scripts/test-e2e.sh");
const executable = (name: string) => path.join(bin, name + (process.platform === "win32" ? ".exe" : ""));

interface Holder { PID: number; Created: string }
class Fixture {
  state = "";
  name = `e2e-${randomUUID()}`;
  socket = "";
  env: Record<string, string> = {};
  apps: ElectronApplication[] = [];
  async init() {
    this.state = await mkdtemp(path.join(os.tmpdir(), "ada-e2e-"));
    await exec(executable("testseed"), ["seed", this.state]);
    const hash = createHash("sha256").update(await readFile(executable("fakeengine"))).digest("hex");
    this.env = Object.fromEntries(Object.entries(process.env).filter((entry): entry is [string, string] => entry[1] !== undefined && entry[0] !== "ELECTRON_RUN_AS_NODE" && entry[0] !== "ADA_ADAD_SOCKET"));
    Object.assign(this.env, { ADANEXT_ADAD: executable("adad"), ADANEXT_STATE_DIR: this.state, ADANEXT_NAME: this.name, ADANEXT_ENGINE: executable("fakeengine"), ADANEXT_ENGINE_SHA256: hash });
    this.socket = endpoint(this.config(), process.platform, process.platform === "win32" ? await currentSID() : undefined);
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
      await call(this.socket, { method: "POST", path: "/v1/coordinator/stop", idempotent: true }).catch(() => {});
      await waitForExit(this.socket);
    }
    await expect.poll(() => this.holder(), { timeout: 30_000 }).toBeNull();
    // This is the fixture's own mkdtemp directory, after all owners have exited.
    await rm(this.state, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }
}

const fixtureTest = test.extend<{ fixture: Fixture }>({ fixture: async ({}, use) => {
  const fixture = new Fixture();
  await fixture.init();
  try { await use(fixture); } finally { await fixture.dispose(); }
} });

async function createAndStart(page: Page, objective: string) {
  await page.getByLabel("Project", { exact: true }).fill("project");
  await page.getByLabel("Objective", { exact: true }).fill(objective);
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await page.getByRole("button", { name: "Ready", exact: true }).click();
  await page.getByRole("button", { name: "Start", exact: true }).click();
}

fixtureTest("closing the window keeps adad and events alive; reopening replays every event", async ({ fixture: f }) => {
  const { app, page } = await f.launch();
  await createAndStart(page, "keep running across windows");
  await expect(page.locator("li[data-seq]").first()).toBeVisible();
  await expect.poll(async () => (await f.tasks())[0]?.state).toBe("running");
  const task = (await f.tasks())[0]!;
  const holder = await f.holder();
  expect(holder).not.toBeNull();
  if (process.platform === "win32") {
    const electron = JSON.parse((await exec(executable("testseed"), ["job", String(app.process().pid)])).stdout);
    const adad = JSON.parse((await exec(executable("testseed"), ["job", String(holder!.PID)])).stdout);
    console.log(`Windows job observation: Electron in_job=${electron.in_job}, adad in_job=${adad.in_job}`);
    // A host/tool-owned ancestor job can remain after breakaway. The native
    // adad tests destroy a desktop-owned job and verify the coordinator survives;
    // here the real window close proves that boundary in Electron.
  }
  const before = (await f.events(task.attempt!.id)).length;
  await f.closeWindow(app);
  const received: number[] = [];
  const stream = openEventStream({ socketPath: f.socket, attemptId: task.attempt!.id, onMessage: (m) => { if (m.type === "event") received.push(m.event.seq); } });
  try {
    await expect.poll(() => received.length).toBeGreaterThan(before + 5);
    expect(await f.holder()).toEqual(holder);
    expect((await f.tasks())[0]!.state).toBe("running");
  } finally { stream.close(); }
  const whileClosed = await f.events(task.attempt!.id);
  const reopened = await f.launch();
  await reopened.page.getByRole("cell", { name: task.objective, exact: true }).click();
  await expect(reopened.page.locator(`li[data-seq="${whileClosed.at(-1)!.seq}"]`)).toBeVisible();
  const seqs = await reopened.page.locator("li[data-seq]").evaluateAll((els) => els.map((e) => Number(e.getAttribute("data-seq"))));
  expect(seqs).toEqual(Array.from({ length: seqs.length }, (_, n) => n + 1));
  expect(seqs.slice(0, whileClosed.length)).toEqual(whileClosed.map((e) => e.seq));
});

fixtureTest("Quit cancels running and queued work, shows its reason, and exits; reopened tasks retain both", async ({ fixture: f }) => {
  const { app, page } = await f.launch();
  await createAndStart(page, "running task");
  await expect.poll(async () => (await f.tasks()).find((t) => t.objective === "running task")?.state).toBe("running");
  await createAndStart(page, "queued task");
  await expect(page.getByText("Queue position 1", { exact: true })).toBeVisible();
  const closed = app.waitForEvent("close");
  await page.getByRole("button", { name: "Quit Ada and stop work", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("coordinator stopped by the owner: 1 running cancellation requested, 1 queued canceled");
  await closed;
  f.apps = f.apps.filter((a) => a !== app);
  await expect.poll(() => endpointPresent(f.socket)).toBe(false);
  await expect.poll(() => f.holder()).toBeNull();
  const reopened = await f.launch();
  for (const objective of ["running task", "queued task"]) {
    const row = reopened.page.getByRole("row").filter({ hasText: objective });
    await expect(row).toContainText("canceled");
    await row.click();
    await expect(reopened.page.locator("li").filter({ hasText: "coordinator stopped by the owner" })).toBeVisible();
  }
});

fixtureTest("a second app finds the same coordinator; the menu uses the explicit Quit flow", async ({ fixture: f }) => {
  const first = await f.launch();
  const holder = await f.holder();
  const second = await f.launch();
  expect(await f.holder()).toEqual(holder);
  expect(await f.processCount()).toBe(1);
  await createAndStart(first.page, "complete successfully");
  // M2 records a successful engine result as verifying; independent verification is later.
  await expect.poll(async () => (await f.tasks())[0]?.state).toBe("verifying");
  expect((await f.tasks())[0]?.attempt?.state).toBe("ended_ok");
  // Keep the first window and its selected event stream open during Quit.
  const closed = second.app.waitForEvent("close");
  await second.app.evaluate(({ Menu }) => {
    const item = Menu.getApplicationMenu()!.items[0]!.submenu!.items.find((entry) => entry.label === "Quit Ada and stop work")!;
    item.click();
  });
  await expect(second.page.getByRole("status")).toContainText("coordinator stopped by the owner");
  await closed;
  f.apps = f.apps.filter((a) => a !== second.app);
  await expect.poll(() => f.holder()).toBeNull();
  await first.page.waitForTimeout(2000);
  expect(await endpointPresent(f.socket)).toBe(false);
  await f.closeWindow(first.app);
});

fixtureTest("the UI shows queue_full and withdraws a queued task before it launches", async ({ fixture: f }) => {
  const { page } = await f.launch();
  await createAndStart(page, "running");
  await expect.poll(async () => (await f.tasks())[0]?.state).toBe("running");
  for (let n = 1; n <= 3; n++) {
    await createAndStart(page, `queued ${n}`);
    await expect(page.getByText(`Queue position ${n}`, { exact: true })).toBeVisible();
  }
  await createAndStart(page, "queue overflow");
  await expect(page.getByRole("alert")).toContainText("queue_full");
  await page.getByRole("cell", { name: "queued 2", exact: true }).click();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("row").filter({ hasText: "queued 2" })).toContainText("canceled");
  const withdrawn = (await f.tasks()).find((t) => t.objective === "queued 2")!;
  const events = await f.events(withdrawn.attempt!.id);
  expect(events.map((e) => e.kind)).toEqual(["cancel"]);
  await page.getByRole("cell", { name: "queue overflow", exact: true }).click();
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await expect(page.getByText("Queue position 3", { exact: true })).toBeVisible();
});

fixtureTest("Node reconnects after a coordinator restart during a replay longer than one page, with each sequence once", async ({ fixture: f }) => {
  await exec(executable("testseed"), ["replay", f.state]);
  await findOrStart(f.config(), f.socket);
  const holder = (await f.holder())!;
  const received: number[] = [];
  let killed = false;
  let reconnects = 0;
  const stream = openEventStream({ socketPath: f.socket, attemptId: "replay-attempt", onUnavailable: async () => {
    reconnects++;
    await findOrStart(f.config(), f.socket);
  }, onMessage: (message) => {
    if (message.type !== "event") return;
    received.push(message.event.seq);
    if (!killed) { killed = true; process.kill(holder.PID, "SIGKILL"); }
  } });
  try {
    await expect.poll(() => received.length, { timeout: 30_000 }).toBe(900);
    expect(received).toEqual(Array.from({ length: 900 }, (_, n) => n + 1));
    expect(reconnects).toBeGreaterThan(0);
    expect(await f.holder()).not.toEqual(holder);
  } finally { stream.close(); }
});
