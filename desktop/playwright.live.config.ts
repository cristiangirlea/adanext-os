import { defineConfig } from "@playwright/test";

// M4.5's live demonstration on the owner's plan: run by scripts/live-demo.sh with ADANEXT_LIVE=1,
// never by check.sh or CI. The spec skips itself without the opt-in.
export default defineConfig({
  testDir: "./e2e/live",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 20 * 60_000,
  expect: { timeout: 15_000 },
  reporter: "list",
  outputDir: "../.hrn/playwright-live-results",
});
