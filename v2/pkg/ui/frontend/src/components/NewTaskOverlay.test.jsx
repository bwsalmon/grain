import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import NewTaskOverlay from "./NewTaskOverlay.jsx";
import api from "../api.js";
import fileToAttachment from "../attachments.js";

vi.mock("../api.js", () => ({ default: vi.fn().mockResolvedValue(null) }));
// fileToAttachment reads a real File with FileReader -- mocked the same
// way api.js is, so a test that attaches a file exercises the component's
// own wiring without depending on jsdom's own FileReader behaviour.
vi.mock("../attachments.js", () => ({ default: vi.fn() }));

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
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", {
      method: "POST",
      body: JSON.stringify({
        title: "Fix the thing",
        description: "",
        repo: "",
        noRepo: true,
        base: "",
        autoMerge: false,
        sandboxCpus: 0,
        sandboxMemoryMb: 0,
        capabilities: [],
        dependsOn: [],
        reads: [],
        approved: false,
        interactive: false,
        attachments: [],
      }),
    });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onCreated).toHaveBeenCalledTimes(1);
  });

  it("greys out Create task until the title and target repo are both filled", async () => {
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);
    const createButton = screen.getByRole("button", { name: "Create task" });

    expect(createButton).toBeDisabled();
    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    expect(createButton).toBeDisabled();
    await user.click(screen.getByLabelText(/No repo/));
    expect(createButton).toBeEnabled();

    // Unchecking "No repo" reinstates the requirement to pick one.
    await user.click(screen.getByLabelText(/No repo/));
    expect(createButton).toBeDisabled();
  });

  // bwsalmon/agents#534: a per-task sandbox shape override.
  // bwsalmon/agents#613: it now lives behind the "Advanced options" toggle
  // alongside the interactive-session checkbox, so open that first.
  it("includes a sandbox shape override when given", async () => {
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    await user.type(screen.getByLabelText(/vCPUs/), "4");
    await user.type(screen.getByLabelText(/Memory \(MiB\)/), "8192");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", expect.objectContaining({
      body: expect.stringContaining('"sandboxCpus":4,"sandboxMemoryMb":8192'),
    }));
  });

  it("reads and includes a picked file as an attachment", async () => {
    const upload = { filename: "screenshot.png", contentType: "image/png", content: "ZmFrZQ==" };
    fileToAttachment.mockResolvedValueOnce(upload);
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    const file = new File(["fake"], "screenshot.png", { type: "image/png" });
    await user.upload(screen.getByLabelText("Attach files"), file);
    expect(await screen.findByText("screenshot.png")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(fileToAttachment).toHaveBeenCalledWith(file);
    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.attachments).toEqual([upload]);
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
    await user.click(screen.getByLabelText(/No repo/));

    const dependsOnInput = screen.getByPlaceholderText("Search tasks to depend on…");
    await user.type(dependsOnInput, "12");
    await user.click(await screen.findByText("Fix the login bug"));
    await user.type(dependsOnInput, "15");
    await user.click(await screen.findByText("Add dark mode"));

    await user.type(screen.getByLabelText(/Read-only repos/), "owner/shared-lib, owner/schema ");
    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Web search" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.dependsOn).toEqual(["12", "15"]);
    expect(payload.reads).toEqual(["owner/shared-lib", "owner/schema"]);
    expect(payload.capabilities).toEqual(["web-search"]);
  });

  it("offers a repo dropdown built from targetRepos and existing tasks' repos, instead of a bare text field", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [{ id: "1", title: "Old task", repo: "acme/other" }];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/other");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.repo).toBe("acme/other");
  });

  // bwsalmon/agents#641: picking a repo prefills Base branch from
  // whatever base that repo's own most recent task used, so a repo
  // whose work lives off a release branch doesn't make every new task
  // against it retype that branch name by hand.
  it("prefills Base branch from the picked repo's most recent task", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [
      { id: "1", title: "Old task", repo: "acme/widgets", base: "main", createdAt: "2026-01-01T00:00:00Z" },
      { id: "2", title: "Newer task", repo: "acme/widgets", base: "release/2.0", createdAt: "2026-06-01T00:00:00Z" },
      { id: "3", title: "Other repo task", repo: "acme/other", base: "main", createdAt: "2026-08-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("release/2.0");
  });

  it("leaves a manually-typed Base branch alone when the picked repo has no task history", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets", "acme/fresh"] };
    const tasks = [
      { id: "1", title: "Old task", repo: "acme/widgets", base: "release/2.0", createdAt: "2026-01-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.type(screen.getByLabelText(/Base branch/), "my-custom-branch");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/fresh");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("my-custom-branch");
  });

  // bwsalmon/agents#539: an interactive task always queues immediately
  // (no "Queue immediately" checkbox left to check), and its creator is
  // taken straight to its chat rather than back to the task list.
  // bwsalmon/agents#613: the interactive-session checkbox lives behind the
  // "Advanced options" toggle, so open that first.
  it("files an interactive task as approved and opens its chat once created", async () => {
    api.mockResolvedValueOnce({ id: "42" });
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(
      <NewTaskOverlay config={null} onClose={() => {}} onCreated={() => Promise.resolve()} onOpenTask={onOpenTask} showError={() => {}} />
    );

    await user.type(screen.getByLabelText(/Title/), "Talk this through");
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    await user.click(screen.getByLabelText(/Interactive session/));
    await user.click(screen.getByLabelText(/No repo/));
    expect(screen.queryByLabelText(/Queue immediately/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.interactive).toBe(true);
    expect(payload.approved).toBe(true);
    expect(onOpenTask).toHaveBeenCalledWith("42");
  });

  it("reports the error and leaves the overlay open when the request fails", async () => {
    api.mockRejectedValueOnce(new Error("title is required"));
    const showError = vi.fn();
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<NewTaskOverlay config={null} onClose={onClose} onCreated={() => {}} showError={showError} />);

    await user.type(screen.getByLabelText(/Title/), "x");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "title is required" }));
    expect(onClose).not.toHaveBeenCalled();
  });
});
