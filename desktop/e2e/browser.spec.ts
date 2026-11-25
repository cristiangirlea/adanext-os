import { chromium, expect, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { mkdtemp } from "node:fs/promises";
import net from "node:net";
import path from "node:path";
import { call, findOrStart } from "../src/adad";
import { exec, executable, fixtureTest } from "./fixture";

// The browser shell against a real adad on its TLS listener: two seeded accounts, each with a
// project holding a task and an empty project, each enrolling with its code in its own browser
// context, signing in, and seeing only its own projects. No request goes to /v1/experience.
// The certificate is a test-only self-signed pair for localhost.

const ui = path.resolve("..", "ui", "dist");

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const port = (server.address() as net.AddressInfo).port;
      server.close(() => resolve(port));
    });
  });
}

interface Code {
  code: string;
}

// adad lists projects by name; the assertion retries until the navigation shows exactly these.
async function expectProjects(page: Page, names: string[]): Promise<void> {
  await expect(page.getByRole("navigation", { name: "Projects and tasks" }).locator("caption .project-name")).toHaveText(names);
}

fixtureTest("two accounts enroll and sign in from their own browsers and each sees only its own projects", async ({ fixture: f }) => {
  fixtureTest.setTimeout(180_000);
  await exec(executable("testseed"), ["users", f.state]);
  const config = f.config();
  await findOrStart({ ...config, args: [...config.args, "-ui", ui] }, f.socket);

  // The listener and its test certificate, set up through the owner's channel.
  const port = await freePort();
  const origin = `https://localhost:${port}`;
  await call(f.socket, { method: "PUT", path: "/v1/listener", body: { enabled: true, address: `127.0.0.1:${port}`, name: "localhost" } });
  const certs = await mkdtemp(path.join(f.state, "certs-"));
  await exec(executable("testseed"), ["cert", certs]);
  const status = (await call(f.socket, { method: "PUT", path: "/v1/listener/certificate", body: { certificate: path.join(certs, "cert.pem"), key: path.join(certs, "key.pem") } })) as { state: string; origin: string; message: string };
  expect(status, status.message).toMatchObject({ state: "serving", origin });

  // The accounts: sam may use a password; the owner's code comes from a recovery.
  await call(f.socket, { method: "PATCH", path: "/v1/accounts/sam", body: { password_allowed: true }, idempotent: true });
  const samCode = (await call(f.socket, { method: "POST", path: "/v1/accounts/sam/enrollment-codes", idempotent: true })) as Code;
  const ownerCode = (await call(f.socket, { method: "POST", path: "/v1/accounts/local/recover", idempotent: true })) as Code;

  let browser: Browser | undefined;
  try {
    browser = await chromium.launch();
    const requests: string[] = [];
    const open = async (): Promise<{ context: BrowserContext; page: Page }> => {
      const context = await browser!.newContext({ ignoreHTTPSErrors: true });
      context.on("request", (r) => requests.push(r.url()));
      const page = await context.newPage();
      await page.goto(`${origin}/`);
      await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
      return { context, page };
    };
    const enrollWithCode = async (page: Page, code: string) => {
      await page.getByRole("button", { name: "Enroll with a code" }).click();
      await page.getByLabel("Enrollment code").fill(code);
      await page.getByRole("button", { name: "Continue" }).click();
    };

    // sam enrolls a password with the code, leaves, and signs in with it again.
    const sam = await open();
    await enrollWithCode(sam.page, samCode.code);
    await sam.page.getByLabel("New password").fill("a long enough test password");
    await sam.page.getByRole("button", { name: "Set the password and sign in" }).click();
    await expect(sam.page.getByText("Signed in as")).toBeVisible();
    await sam.page.getByRole("button", { name: "Leave" }).click();
    await expect(sam.page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await sam.page.getByLabel("Account name").fill("sam");
    await sam.page.getByLabel("Password", { exact: true }).fill("a long enough test password");
    await sam.page.getByRole("button", { name: "Sign in with the password" }).click();
    await expectProjects(sam.page, ["Sam empty project", "Sam project"]);
    await expect(sam.page.getByText("Sam task")).toBeVisible();

    // The owner enrolls a passkey on Chromium's virtual authenticator, leaves, and signs in with it.
    const owner = await open();
    const cdp = await owner.context.newCDPSession(owner.page);
    await cdp.send("WebAuthn.enable");
    await cdp.send("WebAuthn.addVirtualAuthenticator", { options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } });
    await enrollWithCode(owner.page, ownerCode.code);
    await owner.page.getByRole("button", { name: "Add a passkey and sign in" }).click();
    await expect(owner.page.getByText("Signed in as")).toBeVisible();
    await owner.page.getByRole("button", { name: "Leave" }).click();
    await owner.page.getByRole("button", { name: "Sign in with a passkey" }).click();
    await expectProjects(owner.page, ["Owner empty project", "Test project"]);
    await expect(owner.page.getByText("Owner task")).toBeVisible();
    await expect(owner.page.getByText("Sam task")).toHaveCount(0);

    // Each browser's own account page, with this browser's session marked.
    await sam.page.getByRole("button", { name: "Your account" }).click();
    await expect(sam.page.getByRole("dialog", { name: /Your account/ }).getByText("this browser")).toBeVisible();
    await sam.page.getByRole("dialog", { name: /Your account/ }).getByRole("button", { name: "Close" }).click();

    // The owner ends sam's session through the owner's channel: sam's next action reloads the page
    // onto the sign-in page.
    const sessions = (await call(f.socket, { method: "GET", path: "/v1/sessions" })) as { id: string; account: string }[];
    for (const s of sessions.filter((s) => s.account === "sam")) await call(f.socket, { method: "DELETE", path: `/v1/sessions/${s.id}`, idempotent: true, emptyOk: true });
    await sam.page.getByText("Sam task").click();
    await sam.page.getByRole("tab", { name: /Checks/ }).click();
    await expect(sam.page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(sam.page.getByText("Sam task")).toHaveCount(0);

    await owner.page.getByText("Owner task").click();
    await expect(owner.page.getByRole("tablist", { name: "Work area" })).toBeVisible();
    expect(requests.filter((url) => url.startsWith(origin))).not.toEqual([]);
    expect(requests.filter((url) => new URL(url).pathname.startsWith("/v1/experience"))).toEqual([]);
  } finally {
    await browser?.close();
  }
});
