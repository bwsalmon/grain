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
  createdAt: "2026-01-01T00:00:00Z",
};

const noop = () => {};

describe("SchedulesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the schedules it is given, showing just their key details", () => {
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("every 24h")).toBeInTheDocument();
    // No form fields on the main list any more -- editing lives behind
    // clicking a row instead.
    expect(screen.queryByLabelText(/Target repo/)).not.toBeInTheDocument();
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

  it("shows a Paused chip for a disabled schedule", () => {
    render(<SchedulesList schedules={[{ ...schedule, enabled: false }]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Paused")).toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No scheduled tasks.")).toBeInTheDocument();
    // Nothing to search or sort when the list is empty.
    expect(screen.queryByPlaceholderText("Search schedules…")).not.toBeInTheDocument();
  });

  it("filters the list by title or repo", async () => {
    const other = { ...schedule, id: "sched-2", title: "Weekly digest", repo: "acme/other" };
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule, other]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search schedules…"), "digest");

    expect(screen.getByText("Weekly digest")).toBeInTheDocument();
    expect(screen.queryByText("Nightly dependency bump")).not.toBeInTheDocument();
  });

  it("shows a message when a search matches nothing", async () => {
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search schedules…"), "nope");

    expect(screen.getByText("No schedules match your search.")).toBeInTheDocument();
  });

  it("opens a blank overlay from the + button and submits a new schedule, defaulting to every 24 hours", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    expect(screen.getByRole("heading", { name: "New schedule" })).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", {
      method: "POST",
      body: JSON.stringify({
        templateId: "",
        recurrence: { kind: "everyNHours", everyNHours: 24 },
        title: "Nightly dependency bump",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New schedule" })).not.toBeInTheDocument();
  });

  it("submits a daily recurrence with the chosen time", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
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

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
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

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
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

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
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

  it("opens a row's overlay pre-filled and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    expect(screen.getByRole("heading", { name: "Edit schedule" })).toBeInTheDocument();

    const titleField = screen.getByLabelText(/Title/);
    expect(titleField).toHaveValue("Nightly dependency bump");

    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", {
      method: "PATCH",
      body: JSON.stringify({
        templateId: "",
        recurrence: { kind: "everyNHours", everyNHours: 24 },
        title: "Weekly dependency bump",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Edit schedule" })).not.toBeInTheDocument();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  it("pauses a schedule from its overlay via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    await user.click(screen.getByRole("button", { name: "Pause" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", {
      method: "PATCH",
      body: JSON.stringify({ enabled: false }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Edit schedule" })).not.toBeInTheDocument();
  });

  it("shows Resume as the pause/resume action for a disabled schedule", async () => {
    const user = userEvent.setup();
    render(<SchedulesList schedules={[{ ...schedule, enabled: false }]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));

    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
  });

  it("deletes a schedule from its overlay after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("offers a repo dropdown when the deployment has known repos, instead of a bare text field", async () => {
    api.mockResolvedValueOnce({});
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} config={config} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/other");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.repo).toBe("acme/other");
  });

  it("hides the content fields but keeps repo/base when a template is chosen", async () => {
    api.mockResolvedValueOnce({});
    const templates = [{ id: "template-1", name: "Dependency bump" }];
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} templates={templates} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.click(screen.getByLabelText("Template"));
    await user.click(await screen.findByRole("option", { name: "Dependency bump" }));

    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();
    // Repo and base are never among a template's own content (a
    // template carries no target of its own), so they still render.
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");

    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload).toEqual({
      templateId: "template-1",
      repo: "acme/widgets",
      base: "",
      recurrence: { kind: "everyNHours", everyNHours: 24 },
    });
  });

  it("pre-fills the template select when editing a template-backed schedule, and can detach from it", async () => {
    api.mockResolvedValueOnce({});
    const templates = [{ id: "template-1", name: "Dependency bump" }];
    const templateBacked = { ...schedule, templateId: "template-1", templateName: "Dependency bump" };
    const user = userEvent.setup();
    render(<SchedulesList schedules={[templateBacked]} templates={templates} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    const templateSelect = screen.getByLabelText("Template");
    expect(templateSelect).toHaveTextContent("Dependency bump");
    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();

    await user.click(templateSelect);
    await user.click(await screen.findByRole("option", { name: "None -- fill in the fields below" }));
    expect(screen.getByLabelText(/^Title/)).toHaveValue("Nightly dependency bump");

    await user.click(screen.getByRole("button", { name: "Save" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.templateId).toBe("");
    expect(payload.title).toBe("Nightly dependency bump");
  });

  it("reports the error and leaves the overlay open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("everyNHours must be positive"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<SchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.type(screen.getByLabelText(/Title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "everyNHours must be positive" }));
    expect(screen.getByRole("heading", { name: "New schedule" })).toBeInTheDocument();
  });
});
