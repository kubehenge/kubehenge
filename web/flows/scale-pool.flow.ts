import { cluster, expect, refusal, sent, test } from "./fixture.ts";

test("scales a worker pool through Add to plan and Review plan", async ({ page, problems }) => {
  problems.allow("403 PATCH /api/clusters/clusters/web");
  await page.goto("/clusters/clusters/web");

  await page.getByRole("button", { name: "Scale" }).click();
  const scale = page.getByRole("dialog", { name: "Scale web" });
  await scale.getByLabel("Workers").fill("6");
  const plan = sent(page, "POST", "/api/clusters/clusters/web/plan");
  await scale.getByRole("button", { name: "Add to plan" }).click();
  expect((await plan).postDataJSON()).toEqual({ workerPools: { "md-0": 6 } });
  await expect(scale).toBeHidden();

  const staged = page.getByRole("region", { name: "Staged changes" });
  await expect(staged.getByText("1 change not applied")).toBeVisible();
  await staged.getByRole("button", { name: "Review plan" }).click();

  const review = page.getByRole("dialog", { name: "Review plan" });
  await expect(review.getByText("Adds or removes machines.")).toBeVisible();
  await expect(review.getByText("All online")).toBeVisible();
  await expect(review.getByText("No problems")).toBeVisible();
  await expect(review.getByRole("row", { name: /Workers 4 6/u })).toBeVisible();

  const apply = sent(page, "PATCH", "/api/clusters/clusters/web");
  await review.getByRole("button", { name: "Apply" }).click();
  expect((await apply).postDataJSON()).toEqual({ workerPools: { "md-0": 6 } });
  await expect(review.getByText(refusal)).toBeVisible();
  // The plan stays staged and the dialog usable.
  await expect(review.getByRole("button", { name: "Apply" })).toBeEnabled();
  await review.getByRole("button", { name: "Keep staged" }).click();
  await expect(review).toBeHidden();
  await expect(staged.getByText("1 change not applied")).toBeVisible();

  const web = await cluster(page, "web");
  expect(web).toMatchObject({ workers: { desired: 4 }, workerPools: [{ name: "md-0", replicas: 4 }] });
});
