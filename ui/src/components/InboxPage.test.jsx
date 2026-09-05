import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import InboxPage from "./InboxPage.jsx";
import { inboxTasks } from "../state.js";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const task = (over) => ({
  id: "1",
  title: "A task",
  state: "queued",
  capabilities: [],
  blocked: false,
  ...over,
});

// One task in every case the inbox is about, plus three it is not: a
// running task, a queued one and a closed one all wait on grain rather
// than on the reader.
const tasks = [
  task({ id: "1", title: "Add a dark mode toggle", state: "proposed" }),
  task({ id: "2", title: "Fix flaky retry test", state: "queued" }),
  task({ id: "3", title: "Bump the Go toolchain", state: "running" }),
  task({ id: "4", title: "Investigate the stall", state: "awaiting_reply" }),
  task({ id: "5", title: "Rotate the signing key", state: "failed" }),
  task({
    id: "6",
    title: "Cache the readiness probe",
    state: "awaiting_submit",
    pullRequest: "acme/widgets#12",
  }),
  task({
    id: "7",
    title: "Add pagination to the tasks API",
    state: "completed",
    pullRequest: "acme/widgets#13",
    mergeQueueBlockedAt: "2026-09-01T10:00:00Z",
  }),
  task({ id: "8", title: "Spike: websocket transport", state: "closed" }),
];

const noop = () => {};
const baseProps = {
  config: null,
  onOpenTask: noop,
  onAct: noop,
  showError: noop,
};

function renderInbox(over = {}) {
  return render(<InboxPage {...baseProps} tasks={tasks} {...over} />);
}

// A row's own buttons live beside it, so every assertion about an action
// scopes to the row naming the task rather than to the page.
function row(title) {
  return screen.getByText(title).closest(".inbox-item");
}

describe("InboxPage", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    api.mockReset();
  });

  it("lists every task waiting on the reader, and nothing else", () => {
    const { container } = renderInbox();

    expect(container.querySelectorAll(".inbox-item")).toHaveLength(5);
    expect(screen.getByText("Add a dark mode toggle")).toBeInTheDocument();
    expect(screen.getByText("Investigate the stall")).toBeInTheDocument();
    expect(screen.getByText("Rotate the signing key")).toBeInTheDocument();
    expect(screen.getByText("Cache the readiness probe")).toBeInTheDocument();
    expect(
      screen.getByText("Add pagination to the tasks API"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Fix flaky retry test")).not.toBeInTheDocument();
    expect(screen.queryByText("Bump the Go toolchain")).not.toBeInTheDocument();
    expect(
      screen.queryByText("Spike: websocket transport"),
    ).not.toBeInTheDocument();
  });

  // The one way the page's own groups and state.js's waitsOnUser can
  // drift: a task the predicate admits that no group claims would be
  // counted in the nav rail and shown nowhere.
  it("gives every task waitsOnUser admits a group to sit in", () => {
    const { container } = renderInbox();
    expect(container.querySelectorAll(".inbox-item")).toHaveLength(
      inboxTasks(tasks).length,
    );
  });

  it("groups the waits with the answer each one needs first", () => {
    const { container } = renderInbox();
    const headings = [...container.querySelectorAll(".inbox-group-header h3")];
    expect(headings.map((h) => h.textContent)).toEqual([
      "Answer a question",
      "Submit for merge",
      "Unblock a merge",
      "Retry or close",
      "Approve a proposal",
    ]);
  });

  it("says so when nothing is waiting", () => {
    const { container } = renderInbox({
      tasks: [task({ state: "running" })],
    });
    expect(screen.getByText(/Nothing is waiting on you/)).toBeInTheDocument();
    expect(container.querySelectorAll(".inbox-item")).toHaveLength(0);
  });

  it("approves a proposal from its own row", async () => {
    const onAct = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    renderInbox({ onAct });

    await user.click(
      within(row("Add a dark mode toggle")).getByRole("button", {
        name: "Approve",
      }),
    );

    expect(api).toHaveBeenCalledWith("/api/tasks/1/approve", {
      method: "POST",
    });
  });

  it("submits a finished change from its own row", async () => {
    const onAct = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    renderInbox({ onAct });

    await user.click(
      within(row("Cache the readiness probe")).getByRole("button", {
        name: "Submit",
      }),
    );

    expect(api).toHaveBeenCalledWith("/api/tasks/6/submit", { method: "POST" });
  });

  it("retries a failed task from its own row", async () => {
    const onAct = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    renderInbox({ onAct });

    await user.click(
      within(row("Rotate the signing key")).getByRole("button", {
        name: "Retry",
      }),
    );

    expect(api).toHaveBeenCalledWith("/api/tasks/5/retry", { method: "POST" });
  });

  it("asks before closing, and does nothing when declined", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    const onAct = vi.fn();
    const user = userEvent.setup();
    renderInbox({ onAct });

    await user.click(
      within(row("Rotate the signing key")).getByRole("button", {
        name: "Close",
      }),
    );

    expect(confirmSpy).toHaveBeenCalled();
    expect(onAct).not.toHaveBeenCalled();
  });

  it("closes a proposal once the decline is confirmed", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const onAct = vi.fn((mutate) => mutate());
    const user = userEvent.setup();
    renderInbox({ onAct });

    await user.click(
      within(row("Add a dark mode toggle")).getByRole("button", {
        name: "Decline",
      }),
    );

    expect(api).toHaveBeenCalledWith("/api/tasks/1/close", { method: "POST" });
  });

  it("links a merge the queue gave up on straight to its pull request", () => {
    renderInbox();
    const link = within(row("Add pagination to the tasks API")).getByRole(
      "link",
      { name: "Open pull request" },
    );
    expect(link).toHaveAttribute(
      "href",
      "https://github.com/acme/widgets/pull/13",
    );
  });

  it("opens a task's own pane when its row is clicked", async () => {
    const onOpenTask = vi.fn();
    const user = userEvent.setup();
    renderInbox({ onOpenTask });

    await user.click(screen.getByText("Rotate the signing key"));

    expect(onOpenTask).toHaveBeenCalledWith("5");
  });

  describe("replying to a parked question", () => {
    it("shows the question and posts the answer as a comment", async () => {
      api.mockImplementation(async (path) =>
        path === "/api/tasks/4"
          ? {
              id: "4",
              state: "awaiting_reply",
              comments: [
                { id: 1, body: "filed it" },
                { id: 2, body: "Which base branch should this target?" },
              ],
            }
          : null,
      );
      const onAct = vi.fn((mutate) => mutate());
      const user = userEvent.setup();
      renderInbox({ onAct });

      await user.click(
        within(row("Investigate the stall")).getByRole("button", {
          name: "Reply",
        }),
      );
      expect(
        await screen.findByText("Which base branch should this target?"),
      ).toBeInTheDocument();

      await user.type(
        screen.getByLabelText("Reply to 4"),
        "release-1.4, please",
      );
      await user.click(screen.getByRole("button", { name: "Send" }));

      expect(api).toHaveBeenCalledWith("/api/tasks/4/comments", {
        method: "POST",
        body: JSON.stringify({ body: "release-1.4, please" }),
      });
    });

    it("sends nothing when the box is empty", async () => {
      api.mockResolvedValue({ id: "4", comments: [] });
      const onAct = vi.fn();
      const user = userEvent.setup();
      renderInbox({ onAct });

      await user.click(
        within(row("Investigate the stall")).getByRole("button", {
          name: "Reply",
        }),
      );
      await user.click(await screen.findByRole("button", { name: "Send" }));

      expect(onAct).not.toHaveBeenCalled();
    });

    it("closes the box again when Cancel is clicked", async () => {
      api.mockResolvedValue({ id: "4", comments: [] });
      const user = userEvent.setup();
      renderInbox();

      const line = within(row("Investigate the stall"));
      await user.click(line.getByRole("button", { name: "Reply" }));
      await user.click(await line.findByRole("button", { name: "Cancel" }));

      expect(
        screen.queryByRole("button", { name: "Send" }),
      ).not.toBeInTheDocument();
    });

    // A run parked on request_secret is in the same state as one parked
    // on a question, and a reply is a comment -- so the box that would
    // invite somebody to paste a credential into the conversation is not
    // offered at all.
    it("offers no reply box for a run that asked for a secret", async () => {
      api.mockResolvedValue({
        id: "4",
        state: "awaiting_reply",
        comments: [{ id: 1, body: "I need the deploy key" }],
        pendingSecret: {
          name: "deploy-key",
          secret: "deploy-key",
          key: "value",
          set: false,
        },
      });
      const onOpenTask = vi.fn();
      const user = userEvent.setup();
      renderInbox({ onOpenTask });

      await user.click(
        within(row("Investigate the stall")).getByRole("button", {
          name: "Reply",
        }),
      );

      expect(
        await screen.findByText(/asked for a credential \(deploy-key\)/),
      ).toBeInTheDocument();
      expect(
        screen.queryByRole("button", { name: "Send" }),
      ).not.toBeInTheDocument();

      await user.click(screen.getByRole("button", { name: "Open the task" }));
      expect(onOpenTask).toHaveBeenCalledWith("4");
    });

    it("falls back to the task's own pane when the detail cannot be read", async () => {
      api.mockRejectedValue(new Error("nope"));
      const showError = vi.fn();
      const user = userEvent.setup();
      renderInbox({ showError });

      await user.click(
        within(row("Investigate the stall")).getByRole("button", {
          name: "Reply",
        }),
      );

      expect(
        await screen.findByRole("button", { name: "Open the task to reply" }),
      ).toBeInTheDocument();
      await waitFor(() => expect(showError).toHaveBeenCalled());
    });
  });
});
