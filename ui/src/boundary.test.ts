import { describe, expect, it } from "vitest";

// Decisions log, 2026-10-14, #5: the React app talks to adad only through AdaClient, and only
// the adapters know how. Nothing else may reach the preload bridge or the network. Every source
// outside client/adapters/ is read as text and grepped; tests are left out.
const files = import.meta.glob<string>(["./**/*.{ts,tsx}", "!./client/adapters/**", "!./**/*.test.{ts,tsx}"], {
  query: "?raw",
  import: "default",
  eager: true,
});

const banned = [
  { name: "window" + ".ada", pattern: /\bwindow\s*\.\s*ada\b|\bglobalThis\s*\.\s*ada\b|\bwindow\s*\[\s*["']ada["']\s*\]/ },
  { name: "fetch", pattern: /\bfetch\s*\(|\bglobalThis\s*\.\s*fetch\b|\bwindow\s*\.\s*fetch\b/ },
  { name: "EventSource", pattern: /\bEventSource\b/ },
  { name: "XMLHttpRequest", pattern: /\bXMLHttpRequest\b/ },
];

describe("browser boundary", () => {
  const names = Object.keys(files);
  it("covers the app's sources", () => {
    expect(names).toContain("./App.tsx");
    expect(names).toContain("./main.tsx");
    expect(names.some((name) => name.startsWith("./client/adapters/"))).toBe(false);
  });
  for (const name of names) {
    it(`${name} never touches the bridge or the network directly`, () => {
      const text = files[name] ?? "";
      for (const rule of banned) {
        expect(rule.pattern.test(text), `${name} uses ${rule.name}`).toBe(false);
      }
    });
  }
});
