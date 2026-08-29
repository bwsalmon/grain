import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import Sidebar from "./Sidebar.jsx";

const tasks = [
  { id: 1, state: "queued", blocked: false },
  { id: 2, state: "queued", blocked: true, blockedBy: [3] },
  { id: 3, state: "running", blocked: false },
];

const noop = () => {};
const baseProps = {
  stateFilter: "all",
  onSetView: noop,
  onSetFilter: noop,
  onOpenSecrets: noop,
  onOpenSchedules: noop,
  onOpenSettings: noop,
  onOpenReleases: noop,
  onOpenUpgrade: noop,
  onOpenNewTask: noop,
};

describe("Sidebar", () => {
  it("shows the default target when config has one", () => {
    render(<Sidebar {...baseProps} config={{ defaultTarget: "owner/repo" }} tasks={[]} />);
    expect(screen.getByText("owner/repo")).toBeInTheDocument();
  });

  it("falls back to the acting user when config has no default target", () => {
    render(<Sidebar {...baseProps} config={{ actor: "bwsalmon" }} tasks={[]} />);
    expect(screen.getByText("as bwsalmon")).toBeInTheDocument();
  });

  it("counts tasks per state and lists only states that are present", () => {
    render(<Sidebar {...baseProps} config={null} tasks={tasks} />);

    expect(screen.getByRole("button", { name: /All issues 3/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Queued 2/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Running 1/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Completed/ })).not.toBeInTheDocument();
  });

  it("shows a blocked nav entry only when a task is blocked", () => {
    render(<Sidebar {...baseProps} config={null} tasks={tasks} />);
    expect(screen.getByRole("button", { name: /Blocked 1/ })).toBeInTheDocument();
  });

  it("omits the blocked nav entry when nothing is blocked", () => {
    render(<Sidebar {...baseProps} config={null} tasks={[tasks[0]]} />);
    expect(screen.queryByRole("button", { name: /Blocked/ })).not.toBeInTheDocument();
  });

  it("calls onSetFilter with the clicked state", async () => {
    const onSetFilter = vi.fn();
    const user = userEvent.setup();
    render(<Sidebar {...baseProps} config={null} tasks={tasks} onSetFilter={onSetFilter} />);

    await user.click(screen.getByRole("button", { name: /Running 1/ }));

    expect(onSetFilter).toHaveBeenCalledWith("running");
  });

  it("routes the footer and new-task buttons to their own callbacks", async () => {
    const onOpenSecrets = vi.fn();
    const onOpenSchedules = vi.fn();
    const onOpenSettings = vi.fn();
    const onOpenReleases = vi.fn();
    const onOpenUpgrade = vi.fn();
    const onOpenNewTask = vi.fn();
    const user = userEvent.setup();
    render(
      <Sidebar
        {...baseProps}
        config={null}
        tasks={[]}
        onOpenSecrets={onOpenSecrets}
        onOpenSchedules={onOpenSchedules}
        onOpenSettings={onOpenSettings}
        onOpenReleases={onOpenReleases}
        onOpenUpgrade={onOpenUpgrade}
        onOpenNewTask={onOpenNewTask}
      />,
    );

    await user.click(screen.getByRole("button", { name: "+ New task" }));
    await user.click(screen.getByRole("button", { name: "Secrets" }));
    await user.click(screen.getByRole("button", { name: "Scheduled tasks" }));
    await user.click(screen.getByRole("button", { name: "Settings" }));
    await user.click(screen.getByRole("button", { name: "Releases" }));
    await user.click(screen.getByRole("button", { name: "Upgrade" }));

    expect(onOpenNewTask).toHaveBeenCalledTimes(1);
    expect(onOpenSecrets).toHaveBeenCalledTimes(1);
    expect(onOpenSchedules).toHaveBeenCalledTimes(1);
    expect(onOpenSettings).toHaveBeenCalledTimes(1);
    expect(onOpenReleases).toHaveBeenCalledTimes(1);
    expect(onOpenUpgrade).toHaveBeenCalledTimes(1);
  });
});
