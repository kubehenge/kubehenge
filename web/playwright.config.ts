// The interface's flow tests (flows/*.flow.ts) against `kubehenge serve --demo`. `make ui-flows`
// builds the binary and runs them. Run one by hand from web/ with
//
//   KUBEHENGE_BIN=<binary> npx playwright test flows/sign-in.flow.ts
import { defineConfig } from "@playwright/test";

import { hostResolverRule, viewport } from "./flows/demo.ts";

export default defineConfig({
  testDir: "flows",
  testMatch: "*.flow.ts",
  // One server and one browser, the tests one after the other.
  workers: 1,
  fullyParallel: false,
  // A test that passes only on a second try is a flake to fix.
  retries: 0,
  forbidOnly: true,
  reporter: [["list"], ["html", { open: "never", outputFolder: "../dist/ui-flows/report" }]],
  outputDir: "../dist/ui-flows/results",
  use: {
    viewport,
    colorScheme: "dark",
    reducedMotion: "reduce",
    locale: "en-GB",
    timezoneId: "UTC",
    launchOptions: { args: [hostResolverRule] },
    trace: "retain-on-failure",
  },
  expect: { timeout: 10_000 },
});
