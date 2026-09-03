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

  it("closes from the close button in either shape", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const { rerender } = render(<Overlay onClose={onClose}>body</Overlay>);

    await user.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onClose).toHaveBeenCalledTimes(1);

    rerender(<Overlay onClose={onClose} pane>body</Overlay>);
    await user.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
