import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SuitesList from "./SuitesList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

// The suites page (bwsalmon/agents#642) landed with no tests of its
// own, unlike every other list page here -- SchedulesList.test.jsx's own
// shape applied to it: the two lists it renders, the create/edit/delete
// round trips through SuiteOverlay, and starting a run through
// SuiteRunOverlay.

const templates = [
  { id: "template-smoke", name: "Smoke", title: "Run the smoke suite" },
  { id: "template-lint", name: "Lint", title: "Run the linter" },
];

const suite = {
  id: "suite-1",
  name: "Nightly sweep",
  items: [{ templateId: "template-smoke", templateName: "Smoke" }],
  mode: "until_clean",
  maxPasses: 5,
  requireApproval: false,
  autoMerge: true,
  createdAt: "2026-01-01T00:00:00Z",
};

const run = {
  id: 7,
  suiteId: "suite-1",
  suiteName: "Nightly sweep",
  repo: "acme/widgets",
  base: "release-1",
  mode: "until_clean",
  maxPasses: 5,
  requireApproval: false,
  autoMerge: true,
  status: "active",
  pass: 2,
  createdAt: "2026-01-01T00:00:00Z",
  tasks: [],
};

const noop = () => {};

// The runs list echoes its suite's own name, so the name appears twice on
// a page showing both. The suites list renders first, so its row is the
// first match -- clicking that is what opens the edit overlay.
const suiteRow = (name) => screen.getAllByText(name)[0];

// Which suite is open is App.jsx's state now, so that the URL can name
// it (/suites/:id, grain/task-139) -- this wrapper stands in for App
// with the smallest thing that holds that one piece of state, so
// clicking a row still opens the pane the way it does in the app.
function ControlledSuitesList(props) {
  const [openSuiteId, setOpenSuiteId] = useState(null);
  return <SuitesList openSuiteId={openSuiteId} onOpenSuite={setOpenSuiteId} {...props} />;
}

function renderList(props = {}) {
  return render(
    <ControlledSuitesList
      suites={[suite]}
      suiteRuns={[run]}
      templates={templates}
      tasks={[]}
      onRefresh={noop}
      onRefreshRuns={noop}
      showError={noop}
      {...props}
    />,
  );
}

describe("SuitesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists suites with their mode and template count", () => {
    renderList();

    expect(suiteRow("Nightly sweep")).toBeInTheDocument();
    expect(screen.getByText("run until clean (max 5)")).toBeInTheDocument();
    expect(screen.getByText("1 template")).toBeInTheDocument();
    expect(screen.queryByText("Requires approval")).not.toBeInTheDocument();
  });

  it("pluralises the template count and flags a suite that requires approval", () => {
    renderList({
      suites: [{
        ...suite,
        items: [{ templateId: "template-smoke" }, { templateId: "template-lint" }],
        mode: "count",
        count: 3,
        requireApproval: true,
      }],
    });

    expect(screen.getByText("2 templates")).toBeInTheDocument();
    expect(screen.getByText("run 3×")).toBeInTheDocument();
    expect(screen.getByText("Requires approval")).toBeInTheDocument();
  });

  it("lists runs with the repo, branch, status and pass they are on", () => {
    renderList();

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("release-1")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText(/pass 2/)).toBeInTheDocument();
  });

  it("shows a failed run's own error", () => {
    renderList({ suiteRuns: [{ ...run, status: "failed", error: "pass 1 had a task that failed" }] });

    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("pass 1 had a task that failed")).toBeInTheDocument();
  });

  it("shows empty messages for both lists when there is nothing yet", () => {
    renderList({ suites: [], suiteRuns: [] });

    expect(screen.getByText(/No suites yet/)).toBeInTheDocument();
    expect(screen.getByText("No suite runs yet.")).toBeInTheDocument();
  });

  it("creates a suite defaulting to until_clean, auto-merge on and approval off", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    renderList({ suites: [], suiteRuns: [], onRefresh });

    await user.click(screen.getByRole("button", { name: "+ New suite" }));
    expect(screen.getByRole("heading", { name: "New suite" })).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Name/), "Nightly sweep");
    await user.click(screen.getByLabelText("Templates"));
    await user.click(await screen.findByRole("option", { name: /Smoke/ }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Add suite" }));

    expect(api).toHaveBeenCalledWith("/api/suites", {
      method: "POST",
      body: JSON.stringify({
        name: "Nightly sweep",
        templateIds: ["template-smoke"],
        mode: "until_clean",
        maxPasses: 5,
        requireApproval: false,
        autoMerge: true,
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New suite" })).not.toBeInTheDocument();
  });

  it("creates a template inline from the suite overlay and preselects it", async () => {
    const createdTemplate = { id: "template-new", name: "New template" };
    api.mockResolvedValueOnce(createdTemplate); // POST /api/templates
    api.mockResolvedValueOnce({}); // POST /api/suites
    const onRefreshTemplates = vi.fn().mockResolvedValue();
    const user = userEvent.setup();
    renderList({ suites: [], suiteRuns: [], onRefreshTemplates });

    await user.click(screen.getByRole("button", { name: "+ New suite" }));
    await user.type(screen.getByLabelText(/Name/), "Nightly sweep");
    await user.click(screen.getByRole("button", { name: "+ New template" }));

    const templateDialog = screen.getAllByRole("dialog").at(-1);
    expect(within(templateDialog).getByRole("heading", { name: "New template" })).toBeInTheDocument();

    await user.type(within(templateDialog).getByLabelText(/Name/), "New template");
    await user.type(within(templateDialog).getByLabelText(/Task title/), "Bump dependencies");
    await user.click(within(templateDialog).getByRole("button", { name: "Add template" }));

    expect(api).toHaveBeenCalledWith("/api/templates", {
      method: "POST",
      body: JSON.stringify({
        name: "New template",
        title: "Bump dependencies",
        description: "",
        repo: "",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefreshTemplates).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New template" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Add suite" }));

    expect(api).toHaveBeenLastCalledWith("/api/suites", {
      method: "POST",
      body: JSON.stringify({
        name: "Nightly sweep",
        templateIds: ["template-new"],
        mode: "until_clean",
        maxPasses: 5,
        requireApproval: false,
        autoMerge: true,
      }),
    });
  });

  it("sends count rather than maxPasses once the mode is a fixed number of runs", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderList({ suites: [], suiteRuns: [] });

    await user.click(screen.getByRole("button", { name: "+ New suite" }));
    await user.type(screen.getByLabelText(/Name/), "Three times");
    await user.click(screen.getByLabelText("Templates"));
    await user.click(await screen.findByRole("option", { name: /Lint/ }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByLabelText("Run mode"));
    await user.click(await screen.findByRole("option", { name: "Run a fixed number of times" }));
    const countField = screen.getByLabelText(/Number of times/);
    await user.clear(countField);
    await user.type(countField, "3");
    await user.click(screen.getByRole("button", { name: "Add suite" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.mode).toBe("count");
    expect(payload.count).toBe(3);
    expect(payload).not.toHaveProperty("maxPasses");
  });

  // grain/task-94: opening a suite fills the pane beside the sidebar,
  // the same as opening a task, a schedule or a template.
  it("opens a suite as a full pane", async () => {
    const user = userEvent.setup();
    renderList({});

    await user.click(suiteRow("Nightly sweep"));

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane .pane-form")).toBeInTheDocument();
  });

  it("opens a row's overlay pre-filled and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    renderList({ onRefresh });

    await user.click(suiteRow("Nightly sweep"));
    expect(screen.getByRole("heading", { name: "Edit suite" })).toBeInTheDocument();

    const nameField = screen.getByLabelText(/Name/);
    expect(nameField).toHaveValue("Nightly sweep");
    expect(screen.getByLabelText("Templates")).toHaveTextContent("Smoke");

    await user.clear(nameField);
    await user.type(nameField, "Weekly sweep");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/suites/suite-1", {
      method: "PATCH",
      body: JSON.stringify({
        name: "Weekly sweep",
        templateIds: ["template-smoke"],
        mode: "until_clean",
        maxPasses: 5,
        requireApproval: false,
        autoMerge: true,
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("deletes a suite from its overlay after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    renderList({ onRefresh });

    await user.click(suiteRow("Nightly sweep"));
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/suites/suite-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("leaves the suite alone when the delete confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const user = userEvent.setup();
    renderList();

    await user.click(suiteRow("Nightly sweep"));
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("starts a run from a suite's Run action, against the repo and branch given", async () => {
    api.mockResolvedValueOnce({});
    const onRefreshRuns = vi.fn();
    const user = userEvent.setup();
    renderList({ onRefreshRuns });

    await user.click(screen.getByRole("button", { name: "Run…" }));
    expect(screen.getByRole("heading", { name: "Run a suite" })).toBeInTheDocument();
    // The clicked suite comes preselected, so the common case is repo,
    // branch, go.
    expect(screen.getByLabelText("Suite")).toHaveTextContent("Nightly sweep");

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.type(screen.getByLabelText(/Branch/), "release-1");
    await user.click(screen.getByRole("button", { name: "Run" }));

    expect(api).toHaveBeenCalledWith("/api/suite-runs", {
      method: "POST",
      body: JSON.stringify({ suiteId: "suite-1", repo: "acme/widgets", base: "release-1" }),
    });
    expect(onRefreshRuns).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Run a suite" })).not.toBeInTheDocument();
  });

  it("does not open the edit overlay when the Run action is clicked", async () => {
    const user = userEvent.setup();
    renderList();

    await user.click(screen.getByRole("button", { name: "Run…" }));

    expect(screen.queryByRole("heading", { name: "Edit suite" })).not.toBeInTheDocument();
  });

  it("reports the error and leaves the run overlay open when starting one fails", async () => {
    api.mockRejectedValueOnce(new Error("base is required"));
    const showError = vi.fn();
    const user = userEvent.setup();
    renderList({ showError });

    await user.click(screen.getByRole("button", { name: "Run…" }));
    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.type(screen.getByLabelText(/Branch/), "release-1");
    await user.click(screen.getByRole("button", { name: "Run" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "base is required" }));
    expect(screen.getByRole("heading", { name: "Run a suite" })).toBeInTheDocument();
  });
});
