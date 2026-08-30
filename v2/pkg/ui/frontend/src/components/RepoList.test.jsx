import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import RepoList from "./RepoList.jsx";

const tasks = [
  { id: "1", title: "Fix the widget", repo: "acme/widgets", state: "queued", blocked: false, capabilities: [] },
  { id: "2", title: "Ship the widget", repo: "acme/widgets", state: "running", blocked: false, capabilities: [] },
  { id: "3", title: "Recall the widget", repo: "acme/widgets", state: "queued", blocked: true, blockedBy: ["1"], capabilities: [] },
  { id: "4", title: "Ship the gadget", repo: "acme/gadgets", state: "completed", blocked: false, capabilities: [] },
  { id: "5", title: "Untargeted proposal", repo: "", state: "proposed", blocked: false, capabilities: [] },
];

function renderList(overrides = {}) {
  const props = {
    tasks,
    config: null,
    onOpenRepo: vi.fn(),
    onOpenReleases: vi.fn(),
    onOpenTask: vi.fn(),
    onNewTask: vi.fn(),
    ...overrides,
  };
  render(<RepoList {...props} />);
  return props;
}

describe("RepoList", () => {
  it("lists one row per repo with per-state counts and a total", () => {
    renderList();

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();
    expect(screen.getByText("Queued 2")).toBeInTheDocument();
    expect(screen.getByText("Running 1")).toBeInTheDocument();
    expect(screen.getByText("Blocked 1")).toBeInTheDocument();
    expect(screen.getByText("3 tasks")).toBeInTheDocument();
    expect(screen.getByText("1 task")).toBeInTheDocument();
  });

  it("omits tasks with no target repo", () => {
    renderList();
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("calls onOpenRepo when a row is clicked", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo });

    await user.click(screen.getByText("acme/gadgets"));

    expect(onOpenRepo).toHaveBeenCalledWith("acme/gadgets");
  });

  it("calls onOpenReleases, not onOpenRepo, when a row's Releases button is clicked", async () => {
    const onOpenRepo = vi.fn();
    const onOpenReleases = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo, onOpenReleases });

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Releases" }));

    expect(onOpenReleases).toHaveBeenCalledWith("acme/gadgets");
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("shows an empty message when there are no repo-targeted tasks", () => {
    renderList({ tasks: [] });
    expect(screen.getByText("No repos yet -- tasks with a target repo will show up here.")).toBeInTheDocument();
  });

  it("folds a repo's tasks out of view until the chevron is clicked, without opening the repo", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo });

    expect(screen.queryByText("Fix the widget")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/widgets" }));

    expect(screen.getByText("Fix the widget")).toBeInTheDocument();
    expect(screen.getByText("Ship the widget")).toBeInTheDocument();
    expect(screen.getByText("Recall the widget")).toBeInTheDocument();
    expect(screen.queryByText("Ship the gadget")).not.toBeInTheDocument();
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("hides a repo's tasks again when its chevron is toggled a second time", async () => {
    const user = userEvent.setup();
    renderList();

    const toggle = () => screen.getByRole("button", { name: /tasks for acme\/widgets/ });
    await user.click(toggle());
    expect(screen.getByText("Fix the widget")).toBeInTheDocument();

    await user.click(toggle());
    expect(screen.queryByText("Fix the widget")).not.toBeInTheDocument();
  });

  it("opens a task from a folded-out sublist", async () => {
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenTask });

    await user.click(screen.getByRole("button", { name: "Show tasks for acme/widgets" }));
    await user.click(screen.getByText("Fix the widget"));

    expect(onOpenTask).toHaveBeenCalledWith("1");
  });

  it("calls onNewTask with the repo, not onOpenRepo, when a row's + button is clicked", async () => {
    const onOpenRepo = vi.fn();
    const onNewTask = vi.fn();
    const user = userEvent.setup();
    renderList({ onOpenRepo, onNewTask });

    await user.click(screen.getByRole("button", { name: "New task under acme/gadgets" }));

    expect(onNewTask).toHaveBeenCalledWith("acme/gadgets");
    expect(onOpenRepo).not.toHaveBeenCalled();
  });
});
