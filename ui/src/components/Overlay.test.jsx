import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import Overlay from "./Overlay.jsx";

// The three widths Overlay draws, and which one each of its callers is
// entitled to: a centered box for an action (New task, Run a suite), a
// wider one for an action that shows a lot (an attempt's transcript),
// and the full pane beside the sidebar for anything you *open* -- a
// task, a schedule, a template, a suite (grain/task-94), plus Settings
// and Debugging, which are destinations rather than actions
// (grain/task-115).
describe("Overlay", () => {
  it("draws a centered dialog by default", () => {
    render(<Overlay onClose={() => {}}>body</Overlay>);

    const paper = document.querySelector(".MuiDialog-paper");
    expect(paper).toHaveClass("MuiDialog-paperWidthSm");
    expect(paper).not.toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane")).toBeNull();
  });

  it("widens the centered dialog when asked", () => {
    render(<Overlay onClose={() => {}} wide>body</Overlay>);

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperWidthMd");
  });

  // The pane is the full height of the viewport, so its own body is what
  // scrolls (.overlay-pane in style.css) rather than the document -- the
  // markup that split depends on is what this checks for.
  it("fills the pane beside the sidebar when pane is set", () => {
    render(<Overlay onClose={() => {}} pane><p>body</p></Overlay>);

    const paper = document.querySelector(".MuiDialog-paper");
    expect(paper).toHaveClass("MuiDialog-paperFullScreen");
    const body = document.querySelector(".overlay-pane");
    expect(body).toBeInTheDocument();
    expect(body).toContainElement(screen.getByText("body"));
  });

  // A pane's header is chrome that has to sit outside the part that
  // scrolls (Settings' and Debugging's tab strips, grain/task-115) --
  // inside .overlay-pane it would scroll away with everything else.
  it("pins a pane's header above the scrolling body", () => {
    render(<Overlay onClose={() => {}} pane header={<h2>Settings</h2>}><p>body</p></Overlay>);

    const head = document.querySelector(".overlay-pane-header");
    expect(head).toContainElement(screen.getByRole("heading", { name: "Settings" }));
    expect(document.querySelector(".overlay-pane")).not.toContainElement(head);
  });

  it("draws no header element for a pane that passes none", () => {
    render(<Overlay onClose={() => {}} pane><p>body</p></Overlay>);

    expect(document.querySelector(".overlay-pane-header")).toBeNull();
  });

  it("closes a centered dialog from the close button", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<Overlay onClose={onClose}>body</Overlay>);

    await user.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  // grain/task-177: a pane leaves the way a repo's page does -- a back
  // button in the top-left corner, not an X in the top-right. Both are
  // destinations with their own URL, so both should be left by the same
  // gesture in the same corner.
  it("leaves a pane by a back button on the left, not a close button on the right", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<Overlay onClose={onClose} pane>body</Overlay>);

    expect(screen.queryByRole("button", { name: "Close dialog" })).toBeNull();
    const back = screen.getByRole("button", { name: "← Back" });
    expect(document.querySelector(".overlay-pane-back")).toContainElement(back);

    await user.click(back);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  // The panes that only ever open from one list name it, the way
  // RepoPage's own button says "← Repos" rather than "← Back".
  it("names where the back button lands when given a label", () => {
    render(<Overlay onClose={() => {}} pane backLabel="Templates">body</Overlay>);

    expect(screen.getByRole("button", { name: "← Templates" })).toBeInTheDocument();
  });

  // Above the header, not over it: a floating button in the corner used
  // to cost the tab strip beside it 3rem of right padding to stay clear
  // of, and a pane's title now starts at the pane's own left edge with
  // nothing on top of it.
  it("puts the back button above a pane's fixed header", () => {
    render(<Overlay onClose={() => {}} pane header={<h2>Settings</h2>}>body</Overlay>);

    const back = document.querySelector(".overlay-pane-back");
    const head = document.querySelector(".overlay-pane-header");
    expect(back).not.toContainElement(head);
    expect(back.compareDocumentPosition(head) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
