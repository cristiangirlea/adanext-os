import { builtinModules } from "node:module";
import { defineConfig } from "vitest/config";

// Main and preload are bundled to CommonJS: a sandboxed preload must be CommonJS, and bundling
// lets both import the shared types from ../ui/src/client without emitting across packages.
// Electron and Node's own modules stay external.
export default defineConfig({
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "node22",
    minify: false,
    sourcemap: true,
    lib: {
      entry: { main: "src/main.ts", preload: "src/preload.ts" },
      formats: ["cjs"],
      fileName: (_format, name) => `${name}.cjs`,
    },
    rollupOptions: {
      external: ["electron", ...builtinModules, ...builtinModules.map((name) => `node:${name}`)],
    },
  },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
