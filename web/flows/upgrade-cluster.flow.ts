import type { Page } from "@playwright/test";

import { cluster, expect, refusal, sent, test } from "./fixture.ts";

async function openUpgrade(page: Page): Promise<ReturnType<Page["getByRole"]>> {
  await page.goto("/clusters/clusters/web");
  expect(await cluster(page, "web")).toMatchObject({ version: "v1.37.1+rke2r1" });
  const plan = sent(page, "POST", "/api/clusters/clusters/web/plan");
  await page.getByRole("button", { name: "Upgrade" }).click();
  const upgrade = page.getByRole("dialog", { name: "Upgrade web" });
  expect((await plan).postDataJSON()).toEqual({ version: "v1.37.2+rke2r1" });
  await expect(upgrade.getByLabel("Kubernetes version")).toHaveValue("v1.37.2+rke2r1");
  return upgrade;
}

test("shows the upgrade's checks and applies the waiting add-on changes now", async ({ page, problems }) => {
  problems.allow("403 POST /api/clusters/clusters/web/addon-changes");
  const upgrade = await openUpgrade(page);

  await expect(upgrade.getByText("Replaces every node, one at a time")).toBeVisible();
  for (const [check, answer] of [
    ["Addresses", /spare/u],
    ["Nodes", "All online"],
    ["Removed APIs", /1 in use/u],
    ["Webhooks", /1 at risk/u],
    ["Health", "No problems"],
  ] as const) {
    await expect(upgrade.getByText(check, { exact: true })).toBeVisible();
    await expect(upgrade.getByText(answer).first()).toBeVisible();
  }
  await expect(upgrade.getByText("monitoring: kube-prometheus-stack 84.3.0 to 85.0.1")).toBeVisible();
  await expect(upgrade.getByText("headlamp: headlamp 0.40.1 to 0.41.0")).toBeVisible();
  await expect(upgrade.getByText("Applies at 13/10/2026, 18:00, when the upgrade window opens.")).toBeVisible();

  const apply = upgrade.getByRole("button", { name: "Apply now" });
  const post = sent(page, "POST", "/api/clusters/clusters/web/addon-changes");
  await apply.click();
  await post;
  await expect(upgrade.getByText(refusal)).toBeVisible();
  await expect(apply).toBeEnabled();
  await upgrade.getByRole("button", { name: "Cancel" }).click();
  await expect(upgrade).toBeHidden();
});

test("upgrades a cluster through Add to plan and Review plan", async ({ page, problems }) => {
  problems.allow("403 PATCH /api/clusters/clusters/web");
  const upgrade = await openUpgrade(page);
  await upgrade.getByRole("button", { name: "Add to plan" }).click();
  await expect(upgrade).toBeHidden();

  const staged = page.getByRole("region", { name: "Staged changes" });
  await expect(staged.getByText("1 change not applied")).toBeVisible();
  await staged.getByRole("button", { name: "Review plan" }).click();
  const review = page.getByRole("dialog", { name: "Review plan" });
  await expect(review.getByRole("row", { name: /Kubernetes version v1.37.1\+rke2r1 v1.37.2\+rke2r1/u })).toBeVisible();
  // Replacing nodes goes ahead once the admin accepts the risk.
  await review.getByLabel("Accept the risks").check();

  const patch = sent(page, "PATCH", "/api/clusters/clusters/web");
  await review.getByRole("button", { name: "Apply" }).click();
  expect((await patch).postDataJSON()).toMatchObject({ version: "v1.37.2+rke2r1" });
  await expect(review.getByText(refusal)).toBeVisible();
  await expect(review.getByRole("button", { name: "Apply" })).toBeEnabled();

  await review.getByRole("button", { name: "Keep staged" }).click();
  expect(await cluster(page, "web")).toMatchObject({ version: "v1.37.1+rke2r1" });
});
