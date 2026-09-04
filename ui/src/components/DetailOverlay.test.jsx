import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import DetailOverlay from "./DetailOverlay.jsx";
import api from "../api.js";
import fileToAttachment from "../attachments.js";

vi.mock("../api.js", () => ({ default: vi.fn().mockResolvedValue(null) }));
vi.mock("../attachments.js", () => ({ default: vi.fn() }));

const baseTask = {
  id: "12",
  title: "Fix the login bug",
  description: "Login fails on retry",
  state: "queued",
  autoMerge: false,
  capabilities: [],
  comments: [],
};

const config = {
  capabilities: [{ id: "web-search", name: "Web search", description: "search the web" }],
};

describe("DetailOverlay", () => {
  afterEach(() => {
    api.mockClear();
  });

  it("renders the task id, title, description and state badge", () => {
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);

    expect(screen.getByText("12 Fix the login bug")).toBeInTheDocument();
    expect(screen.getByText("Login fails on retry")).toBeInTheDocument();
    expect(screen.getByText("Queued")).toBeInTheDocument();
  });

  // grain/task-94: a task fills the whole pane beside the sidebar, not a
  // box floating in the middle of it -- an agent's own answer runs long,
  // and reading one through a 900px porthole was the complaint.
  it("opens as a full pane rather than a centered dialog", () => {
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);

    expect(document.querySelector(".MuiDialog-paper")).toHaveClass("MuiDialog-paperFullScreen");
    expect(document.querySelector(".overlay-pane .detail-layout")).toBeInTheDocument();
  });

  it("shows a placeholder when there is no description", () => {
    render(<DetailOverlay task={{ ...baseTask, description: "" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("(no description)")).toBeInTheDocument();
  });

  it("links the pull request when one is attached", () => {
    render(<DetailOverlay task={{ ...baseTask, pullRequest: "acme/widgets#42" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    const link = screen.getByRole("link", { name: "acme/widgets#42" });
    expect(link).toHaveAttribute("href", "https://github.com/acme/widgets/pull/42");
  });

  // A per-task agent framework override gets the same treatment: named
  // only when this task overrides the deployment's own, since most do
  // not and a row saying so on every task is noise.
  it("shows the agent framework only when the task overrides it", () => {
    const { rerender } = render(
      <DetailOverlay
        task={{ ...baseTask, agentFramework: "claude" }}
        tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()}
      />
    );
    expect(screen.getByText("Agent framework")).toBeInTheDocument();
    expect(screen.getByText("Claude")).toBeInTheDocument();

    rerender(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.queryByText("Agent framework")).not.toBeInTheDocument();
  });

  // grain/task-114: a per-task prompt extension override, shown the same
  // way and for the same reason -- and shown in full, since what it says
  // is the whole of what makes this task's runs different.
  it("shows the prompt extension only when the task overrides it", () => {
    const { rerender } = render(
      <DetailOverlay
        task={{ ...baseTask, promptExtension: "Ignore the house rules." }}
        tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()}
      />
    );
    expect(screen.getByText("Prompt extension")).toBeInTheDocument();
    expect(screen.getByText("Ignore the house rules.")).toBeInTheDocument();

    rerender(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.queryByText("Prompt extension")).not.toBeInTheDocument();
  });

  // bwsalmon/agents#534: a per-task sandbox shape override.
  it("shows a sandbox shape override when set, and hides it when not", () => {
    const { rerender } = render(
      <DetailOverlay
        task={{ ...baseTask, sandboxCpus: 4, sandboxMemoryMb: 8192, sandboxDiskGb: 40 }}
        tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()}
      />
    );
    expect(screen.getByText("Sandbox vCPUs")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("Sandbox memory (MiB)")).toBeInTheDocument();
    expect(screen.getByText("8192")).toBeInTheDocument();
    expect(screen.getByText("Sandbox disk (GiB)")).toBeInTheDocument();
    expect(screen.getByText("40")).toBeInTheDocument();

    rerender(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.queryByText("Sandbox vCPUs")).not.toBeInTheDocument();
    expect(screen.queryByText("Sandbox memory (MiB)")).not.toBeInTheDocument();
    expect(screen.queryByText("Sandbox disk (GiB)")).not.toBeInTheDocument();
  });

  // bwsalmon/agents#539: an interactive task's Timeline reads as a chat.
  it("labels an interactive task's mode and renders its Timeline as a Chat", () => {
    render(<DetailOverlay task={{ ...baseTask, interactive: true }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("Mode")).toBeInTheDocument();
    expect(screen.getByText("Interactive")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Chat" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Message...")).toBeInTheDocument();
  });

  it("labels an ordinary task's Timeline as Timeline, not Chat, and shows no Mode row", () => {
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.queryByText("Mode")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Timeline" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Reply...")).toBeInTheDocument();
  });

  it("shows the merge-blocked chip once the merge queue has given up", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, state: "completed", pullRequest: "acme/widgets#42", autoMerge: true, mergeQueueBlockedAt: "2026-08-01T00:00:00Z" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.getByText("Merge blocked")).toBeInTheDocument();
  });

  // Waiting on a Submit click is a state now, so the badge itself says
  // it and no chip is added beside it (state.js's completionPhase).
  it("says awaiting submit on the state badge, with no chip repeating it", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, state: "awaiting_submit", pullRequest: "acme/widgets#42", autoMerge: false }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.getAllByText("Awaiting submit")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Submit" })).toBeInTheDocument();
  });

  it("opens the originating task when generatedFrom is clicked", async () => {
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, generatedFrom: "9" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={onOpenTask} act={vi.fn()} />);

    await user.click(screen.getByText("9"));

    expect(onOpenTask).toHaveBeenCalledWith("9");
  });

  it("shows the blocked badge and failure summary for a failed task", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, state: "failed", blocked: true, failedAttempts: 2, lastFailureReason: "build error" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText("Blocked")).toBeInTheDocument();
    expect(screen.getByText(/2 consecutive failed attempts/)).toBeInTheDocument();
    expect(screen.getByText(/build error/)).toBeInTheDocument();
  });

  it("shows an Approve button for a proposed task, wired to the approve endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, state: "proposed" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Approve" }));

    expect(act).toHaveBeenCalledWith(expect.any(Function), "12");
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/approve", { method: "POST" });
  });

  it("shows a Withdraw approval button for a queued task, wired to the withdraw endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Withdraw approval" }));

    expect(act).toHaveBeenCalledWith(expect.any(Function), "12");
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/withdraw-approval", { method: "POST" });
  });

  // The states Client.WithdrawApproval refuses never get the button:
  // a proposed task has no approval to withdraw, and a running one is
  // stopped with Cancel instead.
  it("offers no Withdraw approval button on a task that is not queued", () => {
    const { rerender } = render(
      <DetailOverlay task={{ ...baseTask, state: "proposed" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />
    );
    expect(screen.queryByRole("button", { name: "Withdraw approval" })).not.toBeInTheDocument();

    rerender(
      <DetailOverlay task={{ ...baseTask, state: "running" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />
    );
    expect(screen.queryByRole("button", { name: "Withdraw approval" })).not.toBeInTheDocument();
  });

  it("shows a Submit button once a pull request exists and auto-merge is off", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, pullRequest: "acme/widgets#1" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Submit" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/submit", { method: "POST" });
  });

  it("hides the Submit button once auto-merge is already on", () => {
    render(<DetailOverlay task={{ ...baseTask, pullRequest: "acme/widgets#1", autoMerge: true }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Submit" })).not.toBeInTheDocument();
  });

  // bwsalmon/agents#483: on a deployment whose GitHub credential can't
  // read pull request checks, Submit still flips autoMerge but nothing
  // ever merges -- without this warning that looked identical to Submit
  // doing nothing at all.
  it("warns that auto-merge is degraded once a task is submitted on a deployment that can't read checks", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, pullRequest: "acme/widgets#1", autoMerge: true }}
        tasks={[]}
        config={{ ...config, autoMergeDegraded: true }}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.getByText(/can't read pull request checks/)).toBeInTheDocument();
  });

  it("shows no auto-merge warning on a deployment that can read checks", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, pullRequest: "acme/widgets#1", autoMerge: true }}
        tasks={[]}
        config={{ ...config, autoMergeDegraded: false }}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.queryByText(/can't read pull request checks/)).not.toBeInTheDocument();
  });

  it("shows no auto-merge warning before a task has been submitted, even when degraded", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, pullRequest: "acme/widgets#1", autoMerge: false }}
        tasks={[]}
        config={{ ...config, autoMergeDegraded: true }}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.queryByText(/can't read pull request checks/)).not.toBeInTheDocument();
  });

  it("shows a Retry button for a failed task, wired to the retry endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, state: "failed" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Retry" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/retry", { method: "POST" });
  });

  it("shows Reopen for a closed task, wired to the reopen endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, state: "closed" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Reopen" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/reopen", { method: "POST" });
  });

  // A pull request nobody is going to merge, said where the person who
  // closed the task is looking. grain leaves the same note on the task
  // and on the pull request itself (model.OrphanedPullRequestNote); this
  // is the copy that needs no reading of the conversation to find.
  it("warns that a closed task has left its pull request open and unwatched", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, state: "closed", pullRequest: "acme/widgets#42" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText(/acme\/widgets#42 is still open, and grain has stopped watching it\./)).toBeInTheDocument();
  });

  // The ordinary ending closes the task too: a pull request that merged
  // sets ClosedAt alongside PrMergedAt (orchestrator.
  // recordPullRequestEvents), and there is nothing orphaned about it.
  it("says nothing about a closed task whose pull request merged", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          state: "closed",
          pullRequest: "acme/widgets#42",
          pullRequestEvents: [{ kind: "merged", at: "2026-08-28T12:00:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.queryByText(/grain has stopped watching it/)).not.toBeInTheDocument();
  });

  it("shows Cancel for a running task, only closing after confirmation", async () => {
    const act = vi.fn();
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, state: "running" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(act).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("closes a running task once cancellation is confirmed", async () => {
    const act = vi.fn();
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const user = userEvent.setup();
    render(<DetailOverlay task={{ ...baseTask, state: "running" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", {
      method: "POST",
      body: JSON.stringify({ close_pull_request: false }),
    });
    vi.unstubAllGlobals();
  });

  it("shows Close for a queued task, wired to the close endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Close" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", {
      method: "POST",
      body: JSON.stringify({ close_pull_request: false }),
    });
  });

  // The one choice in grain that destroys work on GitHub, so it is worth
  // pinning all three halves of it: the box is only offered where there
  // is an open pull request to close, it is off until somebody ticks it,
  // and what it sends is read at the moment Close is clicked.
  it("offers to close an open pull request alongside the task, unticked", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, pullRequest: "acme/widgets#42" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={act}
      />
    );

    const box = screen.getByRole("checkbox", { name: /Close acme\/widgets#42 too/ });
    expect(box).not.toBeChecked();

    await user.click(screen.getByRole("button", { name: "Close" }));
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", {
      method: "POST",
      body: JSON.stringify({ close_pull_request: false }),
    });

    await user.click(box);
    await user.click(screen.getByRole("button", { name: "Close" }));
    act.mock.calls[1][0]();
    expect(api).toHaveBeenLastCalledWith("/api/tasks/12/close", {
      method: "POST",
      body: JSON.stringify({ close_pull_request: true }),
    });
  });

  it("does not offer to close a pull request there is no open one to close", () => {
    const { rerender } = render(
      <DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />
    );
    expect(screen.queryByRole("checkbox", { name: /too/ })).not.toBeInTheDocument();

    // One that already merged is the same: there is nothing left to
    // close, and an option with nothing behind it invites a tick that
    // means nothing.
    rerender(
      <DetailOverlay
        task={{
          ...baseTask,
          pullRequest: "acme/widgets#42",
          pullRequestEvents: [{ kind: "merged", at: "2026-08-28T12:00:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.queryByRole("checkbox", { name: /too/ })).not.toBeInTheDocument();
  });

  // Cancelling a running task takes the same close path, so it can orphan
  // a pull request the run had already opened -- and its confirmation has
  // to say when confirming it will also shut one.
  it("names the pull request in a cancel confirmation when it will be closed too", async () => {
    const act = vi.fn();
    const confirm = vi.fn().mockReturnValue(true);
    vi.stubGlobal("confirm", confirm);
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, state: "running", pullRequest: "acme/widgets#42" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={act}
      />
    );

    await user.click(screen.getByRole("checkbox", { name: /Close acme\/widgets#42 too/ }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(confirm.mock.calls[0][0]).toMatch(/close acme\/widgets#42 on GitHub/);
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", {
      method: "POST",
      body: JSON.stringify({ close_pull_request: true }),
    });
    vi.unstubAllGlobals();
  });

  it("shows declared repo, base, reads and auto-merge", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, repo: "acme/widgets", base: "main", reads: ["acme/shared"], autoMerge: true }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("main")).toBeInTheDocument();
    expect(screen.getByText("acme/shared")).toBeInTheDocument();
    expect(screen.getByText("true")).toBeInTheDocument();
  });

  it("toggles a capability via the capabilities select", async () => {
    // act must run its mutate callback synchronously, the same as the
    // real App.jsx does -- the callback closes over the change event's
    // selection, computed from the task's current capabilities, which
    // would otherwise be stale by the time a deferred `act.mock.calls`
    // invocation (as the other assertions in this file use) ran it.
    const act = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "Web search" }));

    expect(api).toHaveBeenCalledWith("/api/tasks/12/capabilities", {
      method: "POST",
      body: JSON.stringify({ id: "web-search", attach: true }),
    });
  });

  // A capability the picker stopped listing since the task was granted
  // it ("scratch-repo", renamed to github-sandbox in
  // bwsalmon/agents#612) still needs a row of its own, or the chip for
  // it has nothing to untick and the grant -- which fails every run of
  // the task holding it -- can never be removed.
  it("offers a row for a granted capability the picker no longer lists", async () => {
    const act = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    const task = { ...baseTask, capabilities: ["scratch-repo"] };
    render(<DetailOverlay task={task} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByLabelText("Capabilities"));
    await user.click(await screen.findByRole("option", { name: "scratch-repo" }));

    expect(api).toHaveBeenCalledWith("/api/tasks/12/capabilities", {
      method: "POST",
      body: JSON.stringify({ id: "scratch-repo", attach: false }),
    });
  });

  it("shows no timeline entries when the task has no history", () => {
    const { container } = render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("Timeline")).toBeInTheDocument();
    expect(container.querySelectorAll(".timeline-item")).toHaveLength(0);
  });

  // bwsalmon/agents#503: the first attempt is the task's one "running"
  // node -- it only reads as "Attempt #1" once a second one exists to
  // tell it apart from.
  it("lists every attempt on the timeline in order, numbering only the second and later ones", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          attempts: [
            { number: 1, startedAt: "2026-08-28T12:00:00Z", finishedAt: "2026-08-28T12:10:00Z", outcome: "failed", detail: "build error" },
            { number: 2, startedAt: "2026-08-28T13:00:00Z" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.queryByText("Attempt #1 · Failed")).not.toBeInTheDocument();
    expect(screen.getByText("build error")).toBeInTheDocument();
    expect(screen.getByText("Attempt #2 · Running")).toBeInTheDocument();
  });

  // bwsalmon/agents#503: a "running" transition and the attempt it started
  // both landing on the timeline read as two nodes for the same moment.
  it("shows a single node for a running transition and its first attempt, not two", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          state: "running",
          transitions: [
            { state: "queued", at: "2026-08-28T12:00:00Z" },
            { state: "running", at: "2026-08-28T12:01:00Z" },
          ],
          attempts: [{ number: 1, startedAt: "2026-08-28T12:01:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const list = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-list");
    expect(within(list).getAllByText("Running")).toHaveLength(1);
    expect(list.querySelectorAll(".timeline-item")).toHaveLength(2);
  });

  it("lists every transition on the timeline in order, with its label and time", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          transitions: [
            { state: "proposed", at: "2026-08-28T12:00:00Z" },
            { state: "queued", at: "2026-08-28T12:01:00Z" },
            { state: "running", at: "2026-08-28T12:02:00Z" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const list = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-list");
    const labels = ["Proposed", "Queued", "Running"];
    for (const label of labels) {
      expect(within(list).getByText(label)).toBeInTheDocument();
    }
  });

  it("lists pull request events on the timeline with their own label and, when a pull request is linked, a link to it", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          pullRequest: "acme/widgets#42",
          pullRequestEvents: [
            { kind: "opened", at: "2026-08-28T12:00:00Z" },
            { kind: "merged", at: "2026-08-28T13:00:00Z" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const list = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-list");
    expect(within(list).getByText("PR opened")).toBeInTheDocument();
    expect(within(list).getByText("PR merged")).toBeInTheDocument();
    expect(within(list).getAllByRole("link", { name: "acme/widgets#42" })).toHaveLength(2);
  });

  it("labels a pull request closed without merging distinctly from a merged one", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, pullRequestEvents: [{ kind: "closed", at: "2026-08-28T12:00:00Z" }] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );
    expect(screen.getByText("PR closed without merging")).toBeInTheDocument();
  });

  // bwsalmon/agents#503: a synced "PR closed"/"merged" event can land with
  // a later timestamp than the local "closed" transition that preceded
  // it -- "closed" should still read as the end of the timeline.
  it("keeps the closed transition last even when a pull request event syncs in with a later timestamp", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          transitions: [
            { state: "proposed", at: "2026-08-28T09:00:00Z" },
            { state: "closed", at: "2026-08-28T10:00:00Z" },
          ],
          pullRequestEvents: [{ kind: "merged", at: "2026-08-28T11:00:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const list = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-list");
    const items = [...list.querySelectorAll(".timeline-item")];
    expect(items[items.length - 1].textContent).toContain("Closed");
  });

  // bwsalmon/agents#503: a pull request re-linked to a reopened task keeps
  // its own original open time, which can predate this lifecycle's first
  // transition -- it should not sort ahead of everything else.
  it("does not let a pull request opened before this lifecycle's first transition sort ahead of it", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          transitions: [{ state: "proposed", at: "2026-08-28T12:00:00Z" }],
          pullRequestEvents: [{ kind: "opened", at: "2026-08-01T00:00:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const list = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-list");
    const items = [...list.querySelectorAll(".timeline-item")];
    expect(items[0].textContent).toContain("Proposed");
  });

  it("interleaves transitions, attempts and comments by their own timestamps", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          transitions: [
            { state: "proposed", at: "2026-08-28T09:00:00Z" },
            { state: "closed", at: "2026-08-28T15:00:00Z" },
          ],
          attempts: [{ number: 1, startedAt: "2026-08-28T12:00:00Z", finishedAt: "2026-08-28T12:10:00Z", outcome: "succeeded" }],
          comments: [{ author: "alice", authorKind: "human", body: "looks good", createdAt: "2026-08-28T13:00:00Z" }],
          pullRequestEvents: [{ kind: "merged", at: "2026-08-28T14:00:00Z" }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const titles = screen.getAllByText(/^Proposed$|^Succeeded$|^looks good$|^PR merged$|^Closed$/).map((el) => el.textContent);
    expect(titles).toEqual(["Proposed", "Succeeded", "looks good", "PR merged", "Closed"]);
  });

  it("only animates the running badge for the current transition, not a past one", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          state: "running",
          transitions: [
            { state: "queued", at: "2026-08-28T12:00:00Z" },
            { state: "running", at: "2026-08-28T12:01:00Z" },
            { state: "awaiting_reply", at: "2026-08-28T12:02:00Z" },
            { state: "running", at: "2026-08-28T12:03:00Z" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const runningItems = screen.getAllByText("Running").map((el) => el.closest(".timeline-item"));
    expect(runningItems[0].querySelector(".badge")).toHaveClass("badge-static");
    expect(runningItems[1].querySelector(".badge")).not.toHaveClass("badge-static");
  });

  // A run that ended without an agent ever finishing still ended, and
  // badly. Without an entry of its own, "setup-failed" rendered through
  // the raw-string fallback as "Setup-failed" under a "queued" badge --
  // which reads as a run that has not started, for one that started and
  // could not be given a sandbox.
  it("labels an attempt whose sandbox could not be built", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          attempts: [
            {
              number: 1,
              startedAt: "2026-08-28T12:00:00Z",
              finishedAt: "2026-08-28T12:02:00Z",
              outcome: "setup-failed",
              detail: "this run's sandbox could not be prepared: guest never became reachable",
            },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const item = screen.getByText("Setup failed").closest(".timeline-item");
    expect(item.querySelector(".badge")).toHaveClass("badge-failed");
  });

  it("labels an attempt whose process died mid-run", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          attempts: [
            {
              number: 1,
              startedAt: "2026-08-28T12:00:00Z",
              finishedAt: "2026-08-28T12:02:00Z",
              outcome: "orphaned",
            },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const item = screen.getByText("Orphaned").closest(".timeline-item");
    expect(item.querySelector(".badge")).toHaveClass("badge-failed");
  });

  // The one a task retried over and over is most likely to be sitting
  // on: the agent worked, the branch is pushed, and the call that turns
  // it into a pull request is what failed. Through the raw-string
  // fallback it read "Finish-failed" under a "queued" badge, which is
  // the opposite of what a reader needs from an attempt that is the
  // reason the task keeps coming back.
  it("labels an attempt whose result could not be turned into a pull request", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          attempts: [
            {
              number: 1,
              startedAt: "2026-08-28T12:00:00Z",
              finishedAt: "2026-08-28T12:02:00Z",
              outcome: "finish-failed",
              detail:
                "this run's result could not be turned into a pull request or a comment: 422 Validation Failed",
            },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const item = screen.getByText("Finish failed").closest(".timeline-item");
    expect(item.querySelector(".badge")).toHaveClass("badge-failed");
  });

  // The fallback still has to work: an outcome added on the backend
  // before this map catches up shows up rather than disappearing.
  it("falls back to the raw outcome for one it does not recognise", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          attempts: [
            { number: 1, startedAt: "2026-08-28T12:00:00Z", finishedAt: "2026-08-28T12:02:00Z", outcome: "invented" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText("Invented")).toBeInTheDocument();
  });

  it("shows a hint when there are no dependencies", () => {
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("No dependencies.")).toBeInTheDocument();
  });

  it("renders dependency chips, marking blocking ones, and removes a dependency via the depends-on endpoint", async () => {
    const act = vi.fn();
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, dependsOn: ["9", "10"], blockedBy: ["9"] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={onOpenTask}
        act={act}
      />
    );

    expect(screen.getByText("9 (open)")).toBeInTheDocument();
    expect(screen.getByText("10")).toBeInTheDocument();

    await user.click(screen.getByTitle("Remove dependency on 9"));
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/depends-on", {
      method: "POST",
      body: JSON.stringify({ id: "9", attach: false }),
    });

    await user.click(screen.getByText("10"));
    expect(onOpenTask).toHaveBeenCalledWith("10");
  });

  it("labels a dependency chip with the depended-on task's title, and the whole task on hover", async () => {
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, dependsOn: ["9"], blockedBy: ["9"] }}
        tasks={[{ id: "9", title: "Add dark mode", state: "running" }]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const chip = screen.getByText("9 Add dark mode (open)");
    await user.hover(chip);
    expect(await screen.findByRole("tooltip")).toHaveTextContent("9 Add dark mode — Running (blocking this task)");
  });

  it("falls back to the id for a dependency the task list does not carry", async () => {
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, dependsOn: ["9"] }}
        tasks={[{ id: "20", title: "Add dark mode", state: "queued" }]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    await user.hover(screen.getByText("9"));
    expect(await screen.findByRole("tooltip")).toHaveTextContent("9");
  });

  it("adds a dependency picked from the task picker", async () => {
    const act = vi.fn();
    const tasks = [{ id: "20", title: "Add dark mode" }];
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={tasks} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.type(screen.getByPlaceholderText("Add a dependency…"), "20");
    await user.click(await screen.findByText("Add dark mode"));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/depends-on", {
      method: "POST",
      body: JSON.stringify({ id: "20", attach: true }),
    });
  });

  it("renders comments, distinguishing ones relayed on behalf of someone else", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          comments: [
            { author: "alice", authorKind: "human", body: "please retry" },
            { author: "grain", authorKind: "agent", onBehalfOf: "the dispatched run", body: "what branch should I target?" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByText("alice · human")).toBeInTheDocument();
    expect(screen.getByText("please retry")).toBeInTheDocument();
    expect(screen.getByText("grain on behalf of the dispatched run · agent")).toBeInTheDocument();
  });

  // grain/task-93: an agent's answer arrives as markdown, and reading it
  // as one flat run of text is what this renders it out of.
  it("renders a comment body as markdown", () => {
    // document, not render()'s own container: Overlay is a MUI Dialog,
    // so its content is portalled out of the container into <body>.
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          comments: [
            { author: "grain", authorKind: "agent", body: "## What I did\n\n- touched `run.go`\n- pushed it" },
          ],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(screen.getByRole("heading", { name: "What I did" })).toBeInTheDocument();
    expect(document.querySelectorAll(".timeline-comment-body li")).toHaveLength(2);
    expect(screen.getByText("run.go").tagName).toBe("CODE");
  });

  // A description is markdown for the same reason a comment body is: a
  // proposed task's own body is written by the run that proposed it
  // (propose_task), in the same syntax.
  it("renders the description as markdown", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, description: "**Why:** the retry path never resets" }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    expect(document.querySelector(".description strong")).toHaveTextContent("Why:");
  });

  it("posts a comment and clears the textarea", async () => {
    const act = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    const textarea = screen.getByPlaceholderText("Reply...");
    await user.type(textarea, "sounds good");
    await user.click(screen.getByRole("button", { name: "Comment" }));

    expect(act).toHaveBeenCalledWith(expect.any(Function), "12");
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/comments", {
      method: "POST",
      body: JSON.stringify({ body: "sounds good", attachments: [] }),
    });
    expect(textarea).toHaveValue("");
  });

  // grain/task-91, grain/task-175: the title and description above are
  // only part of what a dispatch hands the agent, so the whole prompt is
  // a click away from the task itself -- the only place it is offered,
  // now that task rows no longer carry a button of their own.
  it("opens the full prompt the agent was given", async () => {
    const user = userEvent.setup();
    api.mockResolvedValueOnce({ prompt: "Fix the login bug\n\nWork in acme/widgets.", attempt: 2 });
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Prompt" }));

    expect(await screen.findByText(/Work in acme\/widgets\./)).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/tasks/12/prompt");
  });

  // bwsalmon/agents#523: "After creating a task the user should be able
  // to edit it."
  it("edits the title and description via the PATCH endpoint", async () => {
    const act = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));

    const title = screen.getByLabelText("Title");
    await user.clear(title);
    await user.type(title, "Fix the login bug for real");
    const description = screen.getByLabelText("Description");
    await user.clear(description);
    await user.type(description, "New repro steps");

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(act).toHaveBeenCalledWith(expect.any(Function), "12");
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12", {
      method: "PATCH",
      body: JSON.stringify({ title: "Fix the login bug for real", description: "New repro steps" }),
    });
    // Back to the read-only view once saved.
    expect(screen.queryByLabelText("Title")).not.toBeInTheDocument();
    expect(screen.getByText("12 Fix the login bug")).toBeInTheDocument();
  });

  it("leaves the task unchanged when editing is cancelled", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));
    await user.type(screen.getByLabelText("Title"), " (draft)");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(act).not.toHaveBeenCalled();
    expect(screen.getByText("12 Fix the login bug")).toBeInTheDocument();
  });

  it("disables Save once the title is cleared", async () => {
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));
    await user.clear(screen.getByLabelText("Title"));

    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("does not post an empty or whitespace-only comment with no attachment either", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.type(screen.getByPlaceholderText("Reply..."), "   ");
    await user.click(screen.getByRole("button", { name: "Comment" }));

    expect(act).not.toHaveBeenCalled();
  });

  // bwsalmon/agents#522: a reply that carries only a file, no text, is
  // still worth sending -- the empty-comment guard above must not treat
  // it the same as one with nothing at all.
  it("posts a comment carrying only an attachment, with no body", async () => {
    const upload = { filename: "screenshot.png", contentType: "image/png", content: "ZmFrZQ==" };
    fileToAttachment.mockResolvedValueOnce(upload);
    const act = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    const file = new File(["fake"], "screenshot.png", { type: "image/png" });
    await user.upload(document.querySelector('input[type="file"]'), file);
    await user.click(screen.getByRole("button", { name: "Comment" }));

    expect(act).toHaveBeenCalledWith(expect.any(Function), "12");
    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/comments", {
      method: "POST",
      body: JSON.stringify({ body: "", attachments: [upload] }),
    });
  });

  it("shows a comment's own attachments as links to the download endpoint", () => {
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          comments: [{ author: "alice", authorKind: "human", body: "", attachments: [{ id: 5, filename: "repro.zip", contentType: "application/zip", size: 9 }] }],
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const link = screen.getByRole("link", { name: "repro.zip" });
    expect(link).toHaveAttribute("href", "/api/tasks/12/attachments/5");
  });

  it("shows the task's own attachments as links to the download endpoint", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, attachments: [{ id: 3, filename: "notes.txt", contentType: "text/plain", size: 4 }] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const link = screen.getByRole("link", { name: "notes.txt" });
    expect(link).toHaveAttribute("href", "/api/tasks/12/attachments/3");
  });

  // bwsalmon/agents#446: typing on a task attempt opens a window over its
  // own agent transcript.
  it("opens an attempt's transcript when its timeline row is clicked", async () => {
    api.mockResolvedValue({ transcript: "found it" });
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, attempts: [{ number: 1, startedAt: "2026-08-28T12:00:00Z", finishedAt: "2026-08-28T12:10:00Z", outcome: "succeeded" }] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
        showError={vi.fn()}
      />
    );

    await user.click(screen.getByText("Succeeded"));

    expect(await screen.findByText("Attempt #1 transcript")).toBeInTheDocument();
    expect(await screen.findByText("found it")).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/tasks/12/attempts/1/transcript");
  });

  it("opens an attempt's transcript when its timeline row is focused and Enter is pressed", async () => {
    api.mockResolvedValue({ transcript: "found it" });
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{ ...baseTask, attempts: [{ number: 1, startedAt: "2026-08-28T12:00:00Z" }] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
        showError={vi.fn()}
      />
    );

    screen.getByText("Running").closest(".timeline-item-attempt").focus();
    await user.keyboard("{Enter}");

    expect(await screen.findByText("Attempt #1 transcript")).toBeInTheDocument();
  });

  it("does not make a plain transition or comment row interactive", () => {
    render(
      <DetailOverlay
        task={{ ...baseTask, transitions: [{ state: "queued", at: "2026-08-28T12:00:00Z" }] }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
        showError={vi.fn()}
      />
    );

    const row = screen.getByText("Timeline").closest(".timeline").querySelector(".timeline-item");
    expect(row).not.toHaveClass("timeline-item-attempt");
    expect(row).not.toHaveAttribute("tabindex");
  });

  // grain/task-230: a run that called request_secret parks its task the
  // way a question does, and the pane offers a box for the one
  // credential it asked for. The value must not go anywhere near the
  // conversation -- these three pin that it is sent to the task's own
  // secret endpoint, and that a task nobody asked a secret of is offered
  // nothing.
  it("offers a write-only box for the secret a parked run asked for", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          state: "awaiting_reply",
          pendingSecret: { name: "stripe-api-key", secret: "stripe-api-key", key: "value", set: false },
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={act}
        showError={vi.fn()}
      />
    );

    const box = screen.getByLabelText("Value for stripe-api-key");
    expect(box).toHaveAttribute("type", "password");
    await user.type(box, "sk_live_1");
    await user.click(screen.getByRole("button", { name: "Set secret" }));

    expect(api).toHaveBeenCalledWith("/api/tasks/12/secret", {
      method: "PUT",
      body: JSON.stringify({ value: "sk_live_1" }),
    });
    // Never as a comment: that is the whole distinction this box exists
    // to draw.
    expect(api).not.toHaveBeenCalledWith("/api/tasks/12/comments", expect.anything());
  });

  it("keeps a rejected value in the box rather than clearing it", async () => {
    const showError = vi.fn();
    const user = userEvent.setup();
    api.mockRejectedValueOnce(new Error("no secrets store"));
    render(
      <DetailOverlay
        task={{
          ...baseTask,
          state: "awaiting_reply",
          pendingSecret: { name: "stripe-api-key", secret: "stripe-api-key", key: "value", set: false },
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
        showError={showError}
      />
    );

    await user.type(screen.getByLabelText("Value for stripe-api-key"), "sk_live_1");
    await user.click(screen.getByRole("button", { name: "Set secret" }));

    expect(showError).toHaveBeenCalled();
    expect(screen.getByLabelText("Value for stripe-api-key")).toHaveValue("sk_live_1");
  });

  it("offers no secret box on a task with no pending secret", () => {
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} showError={vi.fn()} />);

    expect(screen.queryByRole("button", { name: "Set secret" })).not.toBeInTheDocument();
  });
});
