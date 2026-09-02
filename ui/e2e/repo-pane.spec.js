import { test, expect } from "@playwright/test";

// Folding tasks out under a repo row, and filing one scoped to it
// (bwsalmon/agents#474), both hinge on a chevron/+ button not also
// firing the row's own onOpenRepo navigation -- exactly the kind of
// real-click, real-DOM behavior tasks.spec.js's own doc comment says
// jsdom component tests can't be trusted to catch.

test("folds/unfolds tasks under a repo and files a new task scoped to it", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();

  const row = page.locator(".repo-list li").first();
  await expect(row).toBeVisible();
  const repoName = await row.locator(".repo-list-name").innerText();

  await expect(row.locator(".task-sublist")).toHaveCount(0);
  await row.getByRole("button", { name: /^Show tasks for/ }).click();
  await expect(row.locator(".task-sublist .task-row").first()).toBeVisible();

  await row.getByRole("button", { name: /^Hide tasks for/ }).click();
  await expect(row.locator(".task-sublist")).toHaveCount(0);

  const title = `E2E repo-pane task ${Date.now()}`;
  await row.getByRole("button", { name: /^New task under/ }).click();
  await expect(page.getByRole("heading", { name: "New task" })).toBeVisible();
  await expect(page.getByLabel(/Target repo/)).toHaveValue(repoName);
  await page.getByLabel("Title").fill(title);
  await page.getByRole("button", { name: "Create task" }).click();

  await row.getByRole("button", { name: /^Show tasks for/ }).click();
  await expect(row.locator(".task-sublist", { hasText: title })).toBeVisible();
});
