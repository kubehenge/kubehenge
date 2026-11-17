import { expect, test } from "./fixture.ts";

// Each view, with a text it shows once its data is in. The fixture fails a test whose page logged a
// console error or a failed request.
const pages = [
  ["the fleet", "/", "payments"],
  ["a cluster", "/clusters/clusters/web", "Placement"],
  ["its machines", "/clusters/clusters/web/machines", "web-md-0-82ffn"],
  ["its health", "/clusters/clusters/payments/health", "Needs attention"],
  ["its backups", "/clusters/clusters/web/backups", "before-ingress-change"],
  ["its options", "/clusters/clusters/web/options", "Istio"],
  ["its activity", "/clusters/clusters/web/activity", "changed cluster"],
  ["a run", "/runs/change/clusters/edge", "Change Kubernetes version"],
  ["the cluster wizard", "/clusters/new", "Distribution"],
] as const;

for (const [name, path, ready] of pages) {
  test(`opens ${name}`, async ({ page }) => {
    await page.goto(path);
    await expect(page.getByText(ready).first()).toBeVisible();
    await expect(page.locator(".skeleton, .loading")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible();
  });
}
