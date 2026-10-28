import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { currentTheme, setTheme, themeKey, themes } from "./theme";

const ui = (path: string) => join(dirname(fileURLToPath(import.meta.url)), "..", "..", path);
const indexHtml = readFileSync(ui("index.html"), "utf8");
const bootScript = readFileSync(ui("public/theme-boot.js"), "utf8");

// The policy as it stood before this slice; the boot script must fit it unchanged.
const csp =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'";

function boot(): void {
  // The file runs as a classic script; a Function body gives it the same global scope.
  new Function(bootScript)();
}

function reset(): void {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
}

describe("index.html", () => {
  const doc = new DOMParser().parseFromString(indexHtml, "text/html");
  const head = doc.head;

  it("keeps its content security policy unchanged", () => {
    const meta = head.querySelector('meta[http-equiv="Content-Security-Policy"]');
    expect(meta?.getAttribute("content")).toBe(csp);
    expect(indexHtml).not.toMatch(/unsafe-inline|sha256-|nonce-/);
  });

  it("loads the theme boot script as a blocking classic script in <head> before any stylesheet or module", () => {
    const boots = Array.from(doc.querySelectorAll("script")).filter((s) => s.getAttribute("src")?.endsWith("theme-boot.js"));
    expect(boots).toHaveLength(1);
    const script = boots[0]!;
    expect(script.parentElement).toBe(head);
    expect(script.hasAttribute("type")).toBe(false);
    expect(script.hasAttribute("defer")).toBe(false);
    expect(script.hasAttribute("async")).toBe(false);
    expect(script.textContent).toBe("");
    const before = (el: Element) => (script.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    const later = doc.querySelectorAll('link[rel="stylesheet"], style, script[type="module"], link[rel="modulepreload"]');
    expect(later.length).toBeGreaterThan(0);
    for (const el of Array.from(later)) expect(before(el), el.outerHTML).toBe(true);
  });

  it("holds no inline script", () => {
    for (const s of Array.from(doc.querySelectorAll("script"))) expect(s.getAttribute("src")).toBeTruthy();
  });
});

describe("theme boot script", () => {
  beforeEach(reset);
  afterEach(reset);

  it("sets the light default when nothing is saved", () => {
    boot();
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it.each(themes)("sets the saved theme %s", (theme) => {
    localStorage.setItem(themeKey, theme);
    boot();
    expect(document.documentElement.dataset.theme).toBe(theme);
  });

  it.each(["dark", "", "constructor", "__proto__", "light ", "<script>"])("falls back to light for the invalid value %j", (value) => {
    localStorage.setItem(themeKey, value);
    boot();
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("falls back to light when storage cannot be read", () => {
    const original = Object.getOwnPropertyDescriptor(window, "localStorage")!;
    Object.defineProperty(window, "localStorage", { configurable: true, get: () => { throw new Error("denied"); } });
    try {
      boot();
    } finally {
      Object.defineProperty(window, "localStorage", original);
    }
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("knows exactly the themes the app offers", () => {
    const listed = /themes\s*=\s*\[([^\]]*)\]/.exec(bootScript)?.[1] ?? "";
    expect(listed.split(",").map((s) => s.trim().replace(/^"|"$/g, ""))).toEqual([...themes]);
    expect(bootScript).toContain(JSON.stringify(themeKey));
  });
});

describe("in-app switch", () => {
  beforeEach(reset);
  afterEach(reset);

  it("applies and saves the choice, which the boot script then restores", () => {
    setTheme("workshop");
    expect(document.documentElement.dataset.theme).toBe("workshop");
    expect(localStorage.getItem(themeKey)).toBe("workshop");
    document.documentElement.removeAttribute("data-theme");
    boot();
    expect(currentTheme()).toBe("workshop");
  });

  it("reads an unknown attribute as light", () => {
    document.documentElement.dataset.theme = "neon";
    expect(currentTheme()).toBe("light");
  });

  it("never switches itself on: importing the module changes nothing", async () => {
    await import("./theme");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(localStorage.getItem(themeKey)).toBeNull();
  });
});
