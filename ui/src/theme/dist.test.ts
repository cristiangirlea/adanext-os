import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { checkBuiltPage } from "../../scripts/check-dist.mjs";

// The built page is what the shells load: scripts/check.sh runs check-dist.mjs on ui/dist right
// after the build. These tests hold the rules it applies.
const good = `<!doctype html><html><head>
<meta http-equiv="Content-Security-Policy" content="script-src 'self'" />
<script src="./theme-boot.js"></script>
<script type="module" crossorigin src="./assets/index-a.js"></script>
<link rel="stylesheet" crossorigin href="./assets/index-b.css">
</head><body><div id="root"></div></body></html>`;

describe("the built page check", () => {
  it("passes a page with the boot script first, by a relative path", () => {
    expect(checkBuiltPage(good, ["theme-boot.js"])).toEqual([]);
  });

  it("fails when the boot script comes after the stylesheet or the module", () => {
    const late = good.replace('<script src="./theme-boot.js"></script>\n', "").replace("</head>", '<script src="./theme-boot.js"></script></head>');
    const problems = checkBuiltPage(late, ["theme-boot.js"]);
    expect(problems.join("\n")).toMatch(/before the stylesheet/);
    expect(problems.join("\n")).toMatch(/before the module script/);
  });

  it("fails an absolute path, a missing script, a deferred one, and a boot file not in the build", () => {
    expect(checkBuiltPage(good.replace("./theme-boot.js", "/theme-boot.js"), ["theme-boot.js"]).join()).toMatch(/relative/);
    expect(checkBuiltPage(good.replace('<script src="./theme-boot.js"></script>', ""), ["theme-boot.js"]).join()).toMatch(/no theme-boot\.js/);
    expect(checkBuiltPage(good.replace('<script src="./theme-boot.js">', '<script defer src="./theme-boot.js">'), ["theme-boot.js"]).join()).toMatch(/blocking/);
    expect(checkBuiltPage(good, []).join()).toMatch(/not in the build/);
  });

  it("is run by scripts/check.sh right after the ui build", () => {
    const check = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "..", "..", "scripts", "check.sh"), "utf8");
    expect(check).toMatch(/npm run build && node scripts\/check-dist\.mjs/);
  });
});
