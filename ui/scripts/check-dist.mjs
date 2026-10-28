#!/usr/bin/env node
// Checks the built page (ui/dist/index.html) after `npm run build`: the theme boot script is
// there, blocking, by a relative path (the desktop shell loads the page from a file: URL), and
// before the stylesheet and the module script, and its file is in the build; and the bundled
// fonts load from the build's own files. Plain Node, no dependency. Usage: node scripts/check-dist.mjs [dist directory]. Exit 1 on any problem.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const boot = "theme-boot.js";

// checkBuiltPage returns the problems found in a built index.html; files lists the build's
// top-level file names.
export function checkBuiltPage(html, files) {
  const problems = [];
  const head = /<head>([\s\S]*?)<\/head>/i.exec(html)?.[1] ?? "";
  const tags = [...head.matchAll(/<(script|link)\b([^>]*)>/gi)].map((m) => ({ tag: m[1].toLowerCase(), attrs: m[2], at: m.index }));
  const src = (attrs) => /\bsrc="([^"]*)"/i.exec(attrs)?.[1] ?? "";
  const script = tags.find((t) => t.tag === "script" && src(t.attrs).endsWith(boot));
  if (!script) return [`no ${boot} script in <head>`];
  if (!src(script.attrs).startsWith("./")) problems.push(`${boot} must load by a relative path ("./${boot}"), got "${src(script.attrs)}"`);
  if (/\b(type|defer|async)\b/i.test(script.attrs)) problems.push(`${boot} must be a blocking classic script, with no type, defer or async`);
  const stylesheet = tags.find((t) => t.tag === "link" && /\brel="stylesheet"/i.test(t.attrs));
  const module = tags.find((t) => t.tag === "script" && /\btype="module"/i.test(t.attrs));
  if (!stylesheet || stylesheet.at < script.at) problems.push(`${boot} must come before the stylesheet`);
  if (!module || module.at < script.at) problems.push(`${boot} must come before the module script`);
  if (!files.includes(boot)) problems.push(`${boot} is not in the build`);
  return problems;
}

// checkBuiltFonts returns the problems found in the built stylesheets: both bundled families
// are declared, and every font url is a file of the build by a relative path, never a remote
// host and never inlined (font-src is 'self'). files lists the asset file names.
export function checkBuiltFonts(stylesheets, files) {
  const problems = [];
  const css = stylesheets.join("\n");
  const faces = css.match(/@font-face\s*\{[^}]*\}/g) ?? [];
  for (const family of ["Source Sans 3", "JetBrains Mono"]) {
    if (!faces.some((face) => face.includes(family))) problems.push(`no @font-face for ${family}`);
  }
  for (const face of faces) {
    for (const m of face.matchAll(/url\(\s*["']?([^"')]+)["']?\s*\)/g)) {
      const url = m[1];
      if (/^(?:[a-z][a-z0-9+.-]*:)?\/\//i.test(url) || /^https?:/i.test(url)) problems.push(`a remote font: ${url}`);
      else if (/^data:/i.test(url)) problems.push(`an inline font (font-src is 'self'): ${url.slice(0, 40)}`);
      else if (!files.includes(url.replace(/^\.\//, "").split(/[?#]/)[0].split("/").pop())) problems.push(`a font not in the build: ${url}`);
    }
  }
  return problems;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const dist = resolve(process.argv[2] ?? join(dirname(fileURLToPath(import.meta.url)), "..", "dist"));
  const page = join(dist, "index.html");
  const assets = join(dist, "assets");
  const assetFiles = existsSync(assets) ? readdirSync(assets) : [];
  const stylesheets = assetFiles.filter((f) => f.endsWith(".css")).map((f) => readFileSync(join(assets, f), "utf8"));
  const problems = existsSync(page) ? [...checkBuiltPage(readFileSync(page, "utf8"), readdirSync(dist)), ...checkBuiltFonts(stylesheets, assetFiles)] : [`no ${page}; run npm run build first`];
  for (const line of problems) process.stderr.write(`${line}\n`);
  if (problems.length === 0) process.stdout.write(`${page}: the theme boot script loads first, by a relative path, and the bundled fonts load from the build\n`);
  process.exitCode = problems.length === 0 ? 0 : 1;
}
