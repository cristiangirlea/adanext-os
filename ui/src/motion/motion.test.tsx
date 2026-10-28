import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { effectAttrs, effectMode, effects, motionKey, motionOff, restoreMotion, setMotion, type EffectName } from "./effects";
import { Skeleton, Spinner } from "./parts";

// Read from disk: Vitest hands CSS modules to tests empty.
const stylesDir = join(dirname(fileURLToPath(import.meta.url)), "..", "styles");
const css: Record<string, string> = Object.fromEntries(
  readdirSync(stylesDir)
    .filter((name) => name.endsWith(".css"))
    .map((name) => [`../styles/${name}`, readFileSync(join(stylesDir, name), "utf8")]),
);

function reduceMotion(reduce: boolean): void {
  const listeners = new Set<() => void>();
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: reduce && query.includes("prefers-reduced-motion: reduce"),
    media: query,
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
  }));
}

beforeEach(() => {
  localStorage.clear();
  delete document.body.dataset.motion;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  delete document.body.dataset.motion;
});

const names = Object.keys(effects) as EffectName[];
const expected = (name: EffectName) => (effects[name].reduced === "unchanged" ? "run" : effects[name].reduced);

describe("effect registry", () => {
  it("declares a reduced-motion behaviour for every effect", () => {
    expect(names.length).toBeGreaterThan(0);
    for (const name of names) {
      expect(["static", "off", "unchanged"]).toContain(effects[name].reduced);
      expect(effects[name].does.length).toBeGreaterThan(0);
    }
  });

  it("runs every effect while motion is on", () => {
    reduceMotion(false);
    for (const name of names) expect(effectMode(name)).toBe("run");
  });

  it('stops every effect as it declares when body[data-motion="off"]', () => {
    reduceMotion(false);
    setMotion(false);
    expect(document.body.dataset.motion).toBe("off");
    for (const name of names) expect(effectMode(name), name).toBe(expected(name));
  });

  it("stops every effect as it declares under prefers-reduced-motion", () => {
    reduceMotion(true);
    expect(motionOff()).toBe(true);
    for (const name of names) expect(effectMode(name), name).toBe(expected(name));
  });

  it("marks each effect's element with what it does under reduced motion", () => {
    for (const name of names) expect(effectAttrs(name)).toEqual({ "data-effect": name, "data-reduced": effects[name].reduced });
  });

  it("saves the switch and restores it; nothing switches itself on", () => {
    restoreMotion();
    expect(document.body.dataset.motion).toBeUndefined();
    setMotion(false);
    expect(localStorage.getItem(motionKey)).toBe("off");
    delete document.body.dataset.motion;
    restoreMotion();
    expect(document.body.dataset.motion).toBe("off");
    setMotion(true);
    expect(document.body.dataset.motion).toBeUndefined();
    expect(localStorage.getItem(motionKey)).toBeNull();
  });
});

describe("loading parts", () => {
  it("shapes a skeleton like its content, hidden from assistive technology, and drops its shimmer when motion is off", () => {
    reduceMotion(false);
    render(<Skeleton rows={3} label="Loading tasks" />);
    const region = screen.getByRole("status", { name: "Loading tasks" });
    expect(region.getAttribute("aria-busy")).toBe("true");
    expect(region.querySelectorAll(".skeleton-row")).toHaveLength(3);
    expect(region.querySelector('[data-effect="shimmer"]')).not.toBeNull();
    act(() => setMotion(false));
    expect(region.querySelector('[data-effect="shimmer"]')).toBeNull();
  });

  it("renders the spinner still when motion is off", () => {
    reduceMotion(false);
    const { container } = render(<Spinner />);
    const spinner = container.querySelector('[data-effect="spinner"]')!;
    expect(spinner.getAttribute("aria-hidden")).toBe("true");
    expect(spinner.getAttribute("data-mode")).toBe("run");
    act(() => setMotion(false));
    expect(spinner.getAttribute("data-mode")).toBe(expected("spinner"));
  });
});

describe("motion rules in the stylesheets", () => {
  const all = Object.entries(css);
  const tokens = css["../styles/tokens.css"] ?? "";

  it("defines the named durations and the one easing", () => {
    for (const ms of [90, 120, 140, 180, 280, 350]) expect(tokens).toMatch(new RegExp(`--dur-${ms}:\\s*${ms}ms`));
    expect(tokens).toMatch(/--ease:\s*cubic-bezier\(\.22,\s*1,\s*\.36,\s*1\)/);
  });

  it("animates transform and opacity only, with named durations and the one easing", () => {
    for (const [name, text] of all) {
      for (const m of text.matchAll(/transition(-property)?\s*:\s*([^;}]+)/g)) {
        const value = m[2]!.trim();
        if (value === "none") continue;
        for (const part of value.split(",")) {
          const prop = part.trim().split(/\s+/)[0];
          expect(["transform", "opacity"], `${name}: ${part}`).toContain(prop);
          if (!m[1]) {
            expect(part, `${name}: ${part}`).toMatch(/var\(--dur-(90|120|140|180|280|350)\)/);
            expect(part, `${name}: ${part}`).toMatch(/var\(--ease\)/);
          }
        }
      }
      for (const m of text.matchAll(/animation\s*:\s*([^;}]+)/g)) {
        if (m[1]!.trim() === "none") continue;
        expect(m[1], `${name}: animation`).toMatch(/var\(--dur-(90|120|140|180|280|350)\)/);
        expect(m[1], `${name}: animation`).toMatch(/var\(--ease\)/);
      }
      for (const m of text.matchAll(/@keyframes\s+[\w-]+\s*\{((?:[^{}]*\{[^}]*\})*)\s*\}/g)) {
        for (const decl of m[1]!.matchAll(/([\w-]+)\s*:/g)) expect(["transform", "opacity"], `${name}: keyframes`).toContain(decl[1]);
      }
      if (name !== "../styles/tokens.css") expect(text, name).not.toMatch(/\b\d+(\.\d+)?m?s\b/);
    }
  });

  it("loops only the busy spinner and the loading shimmer", () => {
    for (const [name, text] of all) {
      for (const m of text.matchAll(/([^{}]+)\{[^}]*\binfinite\b[^}]*\}/g)) {
        expect(m[1]!.trim(), name).toMatch(/^\[data-effect="(spinner|shimmer)"\]$/);
      }
    }
  });

  it("has one kill switch beside prefers-reduced-motion for every reduced behaviour", () => {
    const motion = css["../styles/motion.css"] ?? "";
    expect(motion).toMatch(/@media \(prefers-reduced-motion: reduce\)/);
    expect(motion).toMatch(/body\[data-motion="off"\] \[data-reduced="static"\]/);
    expect(motion).toMatch(/body\[data-motion="off"\] \[data-reduced="off"\]/);
  });
});
