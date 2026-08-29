import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import RepoList from "./RepoList.jsx";

const tasks = [
  { id: "1", repo: "acme/widgets", state: "queued", blocked: false },
  { id: "2", repo: "acme/widgets", state: "running", blocked: false },
  { id: "3", repo: "acme/widgets", state: "queued", blocked: true },
  { id: "4", repo: "acme/gadgets", state: "completed", blocked: false },
  { id: "5", repo: "", state: "proposed", blocked: false },
];

describe("RepoList", () => {
  it("lists one row per repo with per-state counts and a total", () => {
    render(<RepoList tasks={tasks} onOpenRepo={() => {}} />);

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();
    expect(screen.getByText("Queued 2")).toBeInTheDocument();
    expect(screen.getByText("Running 1")).toBeInTheDocument();
    expect(screen.getByText("Blocked 1")).toBeInTheDocument();
    expect(screen.getByText("3 tasks")).toBeInTheDocument();
    expect(screen.getByText("1 task")).toBeInTheDocument();
  });

  it("omits tasks with no target repo", () => {
    render(<RepoList tasks={tasks} onOpenRepo={() => {}} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("calls onOpenRepo when a row is clicked", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    render(<RepoList tasks={tasks} onOpenRepo={onOpenRepo} />);

    await user.click(screen.getByText("acme/gadgets"));

    expect(onOpenRepo).toHaveBeenCalledWith("acme/gadgets");
  });

  it("shows an empty message when there are no repo-targeted tasks", () => {
    render(<RepoList tasks={[]} onOpenRepo={() => {}} />);
    expect(screen.getByText("No repos yet -- tasks with a target repo will show up here.")).toBeInTheDocument();
  });
});
