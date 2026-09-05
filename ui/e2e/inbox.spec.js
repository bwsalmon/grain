import { test, expect } from "@playwright/test";

// The inbox (InboxPage.jsx, grain/task-20) against a real server and a
// real browser -- see tasks.spec.js's own doc comment for what this
// suite is and what `grain demo` has already seeded by the time it runs.
//
// What is worth a real browser here rather than another jsdom test
// beside the component's own: /inbox as a route somebody can load cold,
// and the two answers that only mean anything end to end -- an approval
// and a reply fired from a row, each of which has to reach the real
// server, change the task's real state, and take the row off this page
// when it does. The reply is also the one place the question a run
// actually parked on (a comment, plus the observation pointing at it) is
// read back out of a real store.
//
// The proposal this suite approves is one it files itself, for the
// reason tasks.spec.js files its own: a test that approved the seeded
// proposal would leave the demo store a state poorer for whatever runs
// next.

test("loads /inbox cold, grouped by what each task is waiting for", async ({
  page,
}) => {
  await page.goto("/inbox");

  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  // The seeded task parked on a question, under the group naming that
  // wait.
  const asking = page.locator(".inbox-group", { hasText: "Answer a question" });
  await expect(
    asking.locator(".task-row", {
      hasText: "Investigate the merge queue stall",
    }),
  ).toBeVisible();
  // The seeded running task is grain's own business, not the reader's.
  await expect(page.getByText("Bump the Go toolchain to 1.24")).toHaveCount(0);
});

test("approves a proposal from its inbox row", async ({ page }) => {
  await page.goto("/");
  const title = `E2E inbox approve ${Date.now()}`;

  await page.getByRole("button", { name: "+ New task" }).click();
  await page.getByLabel("Title").fill(title);
  await page.getByLabel(/No repo/).check();
  // Unchecked so this files as a proposal -- which is what puts it in
  // the inbox in the first place.
  await page.getByLabel(/Queue immediately/).uncheck();
  await page.getByRole("button", { name: "Create task" }).click();

  await page.getByRole("button", { name: /^Inbox/ }).click();
  const row = page.locator(".inbox-item", { hasText: title });
  await expect(row).toBeVisible();

  await row.getByRole("button", { name: "Approve" }).click();

  // Approved means queued, and a queued task is waiting on grain rather
  // than on the reader -- so it leaves this page, and the pane the list's
  // own rows open never appears over it.
  await expect(page.locator(".inbox-item", { hasText: title })).toHaveCount(0);
  await expect(page.locator(".detail-header h2")).toHaveCount(0);
});

test("answers a parked question from its inbox row", async ({ page }) => {
  await page.goto("/inbox");
  const row = page.locator(".inbox-item", {
    hasText: "Investigate the merge queue stall",
  });

  await row.getByRole("button", { name: "Reply" }).click();

  // The question itself, read back off the real store's comment thread.
  await expect(row.locator(".inbox-question")).toContainText(
    "want me to fix both",
  );
  await row.getByRole("textbox").fill("Just unblock the queue for now.");
  await row.getByRole("button", { name: "Send" }).click();

  // A reply requeues the task (Client.AddComment's own "reply reopens"
  // rule), so it stops waiting on anybody and leaves the page.
  await expect(
    page.locator(".inbox-item", {
      hasText: "Investigate the merge queue stall",
    }),
  ).toHaveCount(0);
});
