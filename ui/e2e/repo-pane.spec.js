import { test, expect } from "@playwright/test";

// A repo has a page of its own (grain/task-111), at /repos/{owner}/
// {name}: the list is one plain row per repo, and everything about one
// repo -- its tasks, its branches, its default capabilities, its
// releases -- is on that repo's page. The list rows used to carry all of
// it as buttons, each depending on not also firing the row's own
// navigation, which is exactly the kind of real-click, real-DOM behavior
// tasks.spec.js's own doc comment says jsdom component tests can't be
// trusted to catch; the same goes for the page those clicks land on now.

test("opens a repo's own page from a plain list row and files a task scoped to it", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();

  const row = page.locator(".repo-list li").first();
  await expect(row).toBeVisible();
  const repoName = await row.locator(".repo-list-name").innerText();
  // Nothing on the row to press but the row itself.
  await expect(row.locator("button")).toHaveCount(0);

  await row.click();
  await expect(page.getByRole("heading", { name: repoName })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/repos/${repoName}$`));
  // The repo's tasks are the page's own list, no longer folded out of a
  // list row.
  await expect(page.locator(".task-row").first()).toBeVisible();

  const title = `E2E repo-page task ${Date.now()}`;
  await page.getByRole("button", { name: "New task", exact: true }).click();
  await expect(page.getByRole("heading", { name: "New task" })).toBeVisible();
  await expect(page.getByLabel(/Target repo/)).toHaveValue(repoName);
  await page.getByLabel("Title").fill(title);
  await page.getByRole("button", { name: "Create task" }).click();

  await expect(page.locator(".task-row", { hasText: title })).toBeVisible();
});

test("reaches a repo's branches, capabilities and releases from its page", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();

  const row = page.locator(".repo-list li").first();
  const repoName = await row.locator(".repo-list-name").innerText();
  await row.click();

  // All three forms are on the page itself rather than behind a toggle,
  // and each is filled in by a read that has to have landed.
  await expect(page.getByLabel("Branch name")).toBeVisible();
  await expect(
    page.getByText(`A task filed against ${repoName} starts with:`),
  ).toBeVisible();
  await expect(
    page.getByLabel(new RegExp(`Prompt extension for ${repoName}`)),
  ).toBeVisible();

  await page.getByRole("button", { name: "Releases" }).click();
  await expect(
    page.getByRole("heading", { name: `${repoName} releases` }),
  ).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/repos/${repoName}/releases$`));

  // Back out of releases lands on the repo page, not on the list.
  await page.getByRole("button", { name: repoName }).click();
  await expect(page.getByRole("heading", { name: repoName })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/repos/${repoName}$`));
});
