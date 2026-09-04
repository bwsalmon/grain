import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import TemplatesList from "./TemplatesList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const template = {
  id: "template-1",
  name: "Dependency bump",
  title: "Bump dependencies",
  description: "",
  autoMerge: false,
  reads: [],
  capabilities: [],
  createdAt: "2026-01-01T00:00:00Z",
};

const otherTemplate = {
  id: "template-2",
  name: "Security patch",
  title: "Apply security patches",
  description: "",
  autoMerge: false,
  reads: [],
  capabilities: [],
  createdAt: "2026-02-01T00:00:00Z",
};

const noop = () => {};

// Which template is open is App.jsx's state now, so that the URL can
// name it (/templates/:id, grain/task-139) -- SchedulesList.test.jsx's
// own wrapper standing in for App with the smallest thing that holds
// that one piece of state.
function ControlledTemplatesList(props) {
  const [openTemplateId, setOpenTemplateId] = useState(null);
  return <TemplatesList openTemplateId={openTemplateId} onOpenTemplate={setOpenTemplateId} {...props} />;
}

describe("TemplatesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the templates it is given, showing just their key details", () => {
    render(<ControlledTemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Dependency bump")).toBeInTheDocument();
    expect(screen.getByText("Bump dependencies")).toBeInTheDocument();
    // No form fields on the main list any more -- editing lives behind
    // clicking a row instead.
    expect(screen.queryByLabelText(/Name/)).not.toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<ControlledTemplatesList templates={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No templates.")).toBeInTheDocument();
    // Nothing to search or sort when the list is empty.
    expect(screen.queryByPlaceholderText("Search templates…")).not.toBeInTheDocument();
  });

  it("filters the list by name or title", async () => {
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template, otherTemplate]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search templates…"), "security");

    expect(screen.getByText("Security patch")).toBeInTheDocument();
    expect(screen.queryByText("Dependency bump")).not.toBeInTheDocument();
  });

  it("shows a message when a search matches nothing", async () => {
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search templates…"), "nope");

    expect(screen.getByText("No templates match your search.")).toBeInTheDocument();
  });

  it("opens a blank overlay from the + button and submits a new template", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New template" }));
    expect(screen.getByRole("heading", { name: "New template" })).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Name/), "Dependency bump");
    await user.type(screen.getByLabelText(/Task title/), "Bump dependencies");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(api).toHaveBeenCalledWith("/api/templates", {
      method: "POST",
      body: JSON.stringify({
        name: "Dependency bump",
        title: "Bump dependencies",
        description: "",
        repo: "",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New template" })).not.toBeInTheDocument();
  });

  // grain/task-94: opening a template fills the pane beside the sidebar,
  // the same as opening a task, a schedule or a suite -- one gesture with
  // one result across all four lists.
  it("opens a template as a full pane", async () => {
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane .pane-form")).toBeInTheDocument();
  });

  it("opens a row's overlay pre-filled and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    expect(screen.getByRole("heading", { name: "Edit template" })).toBeInTheDocument();

    const nameField = screen.getByLabelText(/Name/);
    expect(nameField).toHaveValue("Dependency bump");

    await user.clear(nameField);
    await user.type(nameField, "Dependency bump (patch only)");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/templates/template-1", {
      method: "PATCH",
      body: JSON.stringify({
        name: "Dependency bump (patch only)",
        title: "Bump dependencies",
        description: "",
        repo: "",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Edit template" })).not.toBeInTheDocument();
  });

  // grain/task-241: read-only repos are picked from the repos this
  // deployment already knows about, and the ones a template carries show
  // as chips rather than as a comma-separated string to edit by hand.
  it("shows a template's read-only repos as chips and picks more from the known repos", async () => {
    api.mockResolvedValueOnce({});
    const withReads = { ...template, reads: ["owner/schema"] };
    const config = { targetRepos: ["acme/widgets", "owner/shared-lib"] };
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[withReads]} config={config} tasks={[]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    expect(screen.getByTitle("Remove owner/schema")).toBeInTheDocument();

    await user.click(screen.getByLabelText(/Read-only repos/));
    // By role rather than by text: the target repo dropdown (the
    // optional binding, grain/task-285) offers the same repos as
    // <option>s, so a bare findByText would match two elements.
    await user.click(await screen.findByRole("menuitem", { name: "owner/shared-lib" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(JSON.parse(api.mock.lastCall[1].body).reads).toEqual(["owner/schema", "owner/shared-lib"]);
  });

  // grain/task-285: a template can be bound to one repo (and branch),
  // and the list says which for the templates that are.
  it("chips the repo a bound template is bound to", () => {
    const bound = { ...template, repo: "acme/widgets", base: "release" };
    render(<ControlledTemplatesList templates={[bound, otherTemplate]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("acme/widgets @ release")).toBeInTheDocument();
    // The unbound one carries no chip -- most templates are unbound, and
    // an empty chip would be noise on every row.
    expect(screen.queryByText("Security patch").parentElement.querySelector(".MuiChip-root")).toBeNull();
  });

  it("binds a template to a repo and branch from its overlay", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.type(screen.getByLabelText(/Base branch/), "release");
    await user.click(screen.getByRole("button", { name: "Save" }));

    const sent = JSON.parse(api.mock.lastCall[1].body);
    expect(sent.repo).toBe("acme/widgets");
    expect(sent.base).toBe("release");
  });

  // Clearing the repo is how a template is unbound, which the API reads
  // an empty repo as (ui.UpdateTemplateRequest's own doc comment).
  it("unbinds a template when its repo is cleared", async () => {
    api.mockResolvedValueOnce({});
    const bound = { ...template, repo: "acme/widgets", base: "release" };
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[bound]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    await user.clear(screen.getByDisplayValue("acme/widgets"));
    await user.clear(screen.getByLabelText(/Base branch/));
    await user.click(screen.getByRole("button", { name: "Save" }));

    const sent = JSON.parse(api.mock.lastCall[1].body);
    expect(sent.repo).toBe("");
    expect(sent.base).toBe("");
  });

  it("deletes a template from its overlay after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/templates/template-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  it("reports the error and leaves the overlay open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("unknown capability not-a-real-capability"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<ControlledTemplatesList templates={[]} onRefresh={noop} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "+ New template" }));
    await user.type(screen.getByLabelText(/Name/), "x");
    await user.type(screen.getByLabelText(/Task title/), "x");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "unknown capability not-a-real-capability" }));
    expect(screen.getByRole("heading", { name: "New template" })).toBeInTheDocument();
  });
});
