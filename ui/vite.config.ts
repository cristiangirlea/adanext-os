import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// base "./" keeps the built page loadable from a file: URL, which is how the desktop shell loads it.
export default defineConfig({
  plugins: [react()],
  base: "./",
  // assetsInlineLimit 0: the fonts stay files, since font-src 'self' refuses data: URLs.
  build: { outDir: "dist", emptyOutDir: true, assetsInlineLimit: 0 },
  test: { environment: "jsdom", include: ["src/**/*.test.{ts,tsx}"] },
});
