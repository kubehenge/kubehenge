// Takes the documentation's screenshots of the interface: starts `kubehenge serve --demo` on a
// loopback port, opens each view in a headless Chromium and writes it as a PNG.
//
//   node screenshots.ts <kubehenge binary> <output directory>
//
// The window, the colour scheme and both clocks are fixed, so a run gives the same images as the
// one before unless the interface or the demo changed.
import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { type Browser, chromium } from "playwright";

import { demoOrigin, demoTime, hostResolverRule, startDemo, viewport } from "./flows/demo.ts";

interface Shot {
  file: string;
  path: string;
  // A text the page shows once its data is in.
  ready: string;
  signOut?: boolean;
  clip?: { x: number; y: number; width: number; height: number };
}

const port = process.env["SCREENSHOT_PORT"] ?? "18443";
const origin = demoOrigin(port);
const settle = 1500;
const readyDeadline = 15_000;

const shots: Shot[] = [
  { file: "fleet.png", path: "/", ready: "payments", clip: { x: 0, y: 0, width: 1440, height: 660 } },
  { file: "create-cluster.png", path: "/clusters/new", ready: "Name" },
  { file: "cluster.png", path: "/clusters/clusters/web", ready: "web" },
  { file: "machines.png", path: "/clusters/clusters/web/machines", ready: "web" },
  { file: "health.png", path: "/clusters/clusters/payments/health", ready: "payments" },
  { file: "backups.png", path: "/clusters/clusters/web/backups", ready: "before-ingress-change" },
  { file: "options.png", path: "/clusters/clusters/web/options", ready: "web" },
  { file: "run.png", path: "/runs/change/clusters/edge", ready: "Change Kubernetes version" },
  {
    file: "sign-in.png",
    path: "/",
    ready: "payments",
    signOut: true,
    clip: { x: 400, y: 255, width: 640, height: 390 },
  },
];

const [binary, outArg] = process.argv.slice(2);
if (binary === undefined || outArg === undefined) {
  console.error("usage: node screenshots.ts <kubehenge binary> <output directory>");
  process.exit(2);
}
const out = resolve(outArg);
mkdirSync(out, { recursive: true });

const server = await startDemo(binary, port);

let browser: Browser | undefined;
try {
  browser = await chromium.launch({
    args: [
      hostResolverRule,
      // Rendering on the CPU in one pass, so shadows and rounded corners come out the same pixels each run.
      "--disable-gpu",
      "--disable-partial-raster",
      "--disable-skia-runtime-opts",
      "--force-color-profile=srgb",
      "--disable-lcd-text",
    ],
  });
  const context = await browser.newContext({
    viewport,
    deviceScaleFactor: 1,
    colorScheme: "dark",
    reducedMotion: "reduce",
    locale: "en-GB",
    timezoneId: "UTC",
  });
  const page = await context.newPage();
  await page.clock.setFixedTime(new Date(demoTime));
  for (const shot of shots) {
    await page.goto(origin + shot.path);
    try {
      await page.getByText(shot.ready, { exact: true }).first().waitFor({ timeout: readyDeadline });
    } catch (err) {
      const failed = join(tmpdir(), `kubehenge-${shot.file}`);
      await page.screenshot({ path: failed });
      throw new Error(`${shot.path} never showed "${shot.ready}". The page as it was: ${failed}`, { cause: err });
    }
    // The demo signs everyone in. Signing out shows the sign-in page, which the next page signs past.
    if (shot.signOut === true) {
      await page.getByRole("button", { name: "Sign out" }).click();
      await page.getByRole("button", { name: "Sign in" }).waitFor();
    }
    await page.locator(".skeleton, .loading").first().waitFor({ state: "detached" });
    // The polls that follow the first reads fill in the last figures, such as the VMs' load.
    await page.waitForTimeout(settle);
    await page.evaluate(async () => document.fonts.ready);
    await page.screenshot({
      path: join(out, shot.file),
      animations: "disabled",
      caret: "hide",
      ...(shot.clip === undefined ? {} : { clip: shot.clip }),
    });
    console.log(`wrote ${shot.file}`);
  }
} finally {
  await browser?.close();
  server.kill();
}
