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
        sandboxDiskGb: 0,
        agentFramework: "",
        promptExtension: "",
        capabilities: [],
        dependsOn: [],
        reads: [],
        approved: false,
        interactive: false,
        atFront: false,
        attachments: [],
      }),
    });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onCreated).toHaveBeenCalledTimes(1);
  });

  // bwsalmon/agents#612: both checkboxes are seeded from the
  // deployment's own defaults (GET /api/config), which are on unless an
  // operator has turned one off -- so the ordinary task files queued and
  // set to auto-merge without anyone touching either box.
  it("seeds Queue immediately and Auto-merge from the deployment's defaults", async () => {
    const config = { approvedByDefault: true, autoMergeByDefault: true };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    expect(screen.getByLabelText(/Queue immediately/)).toBeChecked();
    expect(screen.getByLabelText(/Auto-merge once checks pass/)).toBeChecked();

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.approved).toBe(true);
    expect(payload.autoMerge).toBe(true);
  });

  // grain/task-202: which end of the backlog a new task joins is a
  // choice on this form rather than only a setting buried in Settings,
  // and the form opens on the end the last task added went to --
  // config.newestFirst, which the server rewrites on every filing that
  // states one (ui.CreateTaskRequest.AtFront).
  it("seeds the backlog-end picker from what the last task added chose", async () => {
    const user = userEvent.setup();
    render(<NewTaskOverlay config={{ newestFirst: true }} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    expect(screen.getByLabelText("Add to backlog")).toHaveTextContent(/^Front/);

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).atFront).toBe(true);
  });

  it("files at the other end of the backlog when that end is picked", async () => {
    const user = userEvent.setup();
    render(<NewTaskOverlay config={{ newestFirst: true }} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByLabelText("Add to backlog"));
    await user.click(screen.getByRole("option", { name: /^End/ }));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).atFront).toBe(false);
  });

  // An interactive session dispatches ahead of the backlog whatever the
  // picker says, so there is no end to pick and nothing to remember: the
  // field is left out of the payload rather than sent as a choice
  // nobody made (ui.CreateTaskRequest.AtFront).
  it("leaves the backlog end out of an interactive task, which always jumps the queue", async () => {
    api.mockResolvedValueOnce({ id: "42" });
    const user = userEvent.setup();
    render(
      <NewTaskOverlay
        config={{ newestFirst: false }}
        onClose={() => {}} onCreated={() => Promise.resolve()} onOpenTask={() => {}} showError={() => {}}
      />
    );

    await user.type(screen.getByLabelText(/Title/), "Talk this through");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    await user.click(screen.getByLabelText(/Interactive session/));
    expect(screen.queryByLabelText("Add to backlog")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body)).not.toHaveProperty("atFront");
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
    await user.type(screen.getByLabelText(/Disk \(GiB\)/), "40");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(api).toHaveBeenCalledWith("/api/tasks", expect.objectContaining({
      body: expect.stringContaining('"sandboxCpus":4,"sandboxMemoryMb":8192,"sandboxDiskGb":40'),
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
    await user.upload(document.querySelector('input[type="file"]'), file);
    expect(await screen.findByText("screenshot.png")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(fileToAttachment).toHaveBeenCalledWith(file);
    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.attachments).toEqual([upload]);
  });

  // grain/task-241: read-only repos are picked the way dependencies are,
  // over the same repo list the target-repo dropdown offers -- a repo
  // this deployment already works with is a click rather than something
  // to spell out from memory.
  it("picks read-only repos from the repos this deployment already knows", async () => {
    const config = { targetRepos: ["acme/widgets", "acme/shared-lib"] };
    const tasks = [{ id: "12", title: "Fix the login bug", repo: "other/tooling" }];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship the other thing");
    await user.click(screen.getByLabelText(/No repo/));

    const readsInput = screen.getByLabelText(/Read-only repos/);
    await user.click(readsInput);
    await user.click(await screen.findByRole("menuitem", { name: "acme/shared-lib" }));
    await user.click(readsInput);
    await user.click(await screen.findByRole("menuitem", { name: "other/tooling" }));
    // A repo neither the deployment nor any task has named yet still has
    // to be reachable, the gap RepoField's own "Other…" covers.
    await user.type(readsInput, "someone/brand-new");
    await user.click(await screen.findByRole("menuitem", { name: "Add someone/brand-new" }));

    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.reads).toEqual(["acme/shared-lib", "other/tooling", "someone/brand-new"]);
  });

  it("adds depends-on tasks via the picker and includes checked capabilities", async () => {
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

    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Web search" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.dependsOn).toEqual(["12", "15"]);
    expect(payload.capabilities).toEqual(["web-search"]);
  });

  // The picker used to offer every capability grain ships with no sign
  // of whether this deployment could honour it, so ticking gemini-key on
  // a deployment with no GCP project filed a task whose only symptom was
  // failing to dispatch later. The row now says so where it is ticked.
  it("warns in the picker about a capability this deployment is not configured for", async () => {
    const config = {
      capabilities: [
        { id: "gemini-key", name: "Gemini key", ready: false, needs: ["GCP project"] },
        { id: "self-debug", name: "Self debug", ready: true },
      ],
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={[]} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.click(screen.getByLabelText("Capabilities"));
    const gemini = await screen.findByRole("option", { name: /Gemini key/ });
    expect(gemini).toHaveTextContent("GCP project");
    expect(gemini).toHaveTextContent(/fail to dispatch/);
    // A ready capability gets no such line.
    expect(screen.getByRole("option", { name: /Self debug/ })).not.toHaveTextContent(/Not ready/);

    // Warning, never refusing: it is still tickable, since filing the
    // task first and setting the project second is an ordinary order.
    await user.click(gemini);
    await user.keyboard("{Escape}");
    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));
    expect(JSON.parse(api.mock.calls[0][1].body).capabilities).toEqual(["gemini-key"]);
  });

  // grain/task-14: the deployment's own default capabilities arrive
  // on GET /api/config and open the form already ticked, so filing a
  // task on a deployment that defaults gcp-key gets one without anyone
  // remembering to ask -- and the payload names them explicitly rather
  // than relying on the server to fill them in, which is what makes
  // unticking one work.
  it("seeds the capability picker from the deployment's defaults", async () => {
    const config = {
      capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }],
      defaultCapabilities: ["gcp-key"],
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Needs a key");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.capabilities).toEqual(["gcp-key"]);
  });

  it("files a task without a defaulted capability once it is unticked", async () => {
    const config = {
      capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }],
      defaultCapabilities: ["gcp-key"],
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "No key needed");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "GCP key" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.capabilities).toEqual([]);
  });

  it("shows a picked dependency's whole task on hover", async () => {
    const tasks = [{ id: "12", title: "Fix the login bug" }];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={null} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByPlaceholderText("Search tasks to depend on…"), "12");
    await user.click(await screen.findByText("Fix the login bug"));

    await user.hover(screen.getByText("12 Fix the login bug"));
    expect(await screen.findByRole("tooltip")).toHaveTextContent("12 Fix the login bug");
  });

  // grain/task-24: a repo can default capabilities of its own on top of
  // the deployment's, so picking a repo re-seeds the picker with the
  // union -- the same answer CreateTask would resolve server-side for a
  // task filed against it.
  it("adds the picked repo's own default capabilities to the deployment's", async () => {
    const config = {
      capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }],
      targetRepos: ["acme/widgets", "acme/gadgets"],
      defaultCapabilities: ["gemini-key"],
      repoDefaultCapabilities: { "acme/widgets": ["gcp-key"] },
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Needs a key");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).capabilities).toEqual(["gemini-key", "gcp-key"]);
  });

  // Switching away from a repo that adds one drops it again: leaving the
  // previous repo's extras ticked would file the task with capabilities
  // the repo it actually targets never asked for.
  it("re-seeds the picker when the repo changes", async () => {
    const config = {
      capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }],
      targetRepos: ["acme/widgets", "acme/gadgets"],
      defaultCapabilities: ["gemini-key"],
      repoDefaultCapabilities: { "acme/widgets": ["gcp-key"] },
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Elsewhere");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).capabilities).toEqual(["gemini-key"]);
  });

  // Once the picker has been touched the ticks are the human's own: a
  // re-seed that put back a capability they had just unticked would file
  // a task with something they had already said no to.
  it("leaves a hand-edited capability picker alone when the repo changes", async () => {
    const config = {
      capabilities: [{ id: "gcp-key", name: "GCP key" }, { id: "gemini-key", name: "Gemini key" }],
      targetRepos: ["acme/widgets", "acme/gadgets"],
      defaultCapabilities: ["gemini-key"],
      repoDefaultCapabilities: { "acme/widgets": ["gcp-key"] },
    };
    const user = userEvent.setup();
    render(<NewTaskOverlay config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "My own choice");
    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Gemini key" }));
    await user.keyboard("{Escape}");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).capabilities).toEqual([]);
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

  // The failure this guards against is self-perpetuating: a base branch
  // that merges and is deleted makes its task fail, that failed task is
  // then the repo's most recent one carrying that base, so the dead
  // branch is suggested again, and the next task fails the same way.
  it("does not prefill Base branch from a task that failed", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [
      { id: "1", title: "Good task", repo: "acme/widgets", base: "release/2.0", state: "completed", createdAt: "2026-01-01T00:00:00Z" },
      { id: "2", title: "Died on a deleted base", repo: "acme/widgets", base: "grain/issue-642", state: "failed", createdAt: "2026-06-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("release/2.0");
  });

  it("prefills nothing when every task carrying a base for that repo failed", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [
      { id: "1", title: "Died", repo: "acme/widgets", base: "grain/issue-642", state: "failed", createdAt: "2026-06-01T00:00:00Z" },
      { id: "2", title: "Closed too", repo: "acme/widgets", base: "grain/issue-642", state: "closed", createdAt: "2026-06-02T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("");
  });

  // A system-generated task -- a schedule firing, a suite pass, a
  // stacked fix, an agent's own propose_task -- picks a base for its own
  // reasons, and that choice is not a suggestion for the human filing
  // the next one.
  it("does not prefill Base branch from a task an agent or automation filed", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [
      { id: "1", title: "Filed by hand", repo: "acme/widgets", base: "release/2.0", authorKind: "human", createdAt: "2026-01-01T00:00:00Z" },
      { id: "2", title: "A suite pass", repo: "acme/widgets", base: "suite/run-7", authorKind: "automation", createdAt: "2026-06-01T00:00:00Z" },
      { id: "3", title: "An agent's proposal", repo: "acme/widgets", base: "grain/task-3", authorKind: "agent", createdAt: "2026-07-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("release/2.0");
  });

  // The counterpart to the "no history" case below: a repo whose only
  // tasks are system-generated has nothing to prefill from either, so
  // picking it is the no-history case rather than the "most recent task
  // used the default branch" one, and must not clear the field.
  it("treats a repo whose only tasks are system-generated as having no history", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets", "acme/robots"] };
    const tasks = [
      { id: "1", title: "Filed by hand", repo: "acme/widgets", base: "release/2.0", authorKind: "human", createdAt: "2026-01-01T00:00:00Z" },
      { id: "2", title: "A schedule fired", repo: "acme/robots", base: "nightly", authorKind: "automation", createdAt: "2026-06-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    expect(screen.getByLabelText(/Base branch/)).toHaveValue("release/2.0");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/robots");
    expect(screen.getByLabelText(/Base branch/)).toHaveValue("release/2.0");
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

  it("leaves a manually-typed Base branch alone even when the picked repo has history", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets"] };
    const tasks = [
      { id: "1", title: "Old task", repo: "acme/widgets", base: "release/2.0", createdAt: "2026-01-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.type(screen.getByLabelText(/Base branch/), "my-custom-branch");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("my-custom-branch");
  });

  // A repo's most recent task can itself have deliberately used the
  // deployment default (no base at all) -- that is a real suggestion
  // ("build off the default branch here"), not an absence of one, and
  // should clear out whatever base picking an earlier repo prefilled.
  it("clears a prefilled Base branch when the newly-picked repo's most recent task used the default branch", async () => {
    const config = { capabilities: [], targetRepos: ["acme/widgets", "acme/other"] };
    const tasks = [
      { id: "1", title: "Off a release branch", repo: "acme/widgets", base: "release/2.0", createdAt: "2026-01-01T00:00:00Z" },
      { id: "2", title: "Off the default branch", repo: "acme/other", base: "", createdAt: "2026-06-01T00:00:00Z" },
    ];
    const user = userEvent.setup();
    render(<NewTaskOverlay tasks={tasks} config={config} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />);

    await user.type(screen.getByLabelText(/Title/), "Ship it");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/widgets");
    expect(screen.getByLabelText(/Base branch/)).toHaveValue("release/2.0");
    await user.selectOptions(screen.getByLabelText(/Target repo/), "acme/other");
    expect(screen.getByLabelText(/Base branch/)).toHaveValue("");

    await user.click(screen.getByRole("button", { name: "Create task" }));
    const payload = JSON.parse(api.mock.calls[0][1].body);
    expect(payload.base).toBe("");
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

  // A task can be driven by a framework other than the deployment's own
  // (model.Task.AgentFramework) -- the picker for that sits with the
  // other per-task overrides, behind "Advanced options".
  it("files a task with its own agent framework when one is picked", async () => {
    api.mockResolvedValueOnce({ id: "42" });
    const user = userEvent.setup();
    render(
      <NewTaskOverlay config={{ agentFramework: "gemini" }} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />
    );

    await user.type(screen.getByLabelText(/Title/), "Port the parser");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    await user.click(screen.getByLabelText("Agent framework"));
    await user.click(screen.getByRole("option", { name: "Claude" }));
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).agentFramework).toBe("claude");
  });

  // Leaving the picker alone must send "" and not the deployment's
  // current framework: an empty override follows the deployment if its
  // default changes later, a pinned one does not.
  it("names the deployment default in the picker without pinning the task to it", async () => {
    const user = userEvent.setup();
    render(
      <NewTaskOverlay config={{ agentFramework: "claude" }} onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}} />
    );

    await user.type(screen.getByLabelText(/Title/), "Fix the thing");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    expect(screen.getByText("Deployment default (Claude)")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).agentFramework).toBe("");
  });

  // grain/task-114: a task can replace the standing instructions its
  // deployment and repo would otherwise give its runs -- the last of the
  // per-task overrides behind "Advanced options", and the only one that
  // replaces rather than adds, which is why the deployment's own text is
  // shown beside it.
  it("files a task with its own prompt extension when one is written", async () => {
    api.mockResolvedValueOnce({ id: "42" });
    const user = userEvent.setup();
    render(
      <NewTaskOverlay
        config={{ promptExtension: "Run `make lint` before you push." }}
        onClose={() => {}} onCreated={() => Promise.resolve()} showError={() => {}}
      />
    );

    await user.type(screen.getByLabelText(/Title/), "Regenerate the client");
    await user.click(screen.getByLabelText(/No repo/));
    await user.click(screen.getByRole("button", { name: "Advanced options" }));
    expect(screen.getByText(/Deployment-wide, used when the box above is empty/))
      .toHaveTextContent("Run `make lint` before you push.");
    await user.type(screen.getByLabelText(/Prompt extension override/), "Ignore the house rules.");
    await user.click(screen.getByRole("button", { name: "Create task" }));

    expect(JSON.parse(api.mock.calls[0][1].body).promptExtension).toBe("Ignore the house rules.");
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
