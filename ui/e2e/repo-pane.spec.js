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

// A repo's page can lay its own tasks out as the board rather than the
// list (grain/task-321). Worth a real browser for the same reason
// /board itself is (board.spec.js): the switch lives in localStorage, so
// "still on the board after a reload" is behavior no jsdom test of the
// page can stand in for.
test("shows one repo's tasks as a board, and stays on it across a reload", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();

  const row = page.locator(".repo-list li").first();
  const repoName = await row.locator(".repo-list-name").innerText();
  await row.click();
  await expect(page.locator(".task-row").first()).toBeVisible();

  // The page's own switch, not the rail's Board entry.
  const views = page.locator(".repo-tasks-switch");
  await views.getByRole("button", { name: "Board" }).click();

  const cards = page.locator(".board-card");
  await expect(cards.first()).toBeVisible();
  await expect(page.locator(".task-row")).toHaveCount(0);
  // Every card is one of this repo's own tasks -- each carries the repo
  // it targets as a chip.
  expect(await page.locator(".board-card", { hasText: repoName }).count()).toBe(
    await cards.count(),
  );
  // Still this repo's page rather than the deployment-wide board: the
  // board is how this page is showing its tasks, not somewhere else.
  await expect(page.getByRole("heading", { name: repoName })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/repos/${repoName}$`));

  await page.reload();
  await expect(page.getByRole("heading", { name: repoName })).toBeVisible();
  await expect(page.locator(".board-card").first()).toBeVisible();

  // Back to the list, so a rerun of this suite starts where a first run
  // did.
  await page
    .locator(".repo-tasks-switch")
    .getByRole("button", { name: "List" })
    .click();
  await expect(page.locator(".task-row").first()).toBeVisible();
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
