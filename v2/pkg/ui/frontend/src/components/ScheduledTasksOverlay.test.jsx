import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import ScheduledTasksOverlay from "./ScheduledTasksOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const schedule = {
  id: "sched-1",
  title: "Nightly dependency bump",
  repo: "acme/widgets",
  interval: "24h0m0s",
  enabled: true,
  nextRunAt: "2026-08-29T00:00:00Z",
};

describe("ScheduledTasksOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists schedules fetched on open", async () => {
    api.mockResolvedValue([schedule]);
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("every 24h0m0s")).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/schedules");
  });

  it("shows a paused badge and an empty message when there are none", async () => {
    api.mockResolvedValue([]);
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("No scheduled tasks.")).toBeInTheDocument();
  });

  it("shows Paused for a disabled schedule, and Resume as its action", async () => {
    api.mockResolvedValue([{ ...schedule, enabled: false }]);
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("Paused")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
  });

  it("submits a new schedule with the expected fields", async () => {
    api.mockResolvedValueOnce([]).mockResolvedValueOnce({}).mockResolvedValueOnce([schedule]);
    const user = userEvent.setup();
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("No scheduled tasks.");

    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Interval/), "24h");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", {
      method: "POST",
      body: JSON.stringify({
        title: "Nightly dependency bump",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        interval: "24h",
      }),
    });
  });

  it("toggles a schedule's enabled state via PATCH", async () => {
    api.mockResolvedValueOnce([schedule]).mockResolvedValueOnce({}).mockResolvedValueOnce([{ ...schedule, enabled: false }]);
    const user = userEvent.setup();
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("Nightly dependency bump");

    await user.click(screen.getByRole("button", { name: "Pause" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", {
      method: "PATCH",
      body: JSON.stringify({ enabled: false }),
    });
  });

  it("deletes a schedule after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce([schedule]).mockResolvedValueOnce({}).mockResolvedValueOnce([]);
    const user = userEvent.setup();
    render(<ScheduledTasksOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("Nightly dependency bump");

    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", { method: "DELETE" });
    vi.unstubAllGlobals();
  });

  it("offers a repo dropdown when the deployment has known repos, instead of a bare text field", async () => {
    api.mockResolvedValueOnce([]).mockResolvedValueOnce({}).mockResolvedValueOnce([schedule]);
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const user = userEvent.setup();
    render(<ScheduledTasksOverlay config={config} onClose={() => {}} showError={() => {}} />);
    await screen.findByText("No scheduled tasks.");

    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/other");
    await user.type(screen.getByLabelText(/Interval/), "24h");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", {
      method: "POST",
      body: JSON.stringify({
        title: "Nightly dependency bump",
        description: "",
        repo: "acme/other",
        base: "",
        autoMerge: false,
        interval: "24h",
      }),
    });
  });

  it("reports the error and leaves the overlay open when creation fails", async () => {
    api.mockResolvedValueOnce([]).mockRejectedValueOnce(new Error("interval must be positive"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<ScheduledTasksOverlay onClose={() => {}} showError={showError} />);
    await screen.findByText("No scheduled tasks.");

    await user.type(screen.getByLabelText(/Title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Interval/), "0s");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "interval must be positive" }));
  });
});
