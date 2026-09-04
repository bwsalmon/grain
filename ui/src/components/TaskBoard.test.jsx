import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import TaskBoard from "./TaskBoard.jsx";
import { BOARD_STORAGE_KEY } from "../board.js";

const tasks = [
  { id: 1, title: "Fix the thing", state: "queued", capabilities: [], blocked: false },
  { id: 2, title: "Ship the other thing", state: "running", capabilities: [], blocked: false },
  { id: 3, title: "Answer me", state: "awaiting_reply", capabilities: [], blocked: false },
  { id: 4, title: "Long done", state: "closed", capabilities: [], blocked: false },
];

function renderBoard(overrides = {}) {
  const props = {
    tasks,
    config: null,
    onOpenTask: vi.fn(),
    selected: new Set(),
    onToggleSelect: vi.fn(),
    onSelectAll: vi.fn(),
    ...overrides,
  };
  render(<TaskBoard {...props} />);
  return props;
}

// column finds a column by the title in its own header, and cardsIn the
// task titles inside it, in order.
function column(title) {
  return screen.getByText(title).closest(".board-column");
}

function cardsIn(title) {
  return [...column(title).querySelectorAll(".board-card-title")].map((el) => el.textContent);
}

function cardFor(text) {
  return screen.getByText(text).closest("li");
}

describe("TaskBoard", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  describe("the default board", () => {
    it("puts each task in the column its state belongs to", () => {
      renderBoard();
      expect(cardsIn("Queued")).toEqual(["Fix the thing"]);
      expect(cardsIn("Running")).toEqual(["Ship the other thing"]);
      expect(cardsIn("Needs you")).toEqual(["Answer me"]);
    });

    it("leaves closed tasks off the board, and says how many it is not showing", () => {
      renderBoard();
      expect(screen.queryByText("Long done")).not.toBeInTheDocument();
      expect(screen.getByText(/1 task in no column \(Closed\)/)).toBeInTheDocument();
    });

    it("counts what is on the board, and what is in each column", () => {
      renderBoard();
      expect(document.querySelector(".content-header .count")).toHaveTextContent("3");
      expect(column("Queued").querySelector(".count")).toHaveTextContent("1");
      expect(column("Proposed").querySelector(".count")).toHaveTextContent("0");
    });

    it("shows every column, including the ones with nothing in them", () => {
      renderBoard();
      expect(column("Proposed")).toBeInTheDocument();
      expect(column("Proposed")).toHaveTextContent("Nothing here.");
    });

    it("opens a task when its card is clicked", async () => {
      const user = userEvent.setup();
      const { onOpenTask } = renderBoard();
      await user.click(screen.getByText("Fix the thing"));
      expect(onOpenTask).toHaveBeenCalledWith(1);
    });
  });

  describe("what a card shows", () => {
    it("carries the same chips a row does", () => {
      renderBoard({
        tasks: [{
          id: 7, title: "Scheduled work", state: "queued", repo: "acme/widgets",
          capabilities: ["web-search"], blocked: false, scheduled: true,
        }],
        config: { capabilities: [{ id: "web-search", name: "Web search" }] },
      });
      expect(screen.getByText("scheduled")).toBeInTheDocument();
      expect(screen.getByText("acme/widgets")).toBeInTheDocument();
      expect(screen.getByText("Web search")).toBeInTheDocument();
    });

    // grain/task-284: the board has no nesting to explain a stacked task
    // with, so every stacked card carries the chip -- and it has to say
    // which of the two kinds it is, since a review read as a merge fix
    // would claim the change had a red build.
    it("tells a review apart from a merge fix on a stacked card", () => {
      renderBoard({
        tasks: [
          { id: 10, title: "Repair the pull request", state: "queued", capabilities: [], blocked: false, stacked: true, generatedFrom: 1 },
          { id: 11, title: "Review the change", state: "queued", capabilities: [], blocked: false, stacked: true, review: true, generatedFrom: 2 },
        ],
      });
      expect(screen.getByTitle("the merge queue's own automatic fix for 1")).toHaveTextContent("merge fix");
      expect(screen.getByTitle("a review of 2's own code, run before it merges")).toHaveTextContent("review");
    });

    it("says what a running task's own agent is doing", () => {
      renderBoard({
        tasks: [{
          id: 8, title: "Busy", state: "running", capabilities: [], blocked: false,
          activity: "waiting for CI", activityAt: new Date().toISOString(),
        }],
      });
      expect(screen.getByText("waiting for CI")).toBeInTheDocument();
    });

    it("says a task is blocked, and on what", () => {
      renderBoard({
        tasks: [{ id: 9, title: "Waiting on 1", state: "queued", capabilities: [], blocked: true, blockedBy: [1] }],
      });
      expect(screen.getByTitle("Waiting on 1")).toBeInTheDocument();
    });

    // A column of one state would put the same dot on every card in it;
    // a column collecting three needs it to tell them apart.
    it("shows a state dot only where a column holds more than one state", () => {
      renderBoard();
      expect(cardFor("Answer me").querySelector(".badge")).toBeInTheDocument();
      expect(cardFor("Fix the thing").querySelector(".badge")).not.toBeInTheDocument();
    });
  });

  describe("narrowing which tasks show up", () => {
    const wider = [
      { id: 1, title: "Alpha", state: "queued", repo: "acme/widgets", capabilities: [], blocked: false },
      { id: 2, title: "Beta", state: "running", repo: "acme/gadgets", capabilities: [], blocked: false },
    ];

    it("searches by title", async () => {
      const user = userEvent.setup();
      renderBoard({ tasks: wider });
      await user.type(screen.getByPlaceholderText("Search tasks…"), "alph");
      expect(screen.getByText("Alpha")).toBeInTheDocument();
      expect(screen.queryByText("Beta")).not.toBeInTheDocument();
    });

    it("narrows by an attribute, using the list's own filter menus", async () => {
      const user = userEvent.setup();
      renderBoard({ tasks: wider });
      await user.click(screen.getByLabelText("Repo"));
      await user.click(screen.getByRole("option", { name: "acme/gadgets" }));
      expect(screen.queryByText("Alpha")).not.toBeInTheDocument();
      expect(screen.getByText("Beta")).toBeInTheDocument();
    });

    it("puts everything back with one Clear", async () => {
      const user = userEvent.setup();
      renderBoard({ tasks: wider });
      await user.type(screen.getByPlaceholderText("Search tasks…"), "alph");
      await user.click(screen.getByRole("button", { name: "Clear" }));
      expect(screen.getByText("Beta")).toBeInTheDocument();
    });

    it("says so when the search and filters leave nothing", async () => {
      const user = userEvent.setup();
      renderBoard({ tasks: wider });
      await user.type(screen.getByPlaceholderText("Search tasks…"), "nothing matches this");
      expect(screen.getByText("No tasks match this search and these filters.")).toBeInTheDocument();
    });

    // The filter menus are built from the tasks the columns admit, so a
    // board with no "Closed" column never offers a value only a closed
    // task carries.
    it("does not offer a filter value only an off-board task has", () => {
      renderBoard({
        tasks: [
          { id: 1, title: "Alpha", state: "queued", repo: "acme/widgets", capabilities: [], blocked: false },
          { id: 2, title: "Old", state: "closed", repo: "acme/archive", capabilities: [], blocked: false },
        ],
      });
      expect(screen.queryByLabelText("Repo")).not.toBeInTheDocument();
    });

    it("orders the cards within a column", async () => {
      const user = userEvent.setup();
      renderBoard({
        tasks: [
          { id: 1, title: "Zulu", state: "queued", capabilities: [], blocked: false },
          { id: 2, title: "Alpha", state: "queued", capabilities: [], blocked: false },
        ],
      });
      expect(cardsIn("Queued")).toEqual(["Zulu", "Alpha"]);
      await user.click(screen.getByLabelText("Sort"));
      await user.click(screen.getByRole("option", { name: "Title (A–Z)" }));
      expect(cardsIn("Queued")).toEqual(["Alpha", "Zulu"]);
    });
  });

  describe("customizing the columns", () => {
    it("lays the board out the way this browser last saved it", () => {
      localStorage.setItem(BOARD_STORAGE_KEY, JSON.stringify([
        { id: "everything", title: "Everything", states: ["queued", "running", "awaiting_reply", "closed"] },
      ]));
      renderBoard();
      expect(cardsIn("Everything")).toEqual(["Fix the thing", "Ship the other thing", "Answer me", "Long done"]);
      expect(screen.queryByText(/in no column/)).not.toBeInTheDocument();
    });

    it("saves an edit and lays the board out again straight away", async () => {
      const user = userEvent.setup();
      renderBoard();

      await user.click(screen.getByRole("button", { name: "Columns" }));
      await user.click(screen.getByRole("button", { name: "+ Add column" }));
      const titles = screen.getAllByLabelText(/Column \d+ title/);
      const last = titles.length - 1;
      await user.clear(titles[last]);
      await user.type(titles[last], "Archive");
      await user.click(screen.getAllByLabelText("States")[last]);
      await user.click(screen.getByRole("option", { name: "Closed" }));
      // A multi-state Select stays open for the next pick; Escape is how
      // somebody who has picked their one state gets back to the form.
      await user.keyboard("{Escape}");
      await user.click(screen.getByRole("button", { name: "Save" }));

      expect(cardsIn("Archive")).toEqual(["Long done"]);
      expect(JSON.parse(localStorage.getItem(BOARD_STORAGE_KEY)).at(-1)).toMatchObject({
        title: "Archive",
        states: ["closed"],
      });
    });

    it("leaves the board alone when the editor is cancelled", async () => {
      const user = userEvent.setup();
      renderBoard();
      await user.click(screen.getByRole("button", { name: "Columns" }));
      await user.click(screen.getByRole("button", { name: "Remove Queued" }));
      await user.click(screen.getByRole("button", { name: "Cancel" }));
      expect(column("Queued")).toBeInTheDocument();
      expect(localStorage.getItem(BOARD_STORAGE_KEY)).toBeNull();
    });

    it("offers the editor from the note about what is off the board", async () => {
      const user = userEvent.setup();
      renderBoard();
      await user.click(screen.getByRole("button", { name: "Edit columns" }));
      expect(screen.getByText("Board columns")).toBeInTheDocument();
    });
  });

  describe("selecting tasks", () => {
    it("selects one task from its card", async () => {
      const user = userEvent.setup();
      const { onToggleSelect, onOpenTask } = renderBoard();
      await user.click(screen.getByRole("checkbox", { name: "Select 1" }));
      expect(onToggleSelect).toHaveBeenCalledWith(1);
      // The checkbox is not a way into the task.
      expect(onOpenTask).not.toHaveBeenCalled();
    });

    it("selects a whole column at once", async () => {
      const user = userEvent.setup();
      const { onSelectAll } = renderBoard();
      await user.click(screen.getByRole("checkbox", { name: "Select every task in Needs you" }));
      expect(onSelectAll).toHaveBeenCalledWith([3], true);
    });

    it("offers no column checkbox for a column with nothing in it", () => {
      renderBoard();
      expect(screen.queryByRole("checkbox", { name: "Select every task in Proposed" })).not.toBeInTheDocument();
    });
  });

  describe("dragging to reorder", () => {
    const queued = [
      { id: 1, title: "First", state: "queued", capabilities: [], blocked: false },
      { id: 2, title: "Second", state: "queued", capabilities: [], blocked: false },
      { id: 3, title: "Third", state: "queued", capabilities: [], blocked: false },
      { id: 4, title: "Elsewhere", state: "running", capabilities: [], blocked: false },
    ];

    it("reorders the backlog between the two cards a drop landed between", () => {
      const onReorder = vi.fn();
      renderBoard({ tasks: queued, onReorder });
      fireEvent.dragStart(cardFor("Third"));
      fireEvent.dragOver(cardFor("Second"));
      fireEvent.drop(cardFor("Second"));
      expect(onReorder).toHaveBeenCalledWith([3], 1, 2);
    });

    it("drops at the end of a column", () => {
      const onReorder = vi.fn();
      renderBoard({ tasks: queued, onReorder });
      fireEvent.dragStart(cardFor("First"));
      fireEvent.dragOver(column("Queued").querySelector(".board-drop-end"));
      fireEvent.drop(column("Queued").querySelector(".board-drop-end"));
      expect(onReorder).toHaveBeenCalledWith([1], 3, null);
    });

    // A task's state is derived by the daemon, not set by the UI, so
    // there is no such thing as dropping a card into another column.
    it("refuses a drop into a different column", () => {
      const onReorder = vi.fn();
      renderBoard({ tasks: queued, onReorder });
      fireEvent.dragStart(cardFor("First"));
      fireEvent.drop(cardFor("Elsewhere"));
      expect(onReorder).not.toHaveBeenCalled();
    });

    it("drags the whole selection when a selected card is picked up", () => {
      const onReorder = vi.fn();
      renderBoard({ tasks: queued, onReorder, selected: new Set([1, 3]) });
      fireEvent.dragStart(cardFor("Third"));
      fireEvent.drop(cardFor("Second"));
      expect(onReorder).toHaveBeenCalledWith([1, 3], null, 2);
    });

    it("never starts a drag without an onReorder prop", () => {
      renderBoard({ tasks: queued });
      expect(cardFor("First")).toHaveAttribute("draggable", "false");
      expect(document.querySelector(".task-drag-handle")).not.toBeInTheDocument();
    });

    it("stops offering drags once a display-only sort is chosen", async () => {
      const user = userEvent.setup();
      renderBoard({ tasks: queued, onReorder: vi.fn() });
      expect(cardFor("First")).toHaveAttribute("draggable", "true");
      await user.click(screen.getByLabelText("Sort"));
      await user.click(screen.getByRole("option", { name: "Title (A–Z)" }));
      expect(cardFor("First")).toHaveAttribute("draggable", "false");
    });
  });
});
