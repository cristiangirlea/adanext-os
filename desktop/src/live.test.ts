import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import config from "../playwright.config";
import { liveEnabled, personalText } from "../e2e/live/guard";

const root = path.resolve(__dirname, "..", "..");

describe("the live demonstration", () => {
  it("is left out of the default Playwright run, which check.sh and CI use", () => {
    expect(config.testIgnore).toEqual(["**/live/**"]);
    for (const script of ["scripts/check.sh", "scripts/test-e2e.sh"]) {
      expect(readFileSync(path.join(root, script), "utf8"), script).not.toContain("ADANEXT_LIVE");
    }
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
    // A short user name would match ordinary words, so it is not looked for.
    expect(personalText("a demo", "de")).toEqual([]);
    // The finding never repeats the text it found.
    expect(personalText("someone@example.com").join(" ")).not.toContain("someone");
  });
});
