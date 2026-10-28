import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import config from "../playwright.config";
import liveConfig from "../playwright.live.config";
import { liveEnabled, personalText, preflightRecord, screenText, underHome } from "../e2e/live/guard";

const root = path.resolve(__dirname, "..", "..");

// A stand-in for document.body: its text, its form controls and its elements with attributes.
function body(text: string, controls: { tag: string; value: string; selected?: string }[], attributes: Record<string, string>[]) {
  return {
    innerText: text,
    querySelectorAll(selector: string) {
      if (selector === "input, textarea, select") {
        return controls.map((c) => ({ tagName: c.tag.toUpperCase(), value: c.value, selectedOptions: c.selected === undefined ? undefined : [{ text: c.selected }], getAttribute: () => null }));
      }
      return attributes.map((a) => ({ tagName: "DIV", getAttribute: (name: string) => a[name] ?? null }));
    },
  };
}

describe("the live demonstration", () => {
  it("is left out of the default Playwright run, which check.sh and CI use", () => {
    expect(config.testIgnore).toEqual(["**/live/**"]);
    for (const script of ["scripts/check.sh", "scripts/test-e2e.sh"]) {
      expect(readFileSync(path.join(root, script), "utf8"), script).not.toContain("ADANEXT_LIVE");
    }
  });

  it("records no screenshots, traces or videos of its own beyond the guarded ones", () => {
    expect(liveConfig.use).toMatchObject({ screenshot: "off", trace: "off", video: "off" });
  });

  it("runs only with ADANEXT_LIVE set to exactly 1", () => {
    expect(liveEnabled({ ADANEXT_LIVE: "1" })).toBe(true);
    for (const value of [undefined, "", "0", "true", "yes", " 1", "1 "]) expect(liveEnabled({ ADANEXT_LIVE: value })).toBe(false);
  });

  it("finds e-mail-shaped text, the home directory and the user name before a screenshot", () => {
    expect(personalText("claude, default model, auto  Task awaiting_review")).toEqual([]);
    expect(personalText("signed in as someone@example.com and other.person@mail.example.org")).toEqual(["2 e-mail-shaped text"]);
    expect(personalText("C:/Users/Someone/repo", "someone", "C:\\Users\\Someone")).toEqual(["the home directory", "the user name"]);
    expect(personalText("E:/ada-live/repo", "someone", "C:\\Users\\Someone")).toEqual([]);
    // Either slash form and any case, on the page or in the home directory given.
    expect(personalText("C:\\USERS\\OWNER1\\repo", "", "c:/users/owner1")).toEqual(["the home directory"]);
    expect(personalText("c:/users/owner1/repo", "", "C:\\Users\\Owner1")).toEqual(["the home directory"]);
    // A short user name would match ordinary words, so it is not looked for.
    expect(personalText("a demo", "de")).toEqual([]);
    // The finding never repeats the text it found.
    expect(personalText("someone@example.com").join(" ")).not.toContain("someone");
  });

  it("scans form values and title, placeholder and aria-label attributes as well as the text", () => {
    const scanned = screenText(
      body("Import a repository", [{ tag: "input", value: "someone@example.com" }, { tag: "select", value: "codex", selected: "codex 0.163" }], [{ title: "C:\\Users\\Someone\\repo" }, { placeholder: "10m", "aria-label": "Name of someone" }]),
    );
    for (const part of ["Import a repository", "someone@example.com", "codex 0.163", "C:\\Users\\Someone\\repo", "10m", "Name of someone"]) expect(scanned).toContain(part);
    expect(personalText(scanned, "someone", "C:\\Users\\Someone")).toEqual(["1 e-mail-shaped text", "the home directory", "the user name"]);
  });

  it("refuses a scratch directory under the home folder, in either slash form and any case", () => {
    const homes = ["C:\\Users\\Someone", "/c/Users/Someone"];
    expect(underHome("C:\\Users\\Someone\\AppData\\Local\\Temp", homes)).toBe(true);
    expect(underHome("c:/users/someone", homes)).toBe(true);
    expect(underHome("/C/USERS/SOMEONE/x", homes)).toBe(true);
    expect(underHome("E:/ada-live", homes)).toBe(false);
    expect(underHome("C:/Users/Someone2/x", homes)).toBe(false);
    expect(underHome("/tmp/ada-live", ["/home/someone", ""])).toBe(false);
    expect(underHome("/home/someone/ada-live", ["/home/someone/"])).toBe(true);
  });

  it("keeps why a preflight was refused, as ada-live masked it", () => {
    const kept = preflightRecord({ outcome: "refused", refused_at: "preflight", reason: "claude auth status: exit status 1", preflight: {} });
    expect(kept.refused_at).toBe("preflight");
    expect(kept.reason).toBe("claude auth status: exit status 1");
  });

  it("keeps only the outcome, version, masked status, settings line and a count of the user configuration", () => {
    const kept = preflightRecord({
      backend: "codex",
      outcome: "passed",
      preflight: { version: "0.163.0", status: "Logged in using ChatGPT\n", status_command: "codex login status", settings: "no apiKeyHelper, no key in env, managed settings included", user_configuration: ["plugin private-tool", "mcp server work-mail"] },
    });
    expect(kept).toEqual({ outcome: "passed", refused_at: undefined, reason: undefined, version: "0.163.0", status: "Logged in using ChatGPT\n", settings: "no apiKeyHelper, no key in env, managed settings included", user_configuration_entries: 2 });
    expect(preflightRecord({ outcome: "refused", preflight: { user_configuration: null } }).user_configuration_entries).toBe(0);
  });
});

// Git Bash on Windows (plain `bash` there may be WSL), bash elsewhere.
const bash = process.platform === "win32" ? path.join(process.env.ProgramFiles ?? "C:/Program Files", "Git", "bin", "bash.exe") : "bash";

describe.skipIf(!existsSync(bash) && process.platform === "win32")("live-demo.sh's scratch check", () => {
  const home = os.homedir();
  const run = (scratch: string) =>
    spawnSync(bash, [path.join(root, "scripts", "live-demo.sh"), "--check-inputs"], {
      env: { ...process.env, ADANEXT_LIVE: "1", ADANEXT_ENGINE: path.join(root, "AGENTS.md"), ADANEXT_LIVE_SCRATCH: scratch },
      encoding: "utf8",
    });

  it("accepts an absolute directory outside the home folder", () => {
    const outside = process.platform === "win32" ? "Z:/ada-live" : "/tmp/ada-live";
    const r = run(outside);
    expect(r.stdout + r.stderr).toContain("inputs accepted");
    expect(r.status).toBe(0);
  });

  it("refuses a relative directory and one in the home folder, in either slash form or case", () => {
    const refused = ["ada-live", path.join(home, "ada-live"), path.join(home, "ada-live").split(path.sep).join("/").toUpperCase()];
    for (const scratch of refused) expect(run(scratch).status, scratch).toBe(2);
  });
});
