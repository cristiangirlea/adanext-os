import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  // The live demonstration has its own config and opt-in (playwright.live.config.ts).
  testIgnore: ["**/live/**"],
  workers: 1,
  fullyParallel: false,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: "list",
  outputDir: "../.hrn/playwright-results",
});
