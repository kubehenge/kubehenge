// Takes the documentation's screenshots of the interface: starts `kubehenge serve --demo` on a
// loopback port, opens each view in a headless Chromium and writes it as a PNG.
//
//   node screenshots.ts <kubehenge binary> <output directory>
//
// The window, the colour scheme and both clocks are fixed, so a run gives the same images as the
// one before unless the interface or the demo changed.
import { spawn } from "node:child_process";
import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

import { type Browser, chromium } from "playwright";

interface Shot {
  file: string;
  path: string;
  // A text the page shows once its data is in.
  ready: string;
  signOut?: boolean;
  clip?: { x: number; y: number; width: number; height: number };
}

// The demo's clock, which the browser's is set to as well, so that ages read the same in both.
const demoTime = "2026-10-13T09:00:00Z";
const port = process.env["SCREENSHOT_PORT"] ?? "18443";
const origin = `http://localhost:${port}`;
const viewport = { width: 1440, height: 900 };
const startDeadline = 30_000;
const pollInterval = 200;
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

const server = spawn(binary, ["serve", "--demo", "--demo-time", demoTime, "--listen", `127.0.0.1:${port}`], {
  stdio: ["ignore", "ignore", "inherit"],
});

async function waitForServer(): Promise<void> {
  const until = Date.now() + startDeadline;
  for (;;) {
    if (server.exitCode !== null) {
      throw new Error(`kubehenge serve --demo exited with ${server.exitCode}`);
    }
    try {
      const res = await fetch(`http://127.0.0.1:${port}/healthz`);
      if (res.ok) {
        return;
      }
    } catch {
      // Not listening yet.
    }
    if (Date.now() > until) {
      throw new Error(`kubehenge serve --demo did not answer on port ${port}`);
    }
    await sleep(pollInterval);
  }
}

let browser: Browser | undefined;
try {
  await waitForServer();
  browser = await chromium.launch({
    args: [
      "--host-resolver-rules=MAP localhost 127.0.0.1",
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
