import { test, expect } from "@playwright/test";

// The board (TaskBoard.jsx, grain/task-287) against a real server and a
// real browser -- see tasks.spec.js's own doc comment for what this
// suite is and what `grain demo` has already seeded by the time it runs
// (one task in every state, "Spike: websocket transport" being the
// closed one).
//
// Two things here are worth a real browser rather than another jsdom
// test beside the component's own: /board as a route somebody can load
// cold, and a column layout that has to survive a reload because it
// lives in localStorage rather than on the server.

test("lays the seeded tasks out in the board's columns", async ({ page }) => {
  await page.goto("/");

  await page.getByRole("button", { name: "Board" }).click();

  await expect(page).toHaveURL(/\/board$/);
  await expect(page.getByRole("heading", { name: "Board" })).toBeVisible();
  // The seeded running task, in the column that collects "running".
  const running = page.locator(".board-column", { hasText: "Running" }).first();
  await expect(running.locator(".board-card", { hasText: "Bump the Go toolchain to 1.24" })).toBeVisible();
  // Closed tasks are off the default board, and it says so rather than
  // quietly showing fewer tasks than the deployment has.
  await expect(page.getByText("Spike: websocket transport")).toHaveCount(0);
  await expect(page.getByText(/in no column \(Closed\)/)).toBeVisible();
});

test("loads /board cold", async ({ page }) => {
  await page.goto("/board");

  await expect(page.getByRole("heading", { name: "Board" })).toBeVisible();
  await expect(page.locator(".board-column").first()).toBeVisible();
});

test("keeps an edited column layout across a reload", async ({ page }) => {
  await page.goto("/board");

  await page.getByRole("button", { name: "Columns" }).click();
  await expect(page.getByText("Board columns")).toBeVisible();
  await page.getByRole("button", { name: "+ Add column" }).click();
  const title = page.getByLabel(/Column \d+ title/).last();
  await title.fill("Archive");
  await page.getByLabel("States").last().click();
  await page.getByRole("option", { name: "Closed" }).click();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "Save" }).click();

  const archive = page.locator(".board-column", { hasText: "Archive" });
  await expect(archive.locator(".board-card", { hasText: "Spike: websocket transport" })).toBeVisible();

  await page.reload();

  await expect(page.locator(".board-column", { hasText: "Archive" })
    .locator(".board-card", { hasText: "Spike: websocket transport" })).toBeVisible();

  // Put the default board back, so a rerun of this suite starts from the
  // same place a first run did.
  await page.getByRole("button", { name: "Columns" }).click();
  await page.getByRole("button", { name: "Reset to default" }).click();
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.locator(".board-column", { hasText: "Archive" })).toHaveCount(0);
});
