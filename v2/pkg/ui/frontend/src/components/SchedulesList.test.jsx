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
  reads: [],
  capabilities: [],
  recurrence: { kind: "everyNHours", everyNHours: 24 },
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
    expect(screen.getByText("every 24h")).toBeInTheDocument();
  });

  it("describes daily, weekly and monthly recurrences", () => {
    const daily = { ...schedule, id: "sched-daily", recurrence: { kind: "daily", timeOfDay: "09:00" } };
    const weekly = { ...schedule, id: "sched-weekly", recurrence: { kind: "weekly", timeOfDay: "14:30", weekday: "friday" } };
    const monthly = { ...schedule, id: "sched-monthly", recurrence: { kind: "monthly", timeOfDay: "00:00", dayOfMonth: 31 } };
    render(<SchedulesList schedules={[daily, weekly, monthly]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("daily at 09:00")).toBeInTheDocument();
    expect(screen.getByText("Fridays at 14:30")).toBeInTheDocument();
    expect(screen.getByText("monthly on day 31 at 00:00")).toBeInTheDocument();
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

  it("submits a new schedule with the expected fields, defaulting to every 24 hours", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", {
      method: "POST",
      body: JSON.stringify({
        title: "Nightly dependency bump",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
        recurrence: { kind: "everyNHours", everyNHours: 24 },
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("submits a daily recurrence with the chosen time", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByLabelText(/Title/), "Morning report");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByLabelText("Repeat"));
    await user.click(await screen.findByRole("option", { name: "Daily, at a time" }));
    const timeField = screen.getByLabelText(/Time \(UTC\)/);
    await user.clear(timeField);
    await user.type(timeField, "07:15");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.recurrence).toEqual({ kind: "daily", timeOfDay: "07:15" });
  });

  it("submits a weekly recurrence with the chosen day and time", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByLabelText(/Title/), "Weekly digest");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByLabelText("Repeat"));
    await user.click(await screen.findByRole("option", { name: "Weekly, on a day and time" }));
    await user.click(screen.getByLabelText("Day of week"));
    await user.click(await screen.findByRole("option", { name: "Friday" }));
    const timeField = screen.getByLabelText(/Time \(UTC\)/);
    await user.clear(timeField);
    await user.type(timeField, "16:00");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.recurrence).toEqual({ kind: "weekly", timeOfDay: "16:00", weekday: "friday" });
  });

  it("submits a monthly recurrence with the chosen day of month and time", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByLabelText(/Title/), "Monthly cleanup");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByLabelText("Repeat"));
    await user.click(await screen.findByRole("option", { name: "Monthly, on a day and time" }));
    const dayField = screen.getByLabelText(/Day of month/);
    await user.clear(dayField);
    await user.type(dayField, "31");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.recurrence).toEqual({ kind: "monthly", timeOfDay: "09:00", dayOfMonth: 31 });
  });

  it("parses read-only repos and includes checked capabilities", async () => {
    api.mockResolvedValueOnce({});
    const config = { capabilities: [{ id: "gemini-key", name: "Gemini key" }, { id: "self-debug", name: "Self debug" }] };
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} config={config} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByLabelText(/Title/), "Ship the other thing");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Read-only repos/), "owner/shared-lib, owner/schema ");
    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Gemini key" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.reads).toEqual(["owner/shared-lib", "owner/schema"]);
    expect(payload.capabilities).toEqual(["gemini-key"]);
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
        reads: [],
        capabilities: [],
        recurrence: { kind: "everyNHours", everyNHours: 24 },
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
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.repo).toBe("acme/other");
  });

  it("reports the error and leaves the form open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("everyNHours must be positive"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={showError} />);

    await user.type(screen.getByLabelText(/Title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "everyNHours must be positive" }));
  });
});
