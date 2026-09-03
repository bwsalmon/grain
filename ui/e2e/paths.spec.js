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
  await expect(page.getByRole("heading", { name: "Schedules" })).toBeVisible();

  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: "Task templates" })).toBeVisible();

  await page.goto("/suites");
  await expect(page.getByRole("heading", { name: "Task suites" })).toBeVisible();

  // /logs and /sandboxes were sidebar destinations of their own until
  // both moved into Settings' Debug tab (bwsalmon/agents#623), then out
  // again onto their own "Debugging" entry at /debug (bwsalmon/
  // agents#640); paths.js never restored them as their own paths, so a
  // stale bookmark to either is still just an unrecognized path and
  // lands on the tasks view.
  await page.goto("/logs");
  await expect(page.locator(".task-row").first()).toBeVisible();

  await page.goto("/sandboxes");
  await expect(page.locator(".task-row").first()).toBeVisible();

  await page.goto("/debug");
  await expect(page.getByRole("heading", { name: "Debug" })).toBeVisible();
});

// A repo's own page is two URL segments deep (grain/task-111), which is
// the first path here the SPA fallback has to answer for that could
// plausibly be mistaken for a real static asset path.
test("loads directly into a repo's own page, and its releases pane, from the URL", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();
  const repoName = await page.locator(".repo-list-name").first().innerText();

  await page.goto(`/repos/${repoName}`);
  await expect(page.getByRole("heading", { name: repoName })).toBeVisible();

  await page.goto(`/repos/${repoName}/releases`);
  await expect(page.getByRole("heading", { name: `${repoName} releases` })).toBeVisible();
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

// grain/task-139: a schedule, a template and a suite each open the same
// pane a task does (grain/task-94), so each is deep-linkable the same
// way -- /schedules/:id, /templates/:id, /suites/:id. `grain demo` seeds
// none of the three, so these file their own through the very endpoints
// the frontend posts to, then ask for the item's URL cold: a real GET
// the SPA fallback has to answer with index.html, and an App.jsx that
// has only the path to tell it which pane to open.
async function seedOne(page, path, data) {
  const res = await page.request.post(path, { data });
  expect(res.ok(), `POST ${path}: ${res.status()}`).toBe(true);
  return res.json();
}

test("deep-links to a schedule, a template and a suite from their own URLs", async ({ page }) => {
  const stamp = Date.now();
  const template = await seedOne(page, "/api/templates", {
    name: `E2E deep-link template ${stamp}`, title: "Bump dependencies",
  });
  const suite = await seedOne(page, "/api/suites", {
    name: `E2E deep-link suite ${stamp}`, templateIds: [template.id], mode: "until_clean", maxPasses: 3,
  });
  const schedule = await seedOne(page, "/api/schedules", {
    title: `E2E deep-link schedule ${stamp}`,
    repo: "acme/widgets",
    recurrence: { kind: "everyNHours", everyNHours: 24 },
  });

  await page.goto(`/templates/${template.id}`);
  await expect(page.getByRole("heading", { name: "Edit template" })).toBeVisible();
  await expect(page.getByLabel("Name")).toHaveValue(`E2E deep-link template ${stamp}`);

  await page.goto(`/suites/${suite.id}`);
  await expect(page.getByRole("heading", { name: "Edit task suite" })).toBeVisible();
  await expect(page.getByLabel("Name")).toHaveValue(`E2E deep-link suite ${stamp}`);

  await page.goto(`/schedules/${schedule.id}`);
  await expect(page.getByRole("heading", { name: "Edit schedule" })).toBeVisible();
  await expect(page.getByLabel(/^Title/)).toHaveValue(`E2E deep-link schedule ${stamp}`);

  // An id no schedule answers to is not a pane: the list shows, and the
  // address bar is corrected back to it rather than left naming
  // something that isn't open.
  await page.goto("/schedules/sched-does-not-exist");
  await expect(page.getByRole("heading", { name: "Schedules" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Edit schedule" })).toHaveCount(0);
  await expect(page).toHaveURL(/\/schedules$/);
});

test("opens a template into its own URL, and back closes the pane rather than the page", async ({ page }) => {
  const name = `E2E back-out template ${Date.now()}`;
  const template = await seedOne(page, "/api/templates", { name, title: "Bump dependencies" });

  await page.goto("/templates");
  await page.locator(".template-row", { hasText: name }).click();
  await expect(page.getByRole("heading", { name: "Edit template" })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/templates/${template.id}$`));

  // Back used to leave the templates page altogether, since the open
  // template was state nothing had pushed a history entry for.
  await page.goBack();
  await expect(page).toHaveURL(/\/templates$/);
  await expect(page.getByRole("heading", { name: "Edit template" })).toHaveCount(0);
  await expect(page.locator(".template-row", { hasText: name })).toBeVisible();

  // ...and forward opens the same pane again.
  await page.goForward();
  await expect(page).toHaveURL(new RegExp(`/templates/${template.id}$`));
  await expect(page.getByRole("heading", { name: "Edit template" })).toBeVisible();
});

test("updates the URL when navigating the sidebar, and restores the previous page on back", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: /^Repos/ }).click();
  await expect(page).toHaveURL(/\/repos$/);

  await page.getByRole("button", { name: /^Schedules/ }).click();
  await expect(page).toHaveURL(/\/schedules$/);

  await page.goBack();
  await expect(page).toHaveURL(/\/repos$/);
  await expect(page.locator(".repo-list")).toBeVisible();

  await page.goBack();
  await expect(page).toHaveURL("/");
  await expect(page.locator(".task-row").first()).toBeVisible();
});
