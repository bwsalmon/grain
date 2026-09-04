import { test, expect } from "@playwright/test";

// The one suite in this package that boots a real pkg/ui.Server (over a
// real embedded SQLite store) and drives the real *built* frontend --
// playwright.config.js's webServer, `grain demo` -- through an actual
// Chromium instead of jsdom against a component tree in isolation.
// Every other *.test.jsx/*.test.js file under ../src is the latter; none
// of them boot a server or a browser, which left routing, real
// fetch/CORS behavior, layout and the server's actual wire format
// unverified by anything short of a human clicking around
// (bwsalmon/agents#415).
//
// `grain demo`'s own seedDemo (cmd/grain/demo.go) has already put a
// fixed set of tasks, one in every state including "proposed", into the
// store by the time these run (cmd/grain/demo_test.go's
// TestSeedDemoCoversEveryState asserts on that directly); these tests
// read a couple of the seeded titles rather than duplicating that
// assertion, and otherwise file and approve their own tasks so each
// test's outcome doesn't depend on what a previous test in the same run
// left behind.

test("lists the seeded tasks from a real server", async ({ page }) => {
  await page.goto("/");

  await expect(page.getByText("Bump the Go toolchain to 1.24")).toBeVisible();
  await expect(page.getByText("Rotate the Gemini signing key")).toBeVisible();
  await expect(page.locator(".task-row").first()).toBeVisible();
});

test("files a new task through the New task overlay", async ({ page }) => {
  await page.goto("/");
  const title = `E2E new task ${Date.now()}`;

  await page.getByRole("button", { name: "+ New task" }).click();
  await expect(page.getByRole("heading", { name: "New task" })).toBeVisible();
  await page.getByLabel("Title").fill(title);
  // No repo is picked from this top-level "+ New task" button, so
  // "No repo" has to be checked before Create task will even enable
  // (bwsalmon/agents#614).
  await page.getByLabel(/No repo/).check();
  // "Queue immediately" starts checked, from the deployment default that
  // is on unless an operator turns it off (bwsalmon/agents#612), so it is
  // unchecked here to file a proposal rather than a queued task -- the
  // next test approves one of those.
  await page.getByLabel(/Queue immediately/).uncheck();
  await page.getByRole("button", { name: "Create task" }).click();

  await expect(page.getByRole("heading", { name: "New task" })).toHaveCount(0);
  const row = page.locator(".task-row", { hasText: title });
  await expect(row).toBeVisible();
  await expect(row.locator(".badge-proposed")).toBeVisible();
});

// grain/task-91, grain/task-175: a task's own page carries the button
// that shows the whole prompt its agent was handed -- the task list's
// rows no longer do. The seeded running task is the one with a recorded
// prompt (cmd/grain/demo.go builds it with the same
// orchestrator.BuildPrompt a real dispatch uses), so this is the one
// place the button, the route and the recorded prompt are exercised
// together against a real server.
test("shows the full prompt from the task's own page", async ({ page }) => {
  await page.goto("/");

  await page.locator(".task-row", { hasText: "Bump the Go toolchain to 1.24" }).click();
  await expect(page.locator(".detail-header")).toBeVisible();
  await page.getByRole("button", { name: "Prompt" }).click();

  // The prompt opens as its own pane over the task's, so it is picked
  // out by its own heading rather than by being the only dialog on
  // screen -- the task page is still behind it.
  const prompt = page.locator(".MuiDialog-paper", { has: page.getByRole("heading", { name: /^Prompt for/ }) });
  await expect(prompt.getByText(/Push your change to a new branch named/)).toBeVisible();
});

// grain/task-94: a task opens into the whole content area beside the
// sidebar rather than a box floating in the middle of it. Layout is the
// one thing jsdom cannot answer for -- Overlay.test.jsx can only check
// which classes MUI put on the paper -- so where the pane actually lands
// on screen is measured here, against a real browser.
test("opens a task into the pane beside the sidebar", async ({ page }) => {
  await page.goto("/");
  await page.locator(".task-row").first().click();

  const pane = page.locator(".MuiDialog-paper");
  await expect(pane).toBeVisible();

  const sidebar = await page.locator("aside").boundingBox();
  const box = await pane.boundingBox();
  const viewport = page.viewportSize();

  // Flush against the sidebar on the left, the viewport's edge on the
  // right, and its full height -- one pixel of tolerance for a fractional
  // device pixel ratio, not for a pane that landed somewhere else.
  expect(Math.abs(box.x - (sidebar.x + sidebar.width))).toBeLessThanOrEqual(1);
  expect(Math.abs(box.x + box.width - viewport.width)).toBeLessThanOrEqual(1);
  expect(Math.abs(box.height - viewport.height)).toBeLessThanOrEqual(1);
});

test("approves a proposed task from its detail view", async ({ page }) => {
  await page.goto("/");
  const title = `E2E approve me ${Date.now()}`;

  await page.getByRole("button", { name: "+ New task" }).click();
  await page.getByLabel("Title").fill(title);
  await page.getByLabel(/No repo/).check();
  // Unchecked for the same reason as the test above: there is nothing to
  // approve unless this files as a proposal.
  await page.getByLabel(/Queue immediately/).uncheck();
  await page.getByRole("button", { name: "Create task" }).click();

  await page.locator(".task-row", { hasText: title }).click();
  const heading = page.locator(".detail-header h2", { hasText: title });
  await expect(heading).toBeVisible();
  await expect(page.locator(".detail-state .badge-proposed")).toBeVisible();

  await page.getByRole("button", { name: "Approve" }).click();

  await expect(page.locator(".detail-state .badge-queued")).toBeVisible();
  await expect(page.locator(".task-row", { hasText: title }).locator(".badge-queued")).toBeVisible();
});
