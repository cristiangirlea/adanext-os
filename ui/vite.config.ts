import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// base "./" keeps the built page loadable from a file: URL, which is how the desktop shell loads it.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: { outDir: "dist", emptyOutDir: true },
  test: { environment: "jsdom", include: ["src/**/*.test.{ts,tsx}"] },
});
