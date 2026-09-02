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
  // "Queue immediately" is left unchecked, so this files as a proposal
  // rather than a queued task -- the next test approves one of those.
  await page.getByRole("button", { name: "Create task" }).click();

  await expect(page.getByRole("heading", { name: "New task" })).toHaveCount(0);
  const row = page.locator(".task-row", { hasText: title });
  await expect(row).toBeVisible();
  await expect(row.locator(".badge-proposed")).toBeVisible();
});

test("approves a proposed task from its detail view", async ({ page }) => {
  await page.goto("/");
  const title = `E2E approve me ${Date.now()}`;

  await page.getByRole("button", { name: "+ New task" }).click();
  await page.getByLabel("Title").fill(title);
  await page.getByLabel(/No repo/).check();
  await page.getByRole("button", { name: "Create task" }).click();

  await page.locator(".task-row", { hasText: title }).click();
  const heading = page.locator(".detail-header h2", { hasText: title });
  await expect(heading).toBeVisible();
  await expect(page.locator(".detail-state .badge-proposed")).toBeVisible();

  await page.getByRole("button", { name: "Approve" }).click();

  await expect(page.locator(".detail-state .badge-queued")).toBeVisible();
  await expect(page.locator(".task-row", { hasText: title }).locator(".badge-queued")).toBeVisible();
});
