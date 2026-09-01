import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import ConfigurationAgentButton from "./ConfigurationAgentButton.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("ConfigurationAgentButton", () => {
  afterEach(() => {
    api.mockClear();
  });

  it("files a configuration task with no form and opens its chat", async () => {
    api.mockResolvedValueOnce({ id: "42" });
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(<ConfigurationAgentButton defaultRepo={null} onOpenTask={onOpenTask} showError={() => {}} />);

    await user.click(screen.getByRole("button", { name: "Open configuration agent" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", {
      method: "POST",
      body: JSON.stringify({ configuration: true, repo: "" }),
    });
    expect(onOpenTask).toHaveBeenCalledWith("42");
  });

  it("passes the currently scoped repo through as the default", async () => {
    api.mockResolvedValueOnce({ id: "7" });
    const user = userEvent.setup();
    render(<ConfigurationAgentButton defaultRepo="acme/widgets" onOpenTask={() => {}} showError={() => {}} />);

    await user.click(screen.getByRole("button", { name: "Open configuration agent" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", {
      method: "POST",
      body: JSON.stringify({ configuration: true, repo: "acme/widgets" }),
    });
  });

  it("reports a failure through showError instead of opening a task", async () => {
    api.mockRejectedValueOnce(new Error("no repo given"));
    const showError = vi.fn();
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(<ConfigurationAgentButton defaultRepo={null} onOpenTask={onOpenTask} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "Open configuration agent" }));

    expect(showError).toHaveBeenCalledWith(expect.any(Error));
    expect(onOpenTask).not.toHaveBeenCalled();
  });
});
