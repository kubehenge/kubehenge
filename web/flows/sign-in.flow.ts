import { expect, sent, test } from "./fixture.ts";

// The demo takes any password, so a wrong password's message has no flow to show here: the Go test
// TestDemoSignsInAgainAfterSignOut holds that, and the interface's own tests hold the message.
test("signs out and in again", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: /^Fleet/u })).toBeVisible();

  for (const password of ["first", "second"]) {
    const out = sent(page, "DELETE", "/api/session");
    await page.getByRole("button", { name: "Sign out" }).click();
    await out;
    await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
    await expect(page.getByRole("heading", { name: /^Fleet/u })).toBeHidden();

    await page.getByLabel("User name").fill("admin");
    await page.getByLabel("Password").fill(password);
    await expect(page.getByLabel("Realm")).toHaveValue("pve");
    const signIn = sent(page, "POST", "/api/session");
    await page.getByRole("button", { name: "Sign in" }).click();
    expect((await signIn).postDataJSON()).toEqual({ username: "admin", realm: "pve", password });
    await expect(page.getByRole("heading", { name: /^Fleet/u })).toBeVisible();
    await expect(page.getByRole("link", { name: /clusters\/payments/u })).toBeVisible();
  }
});
