import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import DetailOverlay from "./DetailOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn().mockResolvedValue(null) }));

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

  it("shows a placeholder when there is no description", () => {
    render(<DetailOverlay task={{ ...baseTask, description: "" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("(no description)")).toBeInTheDocument();
  });

  it("links the pull request when one is attached", () => {
    render(<DetailOverlay task={{ ...baseTask, pullRequest: "acme/widgets#42" }} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    const link = screen.getByRole("link", { name: "acme/widgets#42" });
    expect(link).toHaveAttribute("href", "https://github.com/acme/widgets/pull/42");
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
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", { method: "POST" });
    vi.unstubAllGlobals();
  });

  it("shows Close for a queued task, wired to the close endpoint", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.click(screen.getByRole("button", { name: "Close" }));

    act.mock.calls[0][0]();
    expect(api).toHaveBeenCalledWith("/api/tasks/12/close", { method: "POST" });
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

  it("shows no timeline entries when the task has no history", () => {
    const { container } = render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={vi.fn()} />);
    expect(screen.getByText("Timeline")).toBeInTheDocument();
    expect(container.querySelectorAll(".timeline-item")).toHaveLength(0);
  });

  it("lists every attempt on the timeline in order, with its number, status and timing", () => {
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

    expect(screen.getByText("Attempt #1 · Failed")).toBeInTheDocument();
    expect(screen.getByText("build error")).toBeInTheDocument();
    expect(screen.getByText("Attempt #2 · Running")).toBeInTheDocument();
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
        }}
        tasks={[]}
        config={config}
        onClose={() => {}}
        onOpenTask={() => {}}
        act={vi.fn()}
      />
    );

    const titles = screen.getAllByText(/^Proposed$|^Attempt #1 · Succeeded$|^looks good$|^Closed$/).map((el) => el.textContent);
    expect(titles).toEqual(["Proposed", "Attempt #1 · Succeeded", "looks good", "Closed"]);
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
      body: JSON.stringify({ body: "sounds good" }),
    });
    expect(textarea).toHaveValue("");
  });

  it("does not post an empty or whitespace-only comment", async () => {
    const act = vi.fn();
    const user = userEvent.setup();
    render(<DetailOverlay task={baseTask} tasks={[]} config={config} onClose={() => {}} onOpenTask={() => {}} act={act} />);

    await user.type(screen.getByPlaceholderText("Reply..."), "   ");
    await user.click(screen.getByRole("button", { name: "Comment" }));

    expect(act).not.toHaveBeenCalled();
  });
});
