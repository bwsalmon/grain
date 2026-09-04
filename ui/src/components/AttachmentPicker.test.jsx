import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import AttachmentPicker from "./AttachmentPicker.jsx";

describe("AttachmentPicker", () => {
  // The visible affordance is a paperclip, not the words "Attach files":
  // its aria-label and tooltip are what still say so.
  it("opens the file picker from the paperclip button", async () => {
    const user = userEvent.setup();
    render(<AttachmentPicker files={[]} onChange={() => {}} />);

    const input = document.querySelector('input[type="file"]');
    const click = vi.spyOn(input, "click");
    await user.click(screen.getByRole("button", { name: "Attach files" }));

    expect(click).toHaveBeenCalled();
  });

  it("chips every picked file and removes the one whose chip is deleted", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const one = new File(["a"], "one.png", { type: "image/png" });
    const two = new File(["b"], "two.png", { type: "image/png" });
    render(<AttachmentPicker files={[one, two]} onChange={onChange} />);

    expect(screen.getByText("one.png")).toBeInTheDocument();
    await user.click(screen.getAllByTestId("CancelIcon")[1]);

    expect(onChange).toHaveBeenCalledWith([one]);
  });
});
