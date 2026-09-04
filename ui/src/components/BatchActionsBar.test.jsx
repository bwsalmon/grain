import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import BatchActionsBar from "./BatchActionsBar.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("BatchActionsBar", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    api.mockReset();
  });

  it("renders nothing when no task is selected", () => {
    const { container } = render(
      <BatchActionsBar
        count={0}
        config={null}
        onRun={() => {}}
        onClear={() => {}}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the selection count", () => {
    render(
      <BatchActionsBar
        count={3}
        config={null}
        onRun={() => {}}
        onClear={() => {}}
      />,
    );
    expect(screen.getByText("3 selected")).toBeInTheDocument();
  });

  it("runs the approve action against each selected task via onRun", async () => {
    const onRun = vi.fn((mutate) => mutate(42));
    const user = userEvent.setup();
    render(
      <BatchActionsBar
        count={2}
        config={null}
        onRun={onRun}
        onClear={() => {}}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Approve" }));

    expect(onRun).toHaveBeenCalledTimes(1);
    expect(api).toHaveBeenCalledWith("/api/tasks/42/approve", {
      method: "POST",
    });
  });

  it("asks for confirmation before closing, and does nothing when declined", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    const onRun = vi.fn();
    const user = userEvent.setup();
    render(
      <BatchActionsBar
        count={2}
        config={null}
        onRun={onRun}
        onClear={() => {}}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Close" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(onRun).not.toHaveBeenCalled();
  });

  it("runs the close action once confirmed", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const onRun = vi.fn();
    const user = userEvent.setup();
    render(
      <BatchActionsBar
        count={2}
        config={null}
        onRun={onRun}
        onClear={() => {}}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Close" }));

    expect(onRun).toHaveBeenCalledTimes(1);
  });

  it("disables Attach/Detach until a capability is picked", async () => {
    const config = { capabilities: [{ id: "web-search", name: "Web search" }] };
    const user = userEvent.setup();
    render(
      <BatchActionsBar
        count={1}
        config={config}
        onRun={() => {}}
        onClear={() => {}}
      />,
    );

    expect(screen.getByRole("button", { name: "Attach" })).toBeDisabled();

    await user.selectOptions(screen.getByRole("combobox"), "web-search");

    expect(screen.getByRole("button", { name: "Attach" })).toBeEnabled();
  });

  it("clears the selection via the clear button", async () => {
    const onClear = vi.fn();
    const user = userEvent.setup();
    render(
      <BatchActionsBar
        count={1}
        config={null}
        onRun={() => {}}
        onClear={onClear}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Clear selection" }));

    expect(onClear).toHaveBeenCalledTimes(1);
  });
});
