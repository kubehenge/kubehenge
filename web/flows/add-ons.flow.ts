import type { Locator, Page } from "@playwright/test";

import { cluster, expect, isRecord, refusal, sent, test } from "./fixture.ts";

const staged = (page: Page): Locator => page.getByRole("region", { name: "Staged changes" });

// Applies what the page has staged, which the demo refuses, and returns the request's body.
async function applyStaged(page: Page, problems: { allow: (entry: string) => void }): Promise<unknown> {
  problems.allow("403 PATCH /api/clusters/clusters/web");
  await staged(page).getByRole("button", { name: "Review plan" }).click();
  const review = page.getByRole("dialog", { name: "Review plan" });
  const patch = sent(page, "PATCH", "/api/clusters/clusters/web");
  await review.getByRole("button", { name: "Apply" }).click();
  const body: unknown = (await patch).postDataJSON();
  await expect(review.getByText(refusal)).toBeVisible();
  await expect(review.getByRole("button", { name: "Apply" })).toBeEnabled();
  await review.getByRole("button", { name: "Keep staged" }).click();
  return body;
}

async function variables(page: Page): Promise<Record<string, unknown>> {
  const found = (await cluster(page, "web"))?.["variables"];
  if (!isRecord(found)) {
    throw new Error("the demo's web cluster has no variables");
  }
  return found;
}

test("turns Istio on and discards the change", async ({ page, problems }) => {
  await page.goto("/clusters/clusters/web/options");
  const istio = page.getByRole("switch", { name: "Istio" });
  await expect(istio).not.toBeChecked();

  const plan = sent(page, "POST", "/api/clusters/clusters/web/plan");
  await istio.click();
  expect((await plan).postDataJSON()).toEqual({ variables: { istio: { enabled: true } } });
  await expect(staged(page).getByText("1 change not applied")).toBeVisible();
  await expect(staged(page).getByText("Istio: Off to On, ambient mode")).toBeVisible();

  expect(await applyStaged(page, problems)).toEqual({ variables: { istio: { enabled: true } } });
  expect(await variables(page)).not.toHaveProperty("istio");

  await staged(page).getByRole("button", { name: "Discard" }).click();
  await expect(staged(page)).toBeHidden();
  await expect(istio).not.toBeChecked();
});

test("turns certificates on through Add to plan", async ({ page, problems }) => {
  await page.goto("/clusters/clusters/web/options");
  await page.getByRole("button", { name: "Edit certificates" }).click();
  const dialog = page.getByRole("dialog", { name: "Edit certificates" });
  await dialog.getByLabel("Issue certificates").check();
  await dialog.getByLabel("Domain").fill("apps.example.com");
  await dialog.getByRole("button", { name: "Add to plan" }).click();
  await expect(dialog).toBeHidden();
  await expect(staged(page).getByText("1 change not applied")).toBeVisible();

  expect(await applyStaged(page, problems)).toEqual({
    variables: {
      certificates: {
        enabled: true,
        issuer: "acme-dns01",
        domain: "apps.example.com",
        dns01: { provider: "cloudflare" },
      },
    },
  });
  expect(await variables(page)).not.toHaveProperty("certificates");
});

test("turns Headlamp off through Add to plan", async ({ page, problems }) => {
  await page.goto("/clusters/clusters/web/options");
  await page.getByRole("button", { name: "Edit Headlamp" }).click();
  const dialog = page.getByRole("dialog", { name: "Edit Headlamp" });
  await dialog.getByLabel("Install Headlamp").uncheck();
  await dialog.getByRole("button", { name: "Add to plan" }).click();
  await expect(dialog).toBeHidden();
  await expect(staged(page).getByText("1 change not applied")).toBeVisible();

  expect(await applyStaged(page, problems)).toEqual({ variables: { headlamp: { enabled: false } } });
  expect(await variables(page)).toMatchObject({ headlamp: { enabled: true } });
});
