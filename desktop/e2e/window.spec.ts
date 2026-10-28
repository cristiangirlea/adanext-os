import { expect, type Page } from "@playwright/test";
import { createHash } from "node:crypto";
import { copyFile, readFile, writeFile } from "node:fs/promises";
import { readFileSync } from "node:fs";
import path from "node:path";
import { endpointPresent, findOrStart, probeCoordinator, spawnCoordinator } from "../src/adad";
import { openEventStream } from "../src/sse";
import { exec, executable, fixtureTest, type Holder } from "./fixture";

// A task without an imported repository, through the navigation's "New task" form.
async function createAndStart(page: Page, objective: string) {
  await page.getByRole("button", { name: "New task", exact: true }).click();
  await page.getByLabel("Project", { exact: true }).fill("project");
  await page.getByLabel("Objective", { exact: true }).fill(objective);
  await page.getByRole("button", { name: "Create", exact: true }).click();
  // The created task is selected: its heading names it.
  await expect(page.getByRole("heading", { level: 2, name: objective, exact: true })).toBeVisible();
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

fixtureTest("synchronized cold starts both connect to exactly one real coordinator", async ({ fixture: f }) => {
  let arrived = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const contender = () => {
    let first = true;
    return findOrStart(f.config(), f.socket, {
      probe: async (socket, timeout) => {
        const found = await probeCoordinator(socket, timeout);
        if (first) {
          first = false;
          expect(found).toBe(false);
          if (++arrived === 2) release();
          await gate;
        }
        return found;
      }, spawn: spawnCoordinator, now: Date.now, sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
    });
  };
  await Promise.all([contender(), contender()]);
  expect(await probeCoordinator(f.socket)).toBe(true);
  const holder = await f.holder();
  expect(holder).not.toBeNull();
  expect(await f.processCount()).toBe(1);
  await Promise.all([findOrStart(f.config(), f.socket), findOrStart(f.config(), f.socket)]);
  expect(await f.holder()).toEqual(holder);
  expect(await f.processCount()).toBe(1);
});

fixtureTest("Quit keeps Electron and the endpoint alive until the delayed engine exits", async ({ fixture: f }) => {
  const originalEngine = f.env.ADANEXT_ENGINE!;
  f.env.ADANEXT_ENGINE = path.join(f.state, process.platform === "win32" ? "slowengine.exe" : "slowengine");
  await copyFile(executable("slowengine"), f.env.ADANEXT_ENGINE);
  f.env.ADANEXT_ENGINE_SHA256 = createHash("sha256").update(await readFile(f.env.ADANEXT_ENGINE)).digest("hex");
  const marker = path.join(f.state, "engine-exit.json");
  await writeFile(f.env.ADANEXT_ENGINE + ".json", JSON.stringify({ engine: originalEngine, marker }));
  const { app, page } = await f.launch();
  await createAndStart(page, "delayed cancellation");
  await expect.poll(async () => (await f.tasks())[0]?.state).toBe("running");
  const started = JSON.parse(await readFile(marker + ".started", "utf8")) as Holder;
  expect(started.PID).toBeGreaterThan(0);
  expect(started.Created).not.toBe("");
  let exited = false;
  let markerAtClose: Holder | undefined;
  app.process().once("exit", () => {
    try { markerAtClose = JSON.parse(readFileSync(marker, "utf8")); } catch { /* Assert below. */ }
  });
  app.on("close", () => { exited = true; });
  const closed = app.waitForEvent("close").then(async () => {
    f.apps = f.apps.filter((a) => a !== app);
    // Compare the OS identity, not just a potentially reused PID.
    if (process.platform === "win32") {
      const script = `$p = Get-Process -Id ${started.PID} -ErrorAction SilentlyContinue; if ($p) { $p.StartTime.ToFileTimeUtc().ToString() }; exit 0`;
      const { stdout } = await exec(path.join(process.env.SystemRoot!, "System32/WindowsPowerShell/v1.0/powershell.exe"), ["-NoProfile", "-NonInteractive", "-Command", script], { windowsHide: true });
      expect(stdout.trim()).not.toBe(started.Created);
    } else {
      const stat = await readFile(`/proc/${started.PID}/stat`, "utf8").catch(() => "");
      expect(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? "").not.toBe(started.Created);
    }
  });
  await page.getByRole("button", { name: "Quit Ada and stop work", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("coordinator stopped by the owner");
  await page.waitForTimeout(2000);
  expect(exited).toBe(false);
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]!.getTitle())).toBe("Ada - waiting for work to stop");
  expect(await endpointPresent(f.socket)).toBe(true);
  await expect(readFile(marker)).rejects.toThrow();
  await closed;
  expect(markerAtClose).toEqual(started);
  f.apps = f.apps.filter((a) => a !== app);
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
