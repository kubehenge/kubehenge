import type { Page } from "@playwright/test";

import { expect, refusal, sent, test, written } from "./fixture.ts";

// Each state the demo can show, as its page renders it. The demo's clusters are in pkg/ui/demo-fleet.json.

interface Attention {
  severity: string;
  what: string;
  message: string;
}

async function needsAttention(page: Page, { severity, what, message }: Attention): Promise<void> {
  const row = page.getByRole("row").filter({ hasText: what }).filter({ hasText: message });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText(severity);
}

test("the fleet sorts the clusters by what needs the owner", async ({ page }) => {
  await page.goto("/");
  // The cluster's entry in the list, ahead of the run that names it.
  const card = (cluster: string): ReturnType<Page["getByRole"]> =>
    page.getByRole("link", { name: new RegExp(`^clusters/${cluster}`, "u") }).first();
  await expect(page.getByRole("heading", { name: "Needs you 3" })).toBeVisible();
  await expect(card("payments")).toContainText(
    "Pod billing/invoice-worker-p5h6r: CrashLoopBackOff, 14 restarts and 3 more",
  );
  await expect(card("legacy")).toContainText("Kubernetes out of support and 3 more");
  await expect(card("staging")).toContainText("Worker staging-md-0-2trfn: taints differ from pool Workers and 3 more");
  await expect(page.getByRole("heading", { name: "Running 1" })).toBeVisible();
  await expect(card("edge")).toContainText("Upgrading to v1.37.1+k3s1");
  await expect(page.getByRole("heading", { name: "Healthy 4" })).toBeVisible();
});

test("payments shows a crash-looping pod, a slow etcd disk and a firing alert", async ({ page }) => {
  await page.goto("/clusters/clusters/payments/health");
  await expect(page.getByText("Degraded", { exact: true }).first()).toBeVisible();
  await needsAttention(page, {
    severity: "Error",
    what: "CrashLoopBackOff, 14 restarts",
    message: "back-off 5m0s restarting failed container",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "etcd disk slow",
    message: "etcd write-ahead log syncs take longer than 25 ms at the 99th percentile",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Alert KubePodCrashLooping in billing",
    message: "Pod billing/invoice-worker-p5h6r is crash looping.",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Proxmox VE node pve2 clock is off by 1m35s",
    message: "sign-ins fail when nodes differ by over 5 minutes",
  });
});

test("legacy shows an unsupported version, a pause and expiring certificates", async ({ page }) => {
  await page.goto("/clusters/clusters/legacy/health");
  await expect(page.getByText("Degraded", { exact: true }).first()).toBeVisible();
  await needsAttention(page, {
    severity: "Warning",
    what: "Kubernetes out of support",
    message: "Kubernetes 1.33 is older than 1.34",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Paused",
    message: "Nothing upgrades, renews certificates or replaces machines while the cluster is paused.",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Certificates expire",
    message: "machine legacy-cp-0 on 20/11/2026",
  });
});

test("staging shows a tainted worker, a skipped upgrade and a node that is not reachable", async ({
  page,
  problems,
}) => {
  problems.allow("403 POST /api/clusters/clusters/staging/upgrade-checks/accept");
  await page.goto("/clusters/clusters/staging/health");
  await needsAttention(page, {
    severity: "Warning",
    what: "taints differ from pool Workers",
    message: "Not in the pool's taints: maintenance:NoSchedule.",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Automatic upgrade skipped",
    message: "Removed APIs still in use: orders.example.com/v1beta1 invoices",
  });
  await needsAttention(page, {
    severity: "Warning",
    what: "Proxmox VE node not reachable",
    message: "pve3 (HTTP 596: certificate verify failed)",
  });
  await expect(page.getByText("pvecm updatecerts --force")).toBeVisible();

  // Accepting the removed APIs is a change, which the demo refuses where the page shows it.
  const accept = sent(page, "POST", "/api/clusters/clusters/staging/upgrade-checks/accept?version=v1.37.1%2Brke2r1");
  await page.getByRole("button", { name: "Upgrade anyway" }).click();
  expect(await written(accept)).toBeNull();
  await expect(page.getByText(/the demo changes nothing\. Install Kubehenge to manage clusters/iu)).toBeVisible();
  await expect(page.getByRole("button", { name: "Upgrade anyway" })).toBeEnabled();
});

test("edge shows an upgrade under way", async ({ page }) => {
  await page.goto("/clusters/clusters/edge/health");
  await needsAttention(page, {
    severity: "In progress",
    what: "Pool Workers rolling out: 1 of 2 up to date",
    message: "Rolling out 1 of 2 workers",
  });
  await expect(page.getByRole("row", { name: /TopologyReconciled False ClusterUpgrading/u })).toContainText(
    "Cluster is upgrading to v1.37.1+k3s1",
  );
});

test("web shows its control planes on one node and a node that is not reachable", async ({ page }) => {
  await page.goto("/clusters/clusters/web");
  await expect(
    page.getByText(
      "pve1 runs 3 of 3 control-plane machines, so losing it stops the control plane. Migrate one to another node.",
    ),
  ).toBeVisible();
  await expect(page.getByText("HTTP 596: certificate verify failed")).toBeVisible();
});

test("payments shows backups that are off and a backup volume it cannot read", async ({ page }) => {
  await page.goto("/clusters/clusters/payments/backups");
  await expect(page.getByText("Backups are off")).toBeVisible();
  await expect(
    page.getByText(
      "The backup volume could not be read: opening the backup volume: the cluster has no backup keys yet",
    ),
  ).toBeVisible();
  await expect(page.getByText("Target not readable")).toBeVisible();
});

test("a cluster that does not exist says so", async ({ page, problems }) => {
  problems.allow("404 GET /api/clusters/clusters/nope");
  await page.goto("/clusters/clusters/nope");
  await expect(page.getByText("Cluster not found.")).toBeVisible();
  await expect(page.getByRole("link", { name: "All clusters" })).toBeVisible();
});

test("a change the demo refuses shows its reason and leaves the page usable", async ({ page, problems }) => {
  problems.allow("403 POST /api/clusters/clusters/web/backups");
  await page.goto("/clusters/clusters/web/backups");
  const post = sent(page, "POST", "/api/clusters/clusters/web/backups");
  await page.getByRole("button", { name: "Back up now" }).click();
  expect(await written(post)).toBeNull();
  await expect(page.getByText(refusal)).toBeVisible();
  await expect(page.getByRole("button", { name: "Back up now" })).toBeEnabled();
  await expect(page.getByText("velero-scheduled-20261013030000")).toBeVisible();
});
