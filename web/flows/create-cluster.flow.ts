import type { Page } from "@playwright/test";

import { cluster, expect, refusal, sent, test, written } from "./fixture.ts";

// What each step shows that no other step does.
const steps = [
  ["Network", "Bridge"],
  ["Machines", "Storage"],
  ["Add-ons", "Install Headlamp"],
  ["Policies", "Pod security"],
  ["Identity providers", "No identity providers"],
] as const;

// What the wizard sends for a copy of ci named fresh: the name, the version and the 3 workers the
// test sets or keeps, and the demo's own defaults for the rest.
const freshCluster = {
  name: "fresh",
  class: "kubehenge-k3s",
  version: "v1.36.6+k3s1",
  controlPlaneReplicas: 1,
  workerReplicas: 3,
  variables: {
    controlPlaneEndpoint: "10.20.70.61",
    network: {
      bridge: "vmbr0",
      ipFamilies: ["IPv4"],
      addresses: ["10.20.70.62-10.20.70.66"],
      prefix: 24,
      gateway: "10.20.70.1",
      dnsServers: ["10.20.0.53"],
    },
    storage: "local-lvm",
    controlPlaneMachine: { cores: 4, memoryMiB: 8192, diskGiB: 40 },
    workerMachine: { cores: 4, memoryMiB: 16_384, diskGiB: 80 },
    nodeOS: { family: "almalinux", release: "10" },
    firewall: { enabled: false },
    cni: "cilium",
    headlamp: { enabled: true },
    backups: {
      enabled: true,
      target: "offsite",
      schedule: "0 2 * * *",
      timeZone: "UTC",
      retentionDays: 7,
      volumes: "all",
    },
    etcdSnapshots: { enabled: true, target: "offsite", schedule: "0 */12 * * *", timeZone: "UTC", keep: 14 },
    oidc: [],
    upgrades: { mode: "patch", window: { start: "02:00", durationMinutes: 240, timeZone: "UTC" } },
  },
};

async function startFromCI(page: Page, name: string): Promise<void> {
  await page.goto("/clusters/new");
  await page.getByLabel("Start from").selectOption({ label: "ci" });
  await page.getByLabel("Name", { exact: true }).fill(name);
}

const stepButton = (page: Page, name: string): ReturnType<Page["getByRole"]> =>
  page.getByRole("navigation", { name: "Steps" }).getByRole("button", { name, exact: true });

test("is refused a name that is in use on the first step", async ({ page }) => {
  await page.goto("/clusters/new");
  await page.getByLabel("Name", { exact: true }).fill("web");
  await page.getByLabel("Distribution").selectOption({ label: "k3s" });
  await expect(page.getByText("web is in use. Enter another name.")).toBeVisible();

  await page.getByRole("button", { name: "Next" }).click();
  await expect(stepButton(page, "General")).toHaveAttribute("aria-current", "step");
  await expect(page.getByText("Bridge", { exact: true })).toBeHidden();

  await page.getByLabel("Name", { exact: true }).fill("fresh");
  await expect(page.getByText("is in use")).toBeHidden();
});

test("creates a cluster through each step to the confirmation", async ({ page, problems }) => {
  problems.allow("403 POST /api/clusters");
  await startFromCI(page, "fresh");
  await expect(stepButton(page, "General")).toHaveAttribute("aria-current", "step");

  for (const [step, shows] of steps) {
    await page.getByRole("button", { name: "Next" }).click();
    await expect(stepButton(page, step)).toHaveAttribute("aria-current", "step");
    await expect(page.getByText(shows).first()).toBeVisible();
    if (step === "Network") {
      // Next holds until the free addresses are in and the API address is picked.
      await expect(page.getByLabel("API address")).toHaveValue("10.20.70.61");
    }
  }

  const dryRun = sent(page, "POST", "/api/clusters?dryRun=All");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(stepButton(page, "Confirm")).toHaveAttribute("aria-current", "step");
  expect(await written(dryRun)).toEqual(freshCluster);

  const summary = page.locator(".wizard-body");
  await expect(summary.getByText("API server")).toBeVisible();
  await expect(summary.getByText("Accepts it")).toBeVisible();
  for (const [label, value] of [
    ["Name", "fresh"],
    ["Distribution", "k3s"],
    ["Kubernetes version", "v1.36.6+k3s1"],
    ["Workers", "3"],
    ["API address", "10.20.70.61"],
    ["Pod network", "Cilium"],
  ] as const) {
    await expect(summary.getByText(label, { exact: true }).first()).toBeVisible();
    await expect(summary.getByText(value, { exact: true }).first()).toBeVisible();
  }

  // The demo refuses the creation itself, so the page stays on the confirmation and nothing is listed.
  const create = sent(page, "POST", "/api/clusters");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  expect(await written(create)).toEqual(freshCluster);
  await expect(page.getByText(refusal)).toBeVisible();
  await expect(page.getByRole("button", { name: "Create", exact: true })).toBeEnabled();
  await expect(stepButton(page, "Confirm")).toHaveAttribute("aria-current", "step");
  expect(await cluster(page, "fresh")).toBeUndefined();
});
