import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SchedulesList from "./SchedulesList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const schedule = {
  id: "sched-1",
  title: "Nightly dependency bump",
  description: "",
  repo: "acme/widgets",
  base: "",
  autoMerge: false,
  interval: "24h0m0s",
  enabled: true,
  nextRunAt: "2026-08-29T00:00:00Z",
};

const noop = () => {};

describe("SchedulesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the schedules it is given", () => {
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("every 24h0m0s")).toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No scheduled tasks.")).toBeInTheDocument();
  });

  it("shows Paused for a disabled schedule, and Resume as its action", () => {
    render(<SchedulesList schedules={[{ ...schedule, enabled: false }]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
  });

  it("submits a new schedule with the expected fields", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

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
    expect(onRefresh).toHaveBeenCalled();
  });

  it("toggles a schedule's enabled state via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Pause" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", {
      method: "PATCH",
      body: JSON.stringify({ enabled: false }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("deletes a schedule after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("opens an edit form pre-filled with the schedule's fields and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));

    const titleField = screen.getAllByLabelText(/Title/)[0];
    expect(titleField).toHaveValue("Nightly dependency bump");

    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", {
      method: "PATCH",
      body: JSON.stringify({
        title: "Weekly dependency bump",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        interval: "24h0m0s",
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  it("offers a repo dropdown when the deployment has known repos, instead of a bare text field", async () => {
    api.mockResolvedValueOnce({});
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} config={config} tasks={[]} onRefresh={onRefresh} showError={noop} />);

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
    expect(onRefresh).toHaveBeenCalled();
  });

  it("reports the error and leaves the form open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("interval must be positive"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={showError} />);

    await user.type(screen.getByLabelText(/Title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Interval/), "0s");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "interval must be positive" }));
  });
});
