import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import NewTaskOverlay from "./NewTaskOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn().mockResolvedValue(null) }));

describe("NewTaskOverlay", () => {
  afterEach(() => {
    api.mockClear();
  });

  it("submits a minimal task with the expected defaults", async () => {
    const onCreated = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={onClose} onCreated={onCreated} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", {
      method: "POST",
      body: JSON.stringify({
        title: "Fix the thing",
        description: "",
        repo: "",
        base: "",
        autoMerge: false,
        capabilities: [],
        dependsOn: [],
        reads: [],
        approved: false,
      }),
    });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onCreated).toHaveBeenCalledTimes(1);
  });

  it("adds depends-on tasks via the picker and parses read-only repos, and includes checked capabilities", async () => {
    const config = { capabilities: [{ id: "web-search", name: "Web search" }, { id: "shell", name: "Shell" }] };
    const tasks = [
      { id: "12", title: "Fix the login bug" },
      { id: "15", title: "Add dark mode" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship the other thing");

    const dependsOnInput = screen.getByPlaceholderText("Search tasks to depend on…");
    await user.type(dependsOnInput, "12");
    await user.click(await screen.findByText("Fix the login bug"));
    await user.type(dependsOnInput, "15");
    await user.click(await screen.findByText("Add dark mode"));

    await user.type(screen.getByLabelText(/Read-only repos/), "owner/shared-lib, owner/schema ");
    await user.click(screen.getByRole("checkbox", { name: "Web search" }));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.dependsOn).toEqual(["12", "15"]);
    expect(payload.reads).toEqual(["owner/shared-lib", "owner/schema"]);
    expect(payload.capabilities).toEqual(["web-search"]);
  });

  it("reports the error and leaves the overlay open when the request fails", async () => {
    api.mockRejectedValueOnce(new Error("title is required"));
    const showError = vi.fn();
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={onClose} onCreated={() => {}} showError={showError} />);

    await user.type(screen.getByLabelText(/Title/), "x");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "title is required" }));
    expect(onClose).not.toHaveBeenCalled();
  });
});
