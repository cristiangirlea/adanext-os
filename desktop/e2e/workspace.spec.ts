import { expect } from "@playwright/test";
import { createHash } from "node:crypto";
import { copyFile, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { call } from "../src/adad";
import { exec, executable, fixtureTest } from "./fixture";
import { screenText } from "./live/guard";

// The M4.4b flow in the real window against the real coordinator: import -> task -> fake engine
// -> checks -> changes -> accept. The repository is a fixture this test builds in its own
// temporary directory; the folder comes from main's dialog, which the test answers inside main.
// The engine is the fake engine behind editengine, which writes the agent's changes.

async function git(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec("git", args, { cwd, windowsHide: true });
  return stdout;
}

fixtureTest("import, task, fake engine, checks, changes and accept through the workspace", async ({ fixture: f }) => {
  fixtureTest.setTimeout(180_000);
  // The fixture repository: one commit, an unstaged change, an untracked note and a credential.
  const repo = await mkdtemp(path.join(os.tmpdir(), "ada-e2e-repo-"));
  try {
    await git(repo, "init", "-q", "-b", "main");
    await git(repo, "config", "user.email", "fixture@example.invalid");
    await git(repo, "config", "user.name", "Fixture");
    await git(repo, "config", "core.autocrlf", "false");
    await writeFile(path.join(repo, "README.md"), "# fixture\n");
    await writeFile(path.join(repo, "app.txt"), "one\n");
    await git(repo, "add", "-A");
    await git(repo, "commit", "-q", "-m", "start");
    await writeFile(path.join(repo, "app.txt"), "one\ntwo\n");
    await writeFile(path.join(repo, "notes.txt"), "a note\n");
    await writeFile(path.join(repo, ".env"), "TOKEN=not-a-real-token\n");
    const before = await git(repo, "status", "--porcelain");

    // The engine: editengine in front of the fake engine, its hash given to the test build.
    const engine = path.join(f.state, process.platform === "win32" ? "editengine.exe" : "editengine");
    await copyFile(executable("editengine"), engine);
    await writeFile(engine + ".json", JSON.stringify({ Engine: f.env.ADANEXT_ENGINE }));
    f.env.ADANEXT_ENGINE = engine;
    f.env.ADANEXT_ENGINE_SHA256 = createHash("sha256").update(await readFile(engine)).digest("hex");

    const { app, page } = await f.launch();
    // Answer the native folder dialog inside main, as the owner would.
    await app.evaluate(({ dialog }, folder) => {
      dialog.showOpenDialog = (async () => ({ canceled: false, filePaths: [folder] })) as typeof dialog.showOpenDialog;
    }, repo);

    await page.getByRole("button", { name: "Import a repository", exact: true }).click();
    const importer = page.getByRole("dialog", { name: "Import a repository" });
    await importer.getByRole("button", { name: "Choose a folder", exact: true }).click();
    await expect(importer.getByText("Include these changes in the new feature branch")).toBeVisible();
    await expect(importer.getByText("app.txt")).toBeVisible();
    const credential = importer.locator("label").filter({ hasText: ".env" });
    await expect(credential).toContainText("looks like a credential");
    await expect(credential.getByRole("checkbox")).not.toBeChecked();
    await expect(importer.locator("label").filter({ hasText: "notes.txt" }).getByRole("checkbox")).toBeChecked();
    await importer.getByRole("button", { name: "Import and take the snapshot", exact: true }).click();

    await expect(importer.getByRole("radio", { name: /readonly/ })).toBeChecked();
    await expect(importer.getByRole("radio", { name: /yolo/ })).not.toBeChecked();
    await expect(importer.getByText("Checks are not sandboxed.")).toBeVisible();
    // The live demonstration finds each policy select inside its label, as here.
    for (const name of ["Backend", "Time limit", "Turn limit"]) await expect(importer.locator("label").filter({ hasText: new RegExp(`^${name}`) }).locator("select"), name).toHaveCount(1);
    await importer.getByLabel("Objective", { exact: true }).fill("complete the workspace flow");
    // The live demonstration's screen guard, run in the page, reads form values too.
    expect(await page.locator("body").evaluate(screenText)).toContain("complete the workspace flow");
    await importer.getByRole("button", { name: "Add a check", exact: true }).click();
    await importer.getByLabel("Name", { exact: true }).fill("git works");
    await importer.getByLabel("Command", { exact: true }).fill("git --version");
    await importer.getByRole("button", { name: "Create", exact: true }).click();
    await expect(importer).toBeHidden();

    await expect(page.getByRole("heading", { level: 2, name: "complete the workspace flow" })).toBeVisible();
    const task = (await f.tasks()).find((t) => t.objective === "complete the workspace flow")!;
    expect(task.snapshot).toBeTruthy();
    // The snapshot took the chosen untracked file and left the credential out.
    const snapshot = (await call(f.socket, { method: "GET", path: `/v1/projects/${task.project}/snapshots/${task.snapshot}` })) as { untracked: { path: string }[]; unstaged_patch: { size: number } };
    expect(snapshot.untracked.map((u) => u.path)).toEqual(["notes.txt"]);
    expect(snapshot.unstaged_patch.size).toBeGreaterThan(0);
    expect(task.checks).toMatchObject([{ name: "git works", argv: ["git", "--version"] }]);
    await page.getByRole("button", { name: "Ready", exact: true }).click();
    await page.getByRole("button", { name: "Start", exact: true }).click();
    await expect(page.getByRole("contentinfo")).toContainText("claude, default model, readonly");

    // The fake engine ends ok, Ada commits the edits, runs the check and waits for review.
    const row = page.getByRole("row").filter({ hasText: "complete the workspace flow" });
    await expect(row).toContainText("awaiting_review", { timeout: 90_000 });
    await expect(page.locator("li[data-seq]").first()).toBeVisible();

    await page.getByRole("tab", { name: /Checks/ }).click();
    const panel = page.getByRole("tabpanel");
    await expect(panel.getByText("Checks are not sandboxed.")).toBeVisible();
    await expect(panel.locator("article.check")).toContainText("passed");
    await panel.getByRole("button", { name: "Show the log of git works" }).click();
    await expect(panel.getByLabel("Log of git works")).toContainText("git version");

    await page.getByRole("tab", { name: "Changes" }).click();
    const readme = panel.locator(".file-change").filter({ hasText: "README.md" });
    await expect(readme.locator(".patch-add").last()).toHaveText("+Edited by the agent: <b>bold</b> <script>alert(1)</script>");
    expect(await panel.locator("b, script").count()).toBe(0);
    const hidden = panel.locator(".file-change").filter({ hasText: "U+202E" });
    await expect(hidden).toContainText("hidden characters");

    await page.getByRole("tab", { name: "Files" }).click();
    await expect(panel.getByRole("button", { name: "notes/" })).toBeVisible();
    await panel.getByRole("button", { name: "README.md" }).click();
    await expect(panel.getByLabel("Content of README.md")).toContainText("<b>bold</b>");

    await page.getByRole("tab", { name: "Changes" }).click();
    await expect(readme).toBeVisible();
    await page.getByRole("button", { name: "Accept", exact: true }).click();
    await expect(row).toContainText("accepted");
    expect((await f.tasks()).find((t) => t.id === task.id)?.state).toBe("accepted");

    // Ada worked in its own clone: the owner's checkout is as it was.
    expect(await git(repo, "status", "--porcelain")).toBe(before);
    expect(await readFile(path.join(repo, "README.md"), "utf8")).toBe("# fixture\n");
  } finally {
    await rm(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }
});

fixtureTest("the bridge refuses a path the page types, and asks main's dialog instead", async ({ fixture: f }) => {
  const { app, page } = await f.launch();
  await app.evaluate(({ dialog }) => {
    dialog.showOpenDialog = (async () => ({ canceled: true, filePaths: [] })) as typeof dialog.showOpenDialog;
  });
  // The page itself calls the bridge with a path, as a compromised renderer could.
  const refused = await page.evaluate(async (folder) => {
    const ada = (globalThis as unknown as { ada: Record<string, (...args: unknown[]) => Promise<{ ok: boolean; error?: { code: string } }>> }).ada;
    return Promise.all([
      ada.inspectFolder!(folder),
      ada.importFolder!(folder, "x"),
      (ada.importFolder as (...args: unknown[]) => Promise<{ ok: boolean; error?: { code: string } }>)({ path: folder, name: "x" }),
    ]);
  }, os.tmpdir());
  for (const result of refused) {
    expect(result.ok).toBe(false);
    expect(["folder_expired", "malformed_call", "path_refused"]).toContain(result.error!.code);
  }
  expect(refused[2]!.error!.code).toBe("path_refused");
  const chosen = await page.evaluate(async () => (globalThis as unknown as { ada: { chooseFolder(): Promise<unknown> } }).ada.chooseFolder());
  expect(chosen).toEqual({ ok: true, value: null });
  // Nothing was imported or created.
  expect((await f.tasks()).length).toBe(0);
});
