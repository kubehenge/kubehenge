import { cluster, expect, refusal, sent, test, written } from "./fixture.ts";

test("deletes a cluster once its name is typed", async ({ page, problems }) => {
  problems.allow("403 DELETE /api/clusters/clusters/web");
  await page.goto("/clusters/clusters/web");

  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Delete cluster web" });
  await expect(dialog.getByText("Deletes its machines and persistent volumes")).toBeVisible();
  await expect(dialog.getByText("Backups stay in the target.")).toBeVisible();

  const confirm = dialog.getByRole("button", { name: "Delete cluster" });
  await expect(confirm).toBeDisabled();
  await dialog.getByLabel("Confirm name").fill("wep");
  await expect(confirm).toBeDisabled();
  await dialog.getByLabel("Confirm name").fill("web");
  await expect(confirm).toBeEnabled();

  const request = sent(page, "DELETE", "/api/clusters/clusters/web");
  await confirm.click();
  expect(await written(request)).toEqual({ confirm: "web" });
  await expect(dialog.getByText(refusal)).toBeVisible();
  await expect(confirm).toBeEnabled();

  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  await page.goto("/");
  await expect(page.getByRole("link", { name: "web", exact: true })).toBeVisible();
  expect(await cluster(page, "web")).toMatchObject({ name: "web", deleting: false });
});
