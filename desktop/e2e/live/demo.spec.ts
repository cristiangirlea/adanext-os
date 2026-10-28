import { _electron, expect, test, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { call, coordinatorConfig, currentSID, endpoint, endpointPresent, waitForExit } from "../../src/adad";
import type { AttemptEvent, Changes, CheckResult, Task } from "../../../ui/src/client/api";
import { liveEnabled, personalText, preflightRecord, screenText, underHome } from "./guard";

// M4.5: the whole workflow through the real app on the owner's plan, with the release adad and the
// pinned engine. Run by scripts/live-demo.sh; never by check.sh or CI (the default config ignores
// this folder, and the spec skips itself without ADANEXT_LIVE=1). The sign-in is proven first by
// cmd/ada-live -preflight-only; every screenshot is kept only when the page held no e-mail-shaped
// text, home directory or user name both before and after it was taken.

const exec = promisify(execFile);
const windows = process.platform === "win32";

test.skip(!liveEnabled(process.env), "the live demonstration runs only with ADANEXT_LIVE=1");

// The fixed tiny task: a failing test the agent fixes, and the same test as the task's check.
const objective = "Fix the add function in sum.js so that node --test sum.test.js passes. Change only sum.js.";
const criteria = "node --test sum.test.js passes.";
const timeLimit = "5m";
const turnLimit = 5;

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} must be set (scripts/live-demo.sh sets it)`);
  return value;
}

// git without the owner's global and system configuration (signing, hooks), as cmd/ada-live runs it.
const gitEnv = { ...process.env, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: windows ? "NUL" : "/dev/null" };
async function git(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec("git", args, { cwd, env: gitEnv, windowsHide: true });
  return stdout;
}

// The fixture repository: one commit with a failing test.
async function writeFixture(repo: string) {
  await mkdir(repo, { recursive: true });
  await git(repo, "init", "-q", "-b", "main");
  // No e-mail-shaped identity, so none can reach the screen.
  await git(repo, "config", "user.email", "fixture");
  await git(repo, "config", "user.name", "Fixture");
  await git(repo, "config", "core.autocrlf", "false");
  await writeFile(path.join(repo, "README.md"), "# demo\n\nA fixture for AdaNext's live demonstration.\n");
  await writeFile(path.join(repo, "sum.js"), "function add(a, b) {\n  return a - b;\n}\n\nmodule.exports = { add };\n");
  await writeFile(
    path.join(repo, "sum.test.js"),
    'const test = require("node:test");\nconst assert = require("node:assert/strict");\nconst { add } = require("./sum.js");\n\ntest("add adds", () => {\n  assert.equal(add(2, 3), 5);\n});\n',
  );
  await git(repo, "add", "-A");
  await git(repo, "commit", "-q", "-m", "a failing test");
}

test("the whole workflow through the app on a real backend", async () => {
  const backend = process.env.ADANEXT_LIVE_BACKEND || "codex";
  if (backend !== "claude" && backend !== "codex") throw new Error("ADANEXT_LIVE_BACKEND must be claude or codex");
  const bin = required("ADANEXT_LIVE_BIN");
  const engine = required("ADANEXT_ENGINE");
  const exe = (name: string) => path.join(bin, name + (windows ? ".exe" : ""));
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const results = process.env.ADANEXT_LIVE_RESULTS || path.resolve("..", ".hrn", "live-demo", `${backend}-${stamp}`);
  await mkdir(results, { recursive: true });
  // The scratch path shows in the import dialog, so it must not lie in the home folder.
  const scratchRoot = required("ADANEXT_LIVE_SCRATCH");
  if (!path.isAbsolute(scratchRoot) || underHome(scratchRoot, [os.homedir(), process.env.HOME ?? "", process.env.USERPROFILE ?? ""])) {
    throw new Error("ADANEXT_LIVE_SCRATCH must be an absolute path outside the home folder");
  }
  await mkdir(scratchRoot, { recursive: true });
  const scratch = await mkdtemp(path.join(scratchRoot, "ada-live-"));
  const record: Record<string, unknown> = { backend, platform: process.platform, started: new Date().toISOString(), screenshots: [] as string[] };
  const save = () => writeFile(path.join(results, "results.json"), JSON.stringify(record, null, 2) + "\n");

  // 1. The sign-in, before anything starts: a plan, no apiKeyHelper, no key in env; masked.
  const preflight = await exec(exe("ada-live"), ["-preflight-only", "-backend", backend, "-engine", engine, "-state-dir", path.join(scratch, "preflight")], { windowsHide: true }).then(
    (r) => r.stdout,
    (e: { stdout?: string }) => {
      const all = (() => {
        try {
          return JSON.parse(e.stdout ?? "") as Record<string, Parameters<typeof preflightRecord>[0]>;
        } catch {
          return {};
        }
      })();
      record.preflight = all[backend] ? preflightRecord(all[backend]) : { outcome: "refused" };
      return save().then(() => {
        throw new Error("the preflight refused the sign-in; see results.json");
      });
    },
  );
  const pre = preflightRecord((JSON.parse(preflight) as Record<string, Parameters<typeof preflightRecord>[0]>)[backend]!);
  expect(pre.outcome).toBe("passed");
  record.preflight = pre;
  record.cli_version = pre.version;
  const status = pre.status;
  record.sign_in = backend === "claude" ? `${(status as Record<string, string>).authMethod} plan (${(status as Record<string, string>).subscriptionType})` : String(status).trim();
  await save();

  // 2. The fixture repository and a scratch state directory.
  const repo = path.join(scratch, "demo-repo");
  await writeFixture(repo);
  const state = path.join(scratch, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });
  const env = Object.fromEntries(
    Object.entries(process.env).filter((e): e is [string, string] => e[1] !== undefined && !["ELECTRON_RUN_AS_NODE", "ADA_ADAD_SOCKET", "ADANEXT_ENGINE_SHA256"].includes(e[0])),
  );
  Object.assign(env, { ADANEXT_ADAD: exe("adad"), ADANEXT_STATE_DIR: state, ADANEXT_NAME: `live-${randomUUID().slice(0, 8)}`, ADANEXT_ENGINE: engine });
  const socket = endpoint(coordinatorConfig(env, bin, os.homedir()), process.platform, windows ? await currentSID() : undefined);

  let step = 0;
  const personal = async (page: Page) => personalText(await page.locator("body").evaluate(screenText), os.userInfo().username, os.homedir());
  // Checked before and after the capture, so nothing that arrived in between is kept.
  const shot = async (page: Page, label: string) => {
    expect(await personal(page), "personal details on screen before a screenshot").toEqual([]);
    const image = await page.screenshot();
    expect(await personal(page), "personal details on screen during a screenshot; it was discarded").toEqual([]);
    const file = `${String(++step).padStart(2, "0")}-${label}.png`;
    await writeFile(path.join(results, file), image);
    (record.screenshots as string[]).push(file);
  };

  const app = await _electron.launch({ args: [path.resolve("."), `--user-data-dir=${path.join(scratch, "electron")}`], env });
  let passed = false;
  try {
    const page = await app.firstWindow();
    await expect(page.getByRole("button", { name: "Create", exact: true })).toBeVisible();
    await shot(page, "workspace");

    // 3. Import through main's dialog, answered with the fixture's path in this test only.
    await app.evaluate(({ dialog }, folder) => {
      dialog.showOpenDialog = (async () => ({ canceled: false, filePaths: [folder] })) as typeof dialog.showOpenDialog;
    }, repo);
    await page.getByRole("button", { name: "Import a repository", exact: true }).click();
    const importer = page.getByRole("dialog", { name: "Import a repository" });
    await importer.getByRole("button", { name: "Choose a folder", exact: true }).click();
    const importButton = importer.getByRole("button", { name: "Import and take the snapshot", exact: true });
    await expect(importButton).toBeVisible();
    await shot(page, "inspection");
    await importButton.click();

    // 4. The task: the fixed objective, the backend, mode auto with a time and (claude) turn cap, the check.
    // A select's label also holds its options' text, so the select is found inside its label.
    const select = (name: string) => importer.locator("label").filter({ hasText: new RegExp(`^${name}`) }).locator("select");
    await select("Backend").selectOption(backend);
    await importer.getByRole("radio", { name: /^auto/ }).check();
    await select("Time limit").selectOption(timeLimit);
    if (backend === "claude") await select("Turn limit").selectOption(String(turnLimit));
    await importer.getByLabel("Objective", { exact: true }).fill(objective);
    await importer.getByLabel("Acceptance criteria", { exact: true }).fill(criteria);
    await importer.getByRole("button", { name: "Add a check", exact: true }).click();
    await importer.getByLabel("Name", { exact: true }).fill("tests");
    await importer.getByLabel("Command", { exact: true }).fill("node --test sum.test.js");
    await shot(page, "task");
    await importer.getByRole("button", { name: "Create", exact: true }).click();
    await expect(importer).toBeHidden();
    await expect(page.getByRole("heading", { level: 2, name: objective })).toBeVisible();
    const task = (await call(socket, { method: "GET", path: "/v1/tasks" }) as Task[]).find((t) => t.objective === objective)!;
    record.task = task.id;

    // 5. Ready, Start, and the run under its caps.
    await page.getByRole("button", { name: "Ready", exact: true }).click();
    await page.getByRole("button", { name: "Start", exact: true }).click();
    const began = Date.now();
    await expect(page.getByRole("contentinfo")).toContainText(`${backend}, default model, auto`);
    await shot(page, "running");
    const row = page.getByRole("row").filter({ hasText: objective });
    await expect(row).toContainText(/awaiting_review|failed|canceled|interrupted|recovery_required/, { timeout: 5 * 60_000 + 3 * 60_000 });
    record.duration_seconds = Math.round((Date.now() - began) / 1000);
    const ended = (await call(socket, { method: "GET", path: `/v1/tasks/${task.id}` })) as Task;
    record.task_state = ended.state;
    record.attempt_state = ended.attempt?.state;
    if (ended.attempt) {
      const events: AttemptEvent[] = [];
      for (;;) {
        const page = (await call(socket, { method: "GET", path: `/v1/attempts/${ended.attempt.id}/events?after=${events.at(-1)?.seq ?? 0}` })) as AttemptEvent[];
        if (!page.length) break;
        events.push(...page);
      }
      record.events = events.length;
      record.run_id = events.map((e) => (e.payload as { run_id?: string } | null)?.run_id).find(Boolean) ?? null;
    }
    await shot(page, "review");
    expect(ended.state).toBe("awaiting_review");

    // 6. The check, then the changes.
    await page.getByRole("tab", { name: /Checks/ }).click();
    const panel = page.getByRole("tabpanel");
    await expect(panel.locator("article.check")).toBeVisible();
    const checks = (await call(socket, { method: "GET", path: `/v1/tasks/${task.id}/checks` })) as CheckResult[];
    record.checks = checks.map((c) => ({ name: c.name, state: c.state, exit_code: c.exit_code, baseline: c.baseline?.state ?? null }));
    await shot(page, "checks");
    expect(checks.map((c) => c.state)).toEqual(["passed"]);

    await page.getByRole("tab", { name: "Changes" }).click();
    await expect(panel.locator(".file-change").filter({ hasText: "sum.js" }).first()).toBeVisible();
    const changes = (await call(socket, { method: "GET", path: `/v1/tasks/${task.id}/changes` })) as Changes;
    record.changed_files = changes.files.map((f) => f.path);
    await shot(page, "changes");

    // 7. Accept.
    await page.getByRole("button", { name: "Accept", exact: true }).click();
    await expect(row).toContainText("accepted");
    record.accepted_commit = changes.commit;
    record.task_state = ((await call(socket, { method: "GET", path: `/v1/tasks/${task.id}` })) as Task).state;
    await shot(page, "accepted");
    expect(record.task_state).toBe("accepted");
    // Ada worked in its own clone: the fixture is as it was.
    expect(await git(repo, "status", "--porcelain")).toBe("");
    passed = true;
  } finally {
    await app.close().catch(() => app.process().kill());
    if (await endpointPresent(socket).catch(() => false)) {
      await call(socket, { method: "POST", path: "/v1/coordinator/stop", idempotent: true }).catch(() => {});
      await waitForExit(socket);
    }
    record.finished = new Date().toISOString();
    record.passed = passed;
    await save();
    console.log(`live demonstration results: ${results}`);
    // The scratch stays for inspection when the run failed.
    if (passed) await rm(scratch, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }
});
