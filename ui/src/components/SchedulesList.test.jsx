import { useState } from "react";
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

// Which schedule is open is App.jsx's state now, so that the URL can
// name it (/schedules/:id, grain/task-139) -- these tests stand in for
// App with the smallest wrapper that holds that one piece of state, so
// clicking a row still opens the pane the way it does in the app.
function ControlledSchedulesList(props) {
  const [openScheduleId, setOpenScheduleId] = useState(null);
  return <SchedulesList openScheduleId={openScheduleId} onOpenSchedule={setOpenScheduleId} {...props} />;
}

// The body api was last called with, parsed rather than compared as a
// string. ScheduleOverlay builds a save payload in two steps -- repo and
// base in the object literal, title and description assigned onto it
// afterwards -- so the key order JSON.stringify emits is an artifact of
// that split, not part of the request. Asserting on the serialized string
// makes a test fail the moment a field moves between the two halves, with
// the same fields and the same values going over the wire.
const lastBody = () => JSON.parse(api.mock.lastCall[1].body);

describe("SchedulesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the schedules it is given, showing just their key details", () => {
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Nightly dependency bump")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("every 24h")).toBeInTheDocument();
    // No form fields on the main list any more -- editing lives behind
    // clicking a row instead.
    expect(screen.queryByLabelText(/Target repo/)).not.toBeInTheDocument();
  });

  // The page's heading carries the same figure as the nav entry that
  // opened it (ItemGlyph.jsx, docs/brand.md) -- decoration beside the
  // heading rather than inside it, so "Schedules" is still the whole of
  // the heading's accessible name.
  it("heads the page with the schedules glyph, without renaming the heading", () => {
    const { container } = render(
      <ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />,
    );

    expect(container.querySelector('svg[data-glyph="schedules"]')).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Schedules" })).toBeInTheDocument();
  });

  it("describes daily, weekly and monthly recurrences", () => {
    const daily = { ...schedule, id: "sched-daily", recurrence: { kind: "daily", timeOfDay: "09:00" } };
    const weekly = { ...schedule, id: "sched-weekly", recurrence: { kind: "weekly", timeOfDay: "14:30", weekday: "friday" } };
    const monthly = { ...schedule, id: "sched-monthly", recurrence: { kind: "monthly", timeOfDay: "00:00", dayOfMonth: 31 } };
    render(<ControlledSchedulesList schedules={[daily, weekly, monthly]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("daily at 09:00")).toBeInTheDocument();
    expect(screen.getByText("Fridays at 14:30")).toBeInTheDocument();
    expect(screen.getByText("monthly on day 31 at 00:00")).toBeInTheDocument();
  });

  it("shows a Paused chip for a disabled schedule", () => {
    render(<ControlledSchedulesList schedules={[{ ...schedule, enabled: false }]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Paused")).toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No schedules yet.")).toBeInTheDocument();
    // Nothing to search or sort when the list is empty.
    expect(screen.queryByPlaceholderText("Search schedules…")).not.toBeInTheDocument();
  });

  it("filters the list by title or repo", async () => {
    const other = { ...schedule, id: "sched-2", title: "Weekly digest", repo: "acme/other" };
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule, other]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search schedules…"), "digest");

    expect(screen.getByText("Weekly digest")).toBeInTheDocument();
    expect(screen.queryByText("Nightly dependency bump")).not.toBeInTheDocument();
  });

  it("shows a message when a search matches nothing", async () => {
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search schedules…"), "nope");

    expect(screen.getByText("No schedules match your search.")).toBeInTheDocument();
  });

  it("opens a blank overlay from the + button and submits a new schedule, defaulting to every 24 hours", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    expect(screen.getByRole("heading", { name: "New schedule" })).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Title/), "Nightly dependency bump");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", expect.objectContaining({ method: "POST" }));
    expect(lastBody()).toEqual({
      templateId: "",
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      title: "Nightly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      reads: [],
      capabilities: [],
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New schedule" })).not.toBeInTheDocument();
  });

  it("submits a daily recurrence with the chosen time", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={noop} />);

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

  // grain/task-241: the read-only repos box is a picker over the repos
  // this deployment already knows about, and what a schedule carries
  // shows as chips -- typing a repo the list has never seen still works,
  // which the test below this one covers.
  it("shows a schedule's read-only repos as chips and picks more from the known repos", async () => {
    api.mockResolvedValueOnce({});
    const withReads = { ...schedule, reads: ["owner/schema"] };
    const config = { targetRepos: ["acme/widgets", "owner/shared-lib"] };
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[withReads]} config={config} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    expect(screen.getByTitle("Remove owner/schema")).toBeInTheDocument();

    await user.click(screen.getByLabelText(/Read-only repos/));
    // By role, not by text: the target repo's own dropdown is offering
    // the same repos as <option>s a few fields up.
    await user.click(await screen.findByRole("menuitem", { name: "owner/shared-lib" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(lastBody().reads).toEqual(["owner/schema", "owner/shared-lib"]);
  });

  // The typed-not-picked path: this box was a plain text field before it
  // was a picker, so a repo typed into it and left there still has to
  // reach the schedule rather than being dropped at submit.
  it("takes a read-only repo typed straight into the box, and includes checked capabilities", async () => {
    api.mockResolvedValueOnce({});
    const config = { capabilities: [{ id: "gemini-key", name: "Gemini key" }, { id: "self-debug", name: "Self debug" }] };
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} config={config} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.type(screen.getByLabelText(/Title/), "Ship the other thing");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Read-only repos/), "owner/shared-lib");
    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Gemini key" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.reads).toEqual(["owner/shared-lib"]);
    expect(payload.capabilities).toEqual(["gemini-key"]);
  });

  // grain/task-94: opening a schedule fills the pane beside the sidebar,
  // the same as opening a task, a template or a suite.
  it("opens a schedule as a full pane", async () => {
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane .pane-form")).toBeInTheDocument();
  });

  it("opens a row's overlay pre-filled and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));
    expect(screen.getByRole("heading", { name: "Edit schedule" })).toBeInTheDocument();

    const titleField = screen.getByLabelText(/Title/);
    expect(titleField).toHaveValue("Nightly dependency bump");

    await user.clear(titleField);
    await user.type(titleField, "Weekly dependency bump");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/schedules/sched-1", expect.objectContaining({ method: "PATCH" }));
    expect(lastBody()).toEqual({
      templateId: "",
      recurrence: { kind: "everyNHours", everyNHours: 24 },
      title: "Weekly dependency bump",
      description: "",
      repo: "acme/widgets",
      base: "",
      autoMerge: false,
      reads: [],
      capabilities: [],
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Edit schedule" })).not.toBeInTheDocument();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={noop} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[{ ...schedule, enabled: false }]} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Nightly dependency bump"));

    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
  });

  it("deletes a schedule from its overlay after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[schedule]} tasks={[]} onRefresh={onRefresh} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[]} config={config} tasks={[]} onRefresh={onRefresh} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[]} templates={templates} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.click(screen.getByLabelText("Template"));
    await user.click(await screen.findByRole("option", { name: "Dependency bump" }));

    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();
    // Repo and base are not among an unbound template's own content, so
    // they still render (the bound case is the next test).
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

  // grain/task-285: a template bound to a repo already decides where its
  // firings go, so the form stops asking for a repo and branch and says
  // where this schedule will fire instead.
  it("replaces repo/base with the binding when the chosen template is bound", async () => {
    api.mockResolvedValueOnce({});
    const templates = [{ id: "template-1", name: "Dependency bump", repo: "acme/widgets", base: "release" }];
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} templates={templates} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.click(screen.getByLabelText("Template"));
    await user.click(await screen.findByRole("option", { name: "Dependency bump" }));

    expect(screen.queryByLabelText(/Target repo/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Base branch/)).not.toBeInTheDocument();
    expect(screen.getByText(/Fires against acme\/widgets on release/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.templateId).toBe("template-1");
    // Sent empty rather than guessed at here: the API fills both in from
    // the binding (ui.CreateSchedule).
    expect(payload.repo).toBe("");
    expect(payload.base).toBe("");
  });

  it("pre-fills the template select when editing a template-backed schedule, and can detach from it", async () => {
    api.mockResolvedValueOnce({});
    const templates = [{ id: "template-1", name: "Dependency bump" }];
    const templateBacked = { ...schedule, templateId: "template-1", templateName: "Dependency bump" };
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[templateBacked]} templates={templates} tasks={[]} onRefresh={noop} showError={noop} />);

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
    render(<ControlledSchedulesList schedules={[]} tasks={[]} onRefresh={noop} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.type(screen.getByLabelText(/Title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "everyNHours must be positive" }));
    expect(screen.getByRole("heading", { name: "New schedule" })).toBeInTheDocument();
  });

  // --- schedules that run a suite ---------------------------------

  const suites = [{ id: "suite-1", name: "Bug sweep" }, { id: "suite-2", name: "Dependency sweep" }];
  const suiteBacked = {
    ...schedule, id: "sched-suite", title: "Bug sweep", base: "main",
    suiteId: "suite-1", suiteName: "Bug sweep",
  };

  it("shows which suite a suite-backed schedule runs", () => {
    render(<ControlledSchedulesList schedules={[suiteBacked]} suites={suites} tasks={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Suite: Bug sweep")).toBeInTheDocument();
  });

  it("creates a schedule that runs a suite", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} suites={suites} tasks={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.click(screen.getByLabelText("Fires"));
    await user.click(await screen.findByRole("option", { name: "A suite" }));
    await user.click(screen.getByLabelText("Suite"));
    await user.click(await screen.findByRole("option", { name: "Bug sweep" }));

    // The suite decides all of this schedule's content, so none of the
    // task fields (nor the template picker) are on offer.
    expect(screen.queryByLabelText(/^Title/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Template")).not.toBeInTheDocument();

    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Base branch/), "main");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).toHaveBeenCalledWith("/api/schedules", {
      method: "POST",
      body: JSON.stringify({
        suiteId: "suite-1",
        recurrence: { kind: "everyNHours", everyNHours: 24 },
        repo: "acme/widgets",
        base: "main",
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("refuses to submit a suite-backed schedule with no suite chosen", async () => {
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[]} suites={suites} tasks={[]} onRefresh={noop} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "+ New schedule" }));
    await user.click(screen.getByLabelText("Fires"));
    await user.click(await screen.findByRole("option", { name: "A suite" }));
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.type(screen.getByLabelText(/Base branch/), "main");
    await user.click(screen.getByRole("button", { name: "Add schedule" }));

    expect(api).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "choose a suite for this schedule to run" }));
  });

  // What a schedule fires is fixed when it is created (ui.
  // UpdateScheduleRequest's own doc comment), so editing one offers no
  // "Fires" picker -- only which suite it runs.
  it("repoints an existing suite-backed schedule at another suite", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<ControlledSchedulesList schedules={[suiteBacked]} suites={suites} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Bug sweep"));
    expect(screen.queryByLabelText("Fires")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Suite")).toHaveTextContent("Bug sweep");

    await user.click(screen.getByLabelText("Suite"));
    await user.click(await screen.findByRole("option", { name: "Dependency sweep" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.suiteId).toBe("suite-2");
    expect(payload.templateId).toBeUndefined();
    expect(payload.title).toBeUndefined();
  });
});
