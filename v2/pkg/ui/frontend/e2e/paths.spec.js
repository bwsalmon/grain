import { test, expect } from "@playwright/test";

// App.jsx now keeps the address bar in sync with which sub-page is
// showing (bwsalmon/agents#548) via plain history.pushState/popstate
// rather than a routing library, and pkg/ui/server.go's own spaHandler
// falls back to index.html for any path that isn't a real static asset
// so a direct hit on one of these URLs -- not just a click starting
// from "/" -- gets a real answer. Both halves only matter together
// against a real server: jsdom component tests never issue a real HTTP
// request for "/repos" the way a bookmark or a page reload does, so
// this suite (see tasks.spec.js's own doc comment for why it's the one
// exception to "jsdom, not a browser") is the only place a regression
// in either half would show up.

test("loads directly into each sidebar sub-page from its URL", async ({ page }) => {
  await page.goto("/repos");
  await expect(page.locator(".repo-list")).toBeVisible();

  await page.goto("/schedules");
  await expect(page.getByRole("heading", { name: "Scheduled tasks" })).toBeVisible();

  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: "Task templates" })).toBeVisible();

  // /logs and /sandboxes were sidebar destinations of their own until
  // both moved into Settings' Debug tab (bwsalmon/agents#623); paths.js
  // dropped them from VIEWS deliberately, so a stale bookmark to either
  // is now just an unrecognized path and lands on the tasks view.
  await page.goto("/logs");
  await expect(page.locator(".task-row").first()).toBeVisible();

  await page.goto("/sandboxes");
  await expect(page.locator(".task-row").first()).toBeVisible();
});

test("deep-links to a task's detail overlay, including after a hard reload", async ({ page }) => {
  await page.goto("/");
  await page.locator(".task-row").first().click();
  const heading = page.locator(".detail-header h2");
  await expect(heading).toBeVisible();
  const title = await heading.innerText();
  await expect(page).toHaveURL(/\/tasks\/[^/]+$/);

  // The URL alone -- not a click -- is what has to reopen the same
  // task: a real GET for it has to fall back to index.html server-side,
  // then App.jsx's own mount effect has to notice /tasks/:id and fetch
  // that task's detail before anything else has rendered.
  await page.reload();
  await expect(page.locator(".detail-header h2", { hasText: title })).toBeVisible();
});

test("updates the URL when navigating the sidebar, and restores the previous page on back", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();
  await expect(page).toHaveURL(/\/repos$/);

  await page.getByRole("button", { name: /^Scheduled tasks/ }).click();
  await expect(page).toHaveURL(/\/schedules$/);

  await page.goBack();
  await expect(page).toHaveURL(/\/repos$/);
  await expect(page.locator(".repo-list")).toBeVisible();

  await page.goBack();
  await expect(page).toHaveURL("/");
  await expect(page.locator(".task-row").first()).toBeVisible();
});
