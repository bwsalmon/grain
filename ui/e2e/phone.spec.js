import { test, expect } from "@playwright/test";

// The phone shell (src/phone.js, src/components/PhoneNav.jsx), in the one
// suite that can actually see a layout: jsdom has no layout engine and no
// stylesheet, so the component tests next door can assert which elements
// render and nothing at all about how wide they end up. Everything this
// change is *about* -- a rail that is not taking half the window, a pane
// that fills the screen, a page that does not scroll sideways -- is only
// checkable here.
//
// Two viewports, and the second matters as much as the first: the brief
// was a phone-friendly UI that leaves the tablet exactly as it was, so
// the tablet half of that is asserted rather than assumed.
const PHONE = { width: 390, height: 844 }; // a modern phone, portrait
const TABLET = { width: 820, height: 1180 }; // an iPad Air, portrait

// src/theme.js's SIDEBAR_WIDTH, repeated rather than imported: these
// specs run in plain Node, which cannot resolve the MUI import that file
// pulls in. If the rail is ever resized, the tablet assertion below is
// what says so.
const SIDEBAR_WIDTH = 232;

test.describe("on a phone", () => {
  test.use({ viewport: PHONE, hasTouch: true, isMobile: true });

  test("hides the nav rail behind the bar, and opens it as a drawer", async ({
    page,
  }) => {
    await page.goto("/");
    await expect(page.locator(".task-row").first()).toBeVisible();

    // The rail is not on screen: its entries are unreachable until the
    // menu is tapped, and the page has the whole width.
    await expect(page.getByText("Settings")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "grain" })).toBeVisible();

    await page.getByRole("button", { name: "Open navigation" }).tap();
    await expect(page.getByText("Settings")).toBeVisible();

    // The same rail, doing the same job -- and getting out of the way
    // once it has done it.
    await page.getByText("Board", { exact: true }).tap();
    await expect(page.locator(".board-column").first()).toBeVisible();
    await expect(page.getByText("Settings")).toHaveCount(0);
  });

  test("does not scroll sideways", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator(".task-row").first()).toBeVisible();

    // A task row's chips (repo, base, capabilities) are wider than a
    // phone laid end to end, which is what the wrapping rules in
    // style.css are for. A document wider than its own viewport is the
    // symptom if they ever stop applying.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow).toBeLessThanOrEqual(0);
  });

  test("gives a task's pane the whole width", async ({ page }) => {
    await page.goto("/");
    await page.locator(".task-row").first().tap();

    const pane = page.locator(".MuiDialog-paper");
    await expect(pane).toBeVisible();
    const box = await pane.boundingBox();
    expect(Math.round(box.width)).toBe(PHONE.width);
  });

  // One column per screen, so the board is swiped through rather than
  // read three-thirds at a time.
  test("gives a board column most of the screen", async ({ page }) => {
    await page.goto("/board");

    const column = page.locator(".board-column").first();
    await expect(column).toBeVisible();
    const box = await column.boundingBox();
    expect(box.width).toBeGreaterThan(PHONE.width * 0.75);
  });
});

// The tablet is the layout this change is careful not to touch: the rail
// stands beside the page, and a pane starts exactly where the rail ends.
test.describe("on a tablet", () => {
  test.use({ viewport: TABLET, hasTouch: true, isMobile: true });

  test("keeps the rail beside the page", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator(".task-row").first()).toBeVisible();

    await expect(page.getByText("Settings")).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Open navigation" }),
    ).toHaveCount(0);

    await page.locator(".task-row").first().tap();
    const box = await page.locator(".MuiDialog-paper").boundingBox();
    expect(Math.round(box.width)).toBe(TABLET.width - SIDEBAR_WIDTH);
  });
});
