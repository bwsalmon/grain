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
  onOpenSettings: noop,
  onOpenDebug: noop,
  onOpenMetrics: noop,
  onOpenNewTask: noop,
};

describe("Sidebar", () => {
  it("counts tasks per state and lists only states that are present", () => {
    render(<Sidebar {...baseProps} config={null} tasks={tasks} />);

    expect(
      screen.getByRole("button", { name: /All tasks 3/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Queued 2/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Running 1/ }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Queued for merge/ }),
    ).not.toBeInTheDocument();
  });

  // The whole point of making the wait a state rather than a chip: it is
  // countable, so "what is sitting here waiting on me?" has an entry in
  // the rail and a filter behind it.
  it("counts tasks waiting on a Submit click under their own entry", () => {
    render(
      <Sidebar
        {...baseProps}
        config={null}
        tasks={[...tasks, { id: 4, state: "awaiting_submit", blocked: false }]}
      />,
    );
    expect(
      screen.getByRole("button", { name: /Awaiting submit 1/ }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Queued for merge/ }),
    ).not.toBeInTheDocument();
  });

  it("shows a blocked nav entry only when a task is blocked", () => {
    render(<Sidebar {...baseProps} config={null} tasks={tasks} />);
    expect(
      screen.getByRole("button", { name: /Blocked 1/ }),
    ).toBeInTheDocument();
  });

  it("omits the blocked nav entry when nothing is blocked", () => {
    render(<Sidebar {...baseProps} config={null} tasks={[tasks[0]]} />);
    expect(
      screen.queryByRole("button", { name: /Blocked/ }),
    ).not.toBeInTheDocument();
  });

  it("calls onSetFilter with the clicked state", async () => {
    const onSetFilter = vi.fn();
    const user = userEvent.setup();
    render(
      <Sidebar
        {...baseProps}
        config={null}
        tasks={tasks}
        onSetFilter={onSetFilter}
      />,
    );

    await user.click(screen.getByRole("button", { name: /Running 1/ }));

    expect(onSetFilter).toHaveBeenCalledWith("running");
  });

  it("routes the footer and new-task buttons to their own callbacks", async () => {
    const onOpenSettings = vi.fn();
    const onOpenDebug = vi.fn();
    const onOpenMetrics = vi.fn();
    const onOpenNewTask = vi.fn();
    const user = userEvent.setup();
    render(
      <Sidebar
        {...baseProps}
        config={null}
        tasks={[]}
        onOpenSettings={onOpenSettings}
        onOpenDebug={onOpenDebug}
        onOpenMetrics={onOpenMetrics}
        onOpenNewTask={onOpenNewTask}
      />,
    );

    await user.click(screen.getByRole("button", { name: "+ New task" }));
    await user.click(screen.getByRole("button", { name: "Settings" }));
    await user.click(screen.getByRole("button", { name: "Debug" }));
    await user.click(screen.getByRole("button", { name: "Metrics" }));

    expect(onOpenNewTask).toHaveBeenCalledTimes(1);
    expect(onOpenSettings).toHaveBeenCalledTimes(1);
    expect(onOpenDebug).toHaveBeenCalledTimes(1);
    expect(onOpenMetrics).toHaveBeenCalledTimes(1);
  });

  // grain/task-115: Settings, Debug and Metrics open a pane beside this
  // rail rather than a box over the middle of the screen, so the rail is
  // still on screen while one is open and has to say which of the three
  // it is -- the same way the nav entries above mark the current view.
  it("marks the footer entry whose pane is open", () => {
    const footer = ["Settings", "Debug", "Metrics"];
    const expectOnly = (open) => {
      for (const name of footer) {
        const button = screen.getByRole("button", { name });
        if (name === open) {
          expect(button).toHaveClass("Mui-selected");
        } else {
          expect(button).not.toHaveClass("Mui-selected");
        }
      }
    };
    const { rerender } = render(
      <Sidebar {...baseProps} config={null} tasks={[]} />,
    );

    expectOnly(null);

    rerender(<Sidebar {...baseProps} config={null} tasks={[]} showSettings />);
    expectOnly("Settings");

    rerender(<Sidebar {...baseProps} config={null} tasks={[]} showDebug />);
    expectOnly("Debug");

    rerender(<Sidebar {...baseProps} config={null} tasks={[]} showMetrics />);
    expectOnly("Metrics");
  });

  it("switches to the schedules view and shows its count when clicked", async () => {
    const onSetView = vi.fn();
    const schedules = [{ id: "sched-1" }, { id: "sched-2" }];
    const user = userEvent.setup();
    render(
      <Sidebar
        {...baseProps}
        config={null}
        tasks={[]}
        schedules={schedules}
        onSetView={onSetView}
      />,
    );

    const button = screen.getByRole("button", { name: /Schedules 2/ });
    await user.click(button);

    expect(onSetView).toHaveBeenCalledWith("schedules");
  });

  // The four list entries used to be four identical invisible dots, so
  // the label was the only thing telling them apart. Each carries its
  // own Chladni figure now (ItemGlyph.jsx, docs/brand.md) -- and a
  // wrong-glyph-per-row bug is exactly the sort of thing nobody notices
  // by eye, which is what this pins.
  it("marks each list entry with its own glyph", () => {
    render(<Sidebar {...baseProps} config={null} tasks={[]} />);

    for (const [label, kind] of [
      ["Repos", "repos"],
      ["Schedules", "schedules"],
      ["Templates", "templates"],
      ["Suites", "suites"],
    ]) {
      const entry = screen.getByRole("button", {
        name: new RegExp(`^${label}`),
      });
      expect(
        entry.querySelector(`svg[data-glyph="${kind}"]`),
      ).toBeInTheDocument();
    }
  });

  // grain/task-69: the deployment's own name, beside the wordmark, so a
  // staging tab and a production tab are not pixel-identical. Nothing at
  // all when the deployment is unnamed, which is grain's own shape for
  // an operator running one of these.
  it("shows the environment name beside the wordmark when one is configured", () => {
    render(
      <Sidebar
        {...baseProps}
        config={{ environmentName: "staging" }}
        tasks={[]}
      />,
    );
    expect(screen.getByText("staging")).toBeInTheDocument();
  });

  it("shows no environment badge when the deployment is unnamed", () => {
    render(
      <Sidebar {...baseProps} config={{ environmentName: "" }} tasks={[]} />,
    );
    expect(screen.queryByTitle(/^Environment:/)).not.toBeInTheDocument();
  });

  // grain/task-164: which build is answering, in the rail's footer --
  // the short commit and the UTC time it was made. UTC to the minute so
  // that every browser looking at one deployment reads the same string
  // and it lines up with `git log`.
  it("prints the build's short commit and commit time in the footer", () => {
    render(
      <Sidebar
        {...baseProps}
        config={{
          version: {
            commit: "0fbfb4619f0a1c2d3e4f5a6b7c8d9e0f11223344",
            committedAt: "2026-09-03T14:02:11Z",
          },
        }}
        tasks={[]}
      />,
    );

    expect(screen.getByText("0fbfb46 · 2026-09-03 14:02Z")).toBeInTheDocument();
    expect(
      screen.getByTitle(
        /Running commit 0fbfb4619f0a1c2d3e4f5a6b7c8d9e0f11223344/,
      ),
    ).toBeInTheDocument();
  });

  it("marks a build made from a modified tree", () => {
    render(
      <Sidebar
        {...baseProps}
        config={{
          version: {
            commit: "0fbfb46199",
            committedAt: "2026-09-03T14:02:11Z",
            modified: true,
          },
        }}
        tasks={[]}
      />,
    );

    expect(
      screen.getByText("0fbfb46-dirty · 2026-09-03 14:02Z"),
    ).toBeInTheDocument();
    expect(screen.getByTitle(/uncommitted changes/)).toBeInTheDocument();
  });

  // Half a stamp is still worth printing, and a build with none at all
  // (-buildvcs=false, and every `go test` binary) prints nothing rather
  // than an empty line or an "Invalid Date".
  it("prints the commit alone when the stamp carries no usable time", () => {
    const { rerender } = render(
      <Sidebar
        {...baseProps}
        config={{ version: { commit: "0fbfb46199" } }}
        tasks={[]}
      />,
    );
    expect(screen.getByText("0fbfb46")).toBeInTheDocument();

    rerender(
      <Sidebar
        {...baseProps}
        config={{
          version: { commit: "0fbfb46199", committedAt: "not a timestamp" },
        }}
        tasks={[]}
      />,
    );
    expect(screen.getByText("0fbfb46")).toBeInTheDocument();
  });

  it("shows no build stamp when the binary carries none", () => {
    render(<Sidebar {...baseProps} config={{}} tasks={[]} />);
    expect(screen.queryByTitle(/^Running commit/)).not.toBeInTheDocument();
  });

  // bwsalmon/agents#640: Logs and Sandbox health share the "Debug" nav
  // entry rather than having one each of their own. Metrics does have
  // one of its own (grain/task-173), since it is the question asked when
  // nothing is wrong rather than one of these.
  it("does not show Logs or Sandbox health as their own nav entries", () => {
    render(<Sidebar {...baseProps} config={null} tasks={[]} />);

    expect(
      screen.queryByRole("button", { name: "Logs" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Sandbox health" }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Debug" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Metrics" })).toBeInTheDocument();
  });
});
