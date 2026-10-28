import { spawnSync } from "node:child_process";
import { copyFileSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { checkThemes, contrastRatio, parseTokens } from "../../scripts/contrast.mjs";

const here = (path: string) => join(dirname(fileURLToPath(import.meta.url)), path);
const script = here("../../scripts/contrast.mjs");
const stylesDir = here(".");

function run(dir: string) {
  return spawnSync(process.execPath, [script, dir], { encoding: "utf8" });
}

const temps: string[] = [];
afterEach(() => {
  for (const dir of temps.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function copyStyles(): string {
  const dir = mkdtempSync(join(tmpdir(), "ada-contrast-"));
  temps.push(dir);
  for (const name of ["theme-light.css", "theme-workshop.css", "contrast-pairs.json"]) copyFileSync(join(stylesDir, name), join(dir, name));
  return dir;
}

describe("contrast ratio", () => {
  it("matches WCAG's reference values", () => {
    expect(contrastRatio("#000000", "#ffffff")).toBeCloseTo(21, 5);
    expect(contrastRatio("#ffffff", "#ffffff")).toBeCloseTo(1, 5);
    // #767676 on white is the classic 4.54:1 grey.
    expect(contrastRatio("#767676", "#ffffff")).toBeCloseTo(4.54, 2);
    expect(contrastRatio("#fff", "#000")).toBeCloseTo(21, 5);
  });

  it("reads custom properties from a theme file", () => {
    expect(parseTokens(":root { --a: #102030; --b:#FFF; --space-1: 4px }")).toEqual({ a: "#102030", b: "#ffffff" });
  });
});

describe("checkThemes", () => {
  const themes = { t: { fg: "#777777", bg: "#ffffff", ui: "#959595" } };

  it("fails a text pair below 4.5:1 and passes it as large text at 3:1", () => {
    expect(checkThemes(themes, [{ fg: "fg", bg: ["bg"], kind: "text" }]).failures).toHaveLength(1);
    expect(checkThemes(themes, [{ fg: "fg", bg: ["bg"], kind: "large" }]).failures).toHaveLength(0);
  });

  it("fails a UI part below 3:1", () => {
    const result = checkThemes(themes, [{ fg: "ui", bg: ["bg"], kind: "ui" }]);
    expect(result.failures).toHaveLength(1);
    expect(result.failures[0]).toMatch(/t: ui on bg is 2\.\d+:1, needs 3:1/);
  });

  it("fails a token a theme does not define, and an unknown kind", () => {
    expect(checkThemes(themes, [{ fg: "missing", bg: ["bg"], kind: "text" }]).failures[0]).toMatch(/missing/);
    expect(checkThemes(themes, [{ fg: "fg", bg: ["bg"], kind: "huge" }]).failures[0]).toMatch(/kind/);
  });
});

describe("the contrast script", () => {
  it("passes the shipped themes", () => {
    const result = run(stylesDir);
    expect(result.stderr).toBe("");
    expect(result.status).toBe(0);
    expect(result.stdout).toMatch(/light: \d+ pairs pass AA/);
    expect(result.stdout).toMatch(/workshop: \d+ pairs pass AA/);
  });

  it("fails when a pair falls below AA in one theme", () => {
    const dir = copyStyles();
    const file = join(dir, "theme-workshop.css");
    writeFileSync(file, readFileSync(file, "utf8").replace(/--text-3:\s*#[0-9a-f]+/i, "--text-3: #3a454c"));
    const result = run(dir);
    expect(result.status).toBe(1);
    expect(result.stderr).toMatch(/workshop: text-3 on /);
  });

  it("fails when there are no pairs or no themes", () => {
    const dir = copyStyles();
    writeFileSync(join(dir, "contrast-pairs.json"), JSON.stringify({ pairs: [] }));
    expect(run(dir).status).toBe(1);
    rmSync(join(dir, "theme-light.css"));
    rmSync(join(dir, "theme-workshop.css"));
    expect(run(dir).status).toBe(1);
  });

  it("declares every pair the specification names", () => {
    const { pairs } = JSON.parse(readFileSync(join(stylesDir, "contrast-pairs.json"), "utf8")) as { pairs: { fg: string; bg: string[]; kind: string }[] };
    const fgs = new Set(pairs.map((p) => p.fg));
    for (const fg of ["text-1", "text-2", "text-3", "accent-text", "accent-fg", "success", "warning", "danger", "info", "focus", "border-2", "border-3"]) {
      expect(fgs.has(fg), fg).toBe(true);
    }
    const backgrounds = ["bg", "sidebar", "surface", "elevated", "overlay"];
    const on = (fg: string, kind: string) => pairs.filter((p) => p.fg === fg && p.kind === kind).flatMap((p) => p.bg);
    for (const status of ["success", "warning", "danger", "info"]) {
      expect(on(status, "text"), status).toEqual(expect.arrayContaining([...backgrounds, `${status}-dim`]));
      expect(on("text-1", "text"), `text-1 on ${status}-dim`).toContain(`${status}-dim`);
    }
    expect(on("text-1", "text")).toContain("accent-dim");
    for (const fg of ["text-1", "text-2", "text-3", "accent-text", "focus", "border-2", "border-3"]) {
      expect([...on(fg, "text"), ...on(fg, "ui")], fg).toEqual(expect.arrayContaining(backgrounds));
    }
    expect(on("accent", "ui")).toEqual(expect.arrayContaining([...backgrounds, "accent-dim"]));
    expect(pairs.find((p) => p.fg === "focus")?.kind).toBe("ui");
  });

  it("is run by scripts/check.sh", () => {
    const check = readFileSync(here("../../../scripts/check.sh"), "utf8");
    expect(check).toMatch(/node ui\/scripts\/contrast\.mjs/);
  });
});
