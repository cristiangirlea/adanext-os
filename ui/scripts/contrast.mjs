#!/usr/bin/env node
// WCAG AA contrast check for every theme: reads ui/src/styles/theme-*.css and the declared
// foreground/background token pairs in contrast-pairs.json beside them. Plain Node, no
// dependency. Usage: node ui/scripts/contrast.mjs [styles directory]. Exit 1 on any failure.
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// text: body text (4.5:1); large: large text (3:1); ui: UI parts such as borders and focus (3:1).
export const required = { text: 4.5, large: 3, ui: 3 };

// parseTokens returns a theme file's colour custom properties, names without the leading "--",
// hex values normalised to six lower-case digits. Other properties are ignored.
export function parseTokens(css) {
  const tokens = {};
  for (const m of css.matchAll(/--([\w-]+)\s*:\s*(#[0-9a-fA-F]{3}|#[0-9a-fA-F]{6})\b/g)) {
    let hex = m[2].toLowerCase();
    if (hex.length === 4) hex = `#${hex[1]}${hex[1]}${hex[2]}${hex[2]}${hex[3]}${hex[3]}`;
    tokens[m[1]] = hex;
  }
  return tokens;
}

function luminance(hex) {
  const full = parseTokens(`--x: ${hex}`).x;
  if (!full) throw new Error(`not a hex colour: ${hex}`);
  const channel = (i) => {
    const c = parseInt(full.slice(i, i + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(1) + 0.7152 * channel(3) + 0.0722 * channel(5);
}

export function contrastRatio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// checkThemes checks every pair in every theme. themes: { name: { token: hex } }; pairs:
// [{ fg, bg: [token...], kind }]. Returns the number of pairs checked per theme and the failures.
export function checkThemes(themes, pairs) {
  const failures = [];
  const checked = {};
  for (const [theme, tokens] of Object.entries(themes)) {
    checked[theme] = 0;
    for (const pair of pairs) {
      const need = required[pair.kind];
      if (need === undefined) {
        failures.push(`${theme}: ${pair.fg} has unknown kind ${JSON.stringify(pair.kind)}`);
        continue;
      }
      for (const bg of pair.bg) {
        const fgHex = tokens[pair.fg];
        const bgHex = tokens[bg];
        if (!fgHex || !bgHex) {
          failures.push(`${theme}: ${pair.fg} on ${bg}: ${!fgHex ? pair.fg : bg} is not defined`);
          continue;
        }
        const ratio = contrastRatio(fgHex, bgHex);
        checked[theme]++;
        if (ratio < need) failures.push(`${theme}: ${pair.fg} on ${bg} is ${(Math.floor(ratio * 100) / 100).toFixed(2)}:1, needs ${need}:1 (${pair.kind})`);
      }
    }
  }
  return { checked, failures };
}

export function main(dir) {
  const themes = {};
  for (const name of readdirSync(dir).sort()) {
    const m = /^theme-([\w-]+)\.css$/.exec(name);
    if (m) themes[m[1]] = parseTokens(readFileSync(join(dir, name), "utf8"));
  }
  const errors = [];
  if (Object.keys(themes).length === 0) errors.push(`no theme-*.css files in ${dir}`);
  let pairs = [];
  try {
    pairs = JSON.parse(readFileSync(join(dir, "contrast-pairs.json"), "utf8")).pairs ?? [];
  } catch (e) {
    errors.push(`contrast-pairs.json could not be read: ${e.message}`);
  }
  if (pairs.length === 0) errors.push("no contrast pairs declared");
  const { checked, failures } = checkThemes(themes, pairs);
  errors.push(...failures);
  if (errors.length > 0) {
    for (const line of errors) process.stderr.write(`${line}\n`);
    return 1;
  }
  for (const [theme, n] of Object.entries(checked)) process.stdout.write(`${theme}: ${n} pairs pass AA\n`);
  return 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const here = dirname(fileURLToPath(import.meta.url));
  process.exitCode = main(resolve(process.argv[2] ?? join(here, "..", "src", "styles")));
}
