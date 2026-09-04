import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import BoardColumnsOverlay from "./BoardColumnsOverlay.jsx";
import { defaultColumns } from "../board.js";

const columns = [
  { id: "wait", title: "Waiting", states: ["proposed", "queued"] },
  { id: "doing", title: "Doing", states: ["running"] },
];

function renderEditor(overrides = {}) {
  const props = { columns, onSave: vi.fn(), onClose: vi.fn(), ...overrides };
  render(<BoardColumnsOverlay {...props} />);
  return props;
}

describe("BoardColumnsOverlay", () => {
  it("shows every column with its title and its states", () => {
    renderEditor();
    expect(screen.getByLabelText("Column 1 title")).toHaveValue("Waiting");
    expect(screen.getByLabelText("Column 2 title")).toHaveValue("Doing");
    expect(screen.getByText("Proposed, Queued")).toBeInTheDocument();
  });

  it("renames a column", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.clear(screen.getByLabelText("Column 1 title"));
    await user.type(screen.getByLabelText("Column 1 title"), "Backlog");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave.mock.calls[0][0][0]).toMatchObject({
      title: "Backlog",
      states: ["proposed", "queued"],
    });
  });

  it("adds a column, and removes one", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Remove Doing" }));
    await user.click(screen.getByRole("button", { name: "+ Add column" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave.mock.calls[0][0].map((c) => c.title)).toEqual([
      "Waiting",
      "New column",
    ]);
  });

  it("reorders the columns", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Move Doing left" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave.mock.calls[0][0].map((c) => c.title)).toEqual([
      "Doing",
      "Waiting",
    ]);
  });

  it("will not move the first column further left, or the last further right", () => {
    renderEditor();
    expect(
      screen.getByRole("button", { name: "Move Waiting left" }),
    ).toBeDisabled();
    expect(
      screen.getByRole("button", { name: "Move Doing right" }),
    ).toBeDisabled();
  });

  // One state, one column: giving a state to a column takes it away
  // from whichever column had it, so no task can be on the board twice.
  it("takes a state out of the column that had it when another claims it", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.click(screen.getAllByLabelText("States")[1]);
    await user.click(screen.getByRole("option", { name: "Queued" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Save" }));
    const saved = onSave.mock.calls[0][0];
    expect(saved[0].states).toEqual(["proposed"]);
    expect(saved[1].states).toEqual(["running", "queued"]);
  });

  it("says which states the board will not be showing", async () => {
    const user = userEvent.setup();
    renderEditor();
    expect(screen.getByText(/Off the board:/)).toHaveTextContent(
      "Awaiting reply",
    );
    await user.click(screen.getByRole("button", { name: "Remove Doing" }));
    expect(screen.getByText(/Off the board:/)).toHaveTextContent("Running");
  });

  it("puts the default board back", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Reset to default" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith(defaultColumns());
  });

  it("saves the default board rather than one with no columns at all", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Remove Waiting" }));
    await user.click(screen.getByRole("button", { name: "Remove Doing" }));
    expect(screen.getByText(/No columns/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave).toHaveBeenCalledWith(defaultColumns());
  });

  it("names a column somebody emptied the title of after its own states", async () => {
    const user = userEvent.setup();
    const { onSave } = renderEditor();
    await user.clear(screen.getByLabelText("Column 2 title"));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onSave.mock.calls[0][0][1].title).toBe("Running");
  });

  it("changes nothing on Cancel", async () => {
    const user = userEvent.setup();
    const { onSave, onClose } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Remove Doing" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onSave).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it("closes once a save has gone through", async () => {
    const user = userEvent.setup();
    const { onClose } = renderEditor();
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onClose).toHaveBeenCalled();
  });
});
