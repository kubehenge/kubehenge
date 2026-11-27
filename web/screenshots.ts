// Takes the documentation's screenshots of the interface: starts `kubehenge serve --demo` on a
// loopback port, opens each view in a headless Chromium and writes it as a PNG.
//
//   node screenshots.ts <kubehenge binary> <output directory> [states]
//
// With "states" it writes instead the pages whose reads fail or have not answered, to check that
// they state nothing they have not read.
//
// The window, the colour scheme and both clocks are fixed, so a run gives the same images as the
// one before unless the interface or the demo changed.
import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { type Browser, chromium } from "playwright";

import { demoTime, hostResolverRule, startDemo, viewport } from "./flows/demo.ts";

interface Shot {
  file: string;
  path: string;
  // A text the page shows once its data is in.
  ready: string;
  signOut?: boolean;
  clip?: { x: number; y: number; width: number; height: number };
}

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

// A page with some of its reads made to fail (answered 502) or never answered.
interface State {
  file: string;
  path: string;
  fail?: string[];
  hang?: string[];
  // The failing reads answer once, and fail only when read again.
  failLater?: boolean;
  // A text the page shows before anything is clicked or read again.
  before?: string;
  // What to click once the page shows the text before.
  click?: { role: "button" | "link"; name: string };
  // A text the page shows in that state.
  ready: string;
}

const states: State[] = [
  { file: "fleet-health-failed.png", path: "/", fail: ["/api/fleet/health"], ready: "Unknown" },
  { file: "fleet-health-pending.png", path: "/", hang: ["/api/fleet/health"], ready: "Loading…" },
  { file: "run-failed.png", path: "/runs/change/clusters/edge", fail: ["/api/runs"], ready: "Try now" },
  { file: "run-pending.png", path: "/runs/change/clusters/edge", hang: ["/api/runs"], ready: "Run" },
  {
    file: "cluster-placement-pending.png",
    path: "/clusters/clusters/web",
    hang: ["/api/clusters/clusters/web"],
    ready: "Placement",
  },
  { file: "runs-failed.png", path: "/", fail: ["/api/runs"], ready: "Try now" },
  { file: "runs-pending.png", path: "/clusters/clusters/web", hang: ["/api/runs"], ready: "Placement" },
  {
    file: "fleet-health-stale.png",
    path: "/",
    fail: ["/api/fleet/health"],
    failLater: true,
    before: "Degraded",
    ready: "Try now",
  },
  { file: "remove-clusters-failed.png", path: "/administration", fail: ["/api/clusters"], ready: "Clusters not read." },
  { file: "remove-clusters-pending.png", path: "/administration", hang: ["/api/clusters"], ready: "Danger zone" },
  {
    file: "cluster-read-failed.png",
    path: "/",
    fail: ["/api/clusters/clusters/web"],
    before: "payments",
    click: { role: "link", name: "web" },
    ready: "Placement",
  },
  {
    file: "placement-nodes-failed.png",
    path: "/clusters/clusters/web",
    fail: ["/api/proxmox/nodes"],
    ready: "Try now",
  },
  {
    file: "placement-nodes-pending.png",
    path: "/clusters/clusters/web",
    hang: ["/api/proxmox/nodes"],
    ready: "Placement",
  },
  {
    file: "upgrade-versions-pending.png",
    path: "/clusters/clusters/web",
    hang: ["/api/versions"],
    before: "Placement",
    click: { role: "button", name: "Upgrade" },
    ready: "Upgrade web",
  },
  {
    file: "upgrade-versions-failed.png",
    path: "/clusters/clusters/web",
    fail: ["/api/versions"],
    before: "Placement",
    click: { role: "button", name: "Upgrade" },
    ready: "Try now",
  },
  {
    file: "backups-etcd-target.png",
    path: "/clusters/clusters/web/backups",
    hang: ["/api/clusters/clusters/web/backups"],
    ready: "Last taken",
  },
  {
    file: "backups-etcd-failed.png",
    path: "/clusters/clusters/web/backups",
    fail: ["/etcd-snapshots"],
    ready: "Try now",
  },
  {
    file: "backups-velero-failed.png",
    path: "/clusters/clusters/web/backups",
    fail: ["/api/clusters/clusters/web/backups"],
    ready: "Try now",
  },
  {
    file: "health-failed.png",
    path: "/clusters/clusters/web/health",
    fail: ["/api/clusters/clusters/web/health"],
    ready: "Try now",
  },
  {
    file: "admin-backups-failed.png",
    path: "/administration/backups",
    fail: ["/api/appliance/etcd-backups"],
    ready: "Try now",
  },
  { file: "admin-health-failed.png", path: "/administration", fail: ["/api/setup/health"], ready: "Try now" },
  {
    file: "notifications-targets-failed.png",
    path: "/administration/notifications",
    fail: ["/api/credentials/notify"],
    ready: "Targets not read.",
  },
];

const [binary, outArg, mode] = process.argv.slice(2);
if (binary === undefined || outArg === undefined) {
  console.error("usage: node screenshots.ts <kubehenge binary> <output directory> [states]");
  process.exit(2);
}
const out = resolve(outArg);
mkdirSync(out, { recursive: true });

const { server, origin } = await startDemo(binary);

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
  for (const state of mode === "states" ? states : []) {
    let answered = false;
    await page.route(
      (url) => [...(state.fail ?? []), ...(state.hang ?? [])].some((p) => url.pathname.includes(p)),
      async (route) => {
        if (state.fail?.some((p) => route.request().url().includes(p)) !== true) {
          return;
        }
        if (state.failLater === true && !answered) {
          answered = true;
          await route.continue();
          return;
        }
        await route.abort();
      },
    );
    await page.goto(origin + state.path);
    if (state.before !== undefined) {
      await page.getByText(state.before, { exact: true }).first().waitFor({ timeout: readyDeadline });
    }
    if (state.click !== undefined) {
      await page.getByRole(state.click.role, { name: state.click.name, exact: true }).first().click();
    }
    // A tab shown again reads everything again.
    if (state.failLater === true) {
      await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    }
    await page.getByText(state.ready, { exact: true }).first().waitFor({ timeout: readyDeadline });
    await page.waitForTimeout(settle);
    await page.screenshot({ path: join(out, state.file), animations: "disabled", caret: "hide" });
    console.log(`wrote ${state.file}`);
    await page.unrouteAll();
  }
  for (const shot of mode === "states" ? [] : shots) {
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
