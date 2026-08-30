import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import RepoList from "./RepoList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const tasks = [
  { id: "1", repo: "acme/widgets", state: "queued", blocked: false },
  { id: "2", repo: "acme/widgets", state: "running", blocked: false },
  { id: "3", repo: "acme/widgets", state: "queued", blocked: true },
  { id: "4", repo: "acme/gadgets", state: "completed", blocked: false },
  { id: "5", repo: "", state: "proposed", blocked: false },
];

describe("RepoList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists one row per repo with per-state counts and a total", () => {
    render(<RepoList tasks={tasks} config={null} onOpenRepo={() => {}} onOpenReleases={() => {}} />);

    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("acme/gadgets")).toBeInTheDocument();
    expect(screen.getByText("Queued 2")).toBeInTheDocument();
    expect(screen.getByText("Running 1")).toBeInTheDocument();
    expect(screen.getByText("Blocked 1")).toBeInTheDocument();
    expect(screen.getByText("3 tasks")).toBeInTheDocument();
    expect(screen.getByText("1 task")).toBeInTheDocument();
  });

  it("omits tasks with no target repo", () => {
    render(<RepoList tasks={tasks} config={null} onOpenRepo={() => {}} onOpenReleases={() => {}} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("also lists a targetRepos entry that has no tasks yet", () => {
    const config = { targetRepos: ["acme/widgets", "acme/newrepo"] };
    render(<RepoList tasks={tasks} config={config} onOpenRepo={() => {}} onOpenReleases={() => {}} />);

    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("acme/newrepo")).toBeInTheDocument();
    expect(screen.getByText("0 tasks")).toBeInTheDocument();
  });

  it("calls onOpenRepo when a row is clicked", async () => {
    const onOpenRepo = vi.fn();
    const user = userEvent.setup();
    render(<RepoList tasks={tasks} config={null} onOpenRepo={onOpenRepo} onOpenReleases={() => {}} />);

    await user.click(screen.getByText("acme/gadgets"));

    expect(onOpenRepo).toHaveBeenCalledWith("acme/gadgets");
  });

  it("calls onOpenReleases, not onOpenRepo, when a row's Releases button is clicked", async () => {
    const onOpenRepo = vi.fn();
    const onOpenReleases = vi.fn();
    const user = userEvent.setup();
    render(<RepoList tasks={tasks} config={null} onOpenRepo={onOpenRepo} onOpenReleases={onOpenReleases} />);

    const row = screen.getByText("acme/gadgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Releases" }));

    expect(onOpenReleases).toHaveBeenCalledWith("acme/gadgets");
    expect(onOpenRepo).not.toHaveBeenCalled();
  });

  it("shows an empty message when there are no known repos", () => {
    render(<RepoList tasks={[]} config={null} onOpenRepo={() => {}} onOpenReleases={() => {}} />);
    expect(screen.getByText("No repos yet -- add one above, or file a task with a target repo.")).toBeInTheDocument();
  });

  it("adds a repo and refreshes config on success", async () => {
    api.mockResolvedValueOnce({});
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    render(<RepoList tasks={[]} config={null} onOpenRepo={() => {}} onOpenReleases={() => {}} onRefreshConfig={onRefreshConfig} showError={() => {}} />);

    await user.type(screen.getByPlaceholderText("owner/name"), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(api).toHaveBeenCalledWith("/api/repos", { method: "POST", body: JSON.stringify({ repo: "acme/widgets" }) });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
  });

  it("reports the error and does not refresh config when adding fails", async () => {
    api.mockRejectedValueOnce(new Error("repo must be owner/name"));
    const onRefreshConfig = vi.fn();
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<RepoList tasks={[]} config={null} onOpenRepo={() => {}} onOpenReleases={() => {}} onRefreshConfig={onRefreshConfig} showError={showError} />);

    await user.type(screen.getByPlaceholderText("owner/name"), "not-a-repo");
    await user.click(screen.getByRole("button", { name: "Add repo" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "repo must be owner/name" }));
    expect(onRefreshConfig).not.toHaveBeenCalled();
  });

  it("only offers Remove on a row that is in config.targetRepos", () => {
    const config = { targetRepos: ["acme/widgets"] };
    render(<RepoList tasks={tasks} config={config} onOpenRepo={() => {}} onOpenReleases={() => {}} />);

    const widgetsRow = screen.getByText("acme/widgets").closest("li");
    const gadgetsRow = screen.getByText("acme/gadgets").closest("li");
    expect(within(widgetsRow).getByRole("button", { name: "Remove" })).toBeInTheDocument();
    expect(within(gadgetsRow).queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
  });

  it("removes a repo after confirmation, without also opening it", async () => {
    api.mockResolvedValueOnce({});
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    const onOpenRepo = vi.fn();
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    const config = { targetRepos: ["acme/widgets"] };
    render(<RepoList tasks={tasks} config={config} onOpenRepo={onOpenRepo} onOpenReleases={() => {}} onRefreshConfig={onRefreshConfig} showError={() => {}} />);

    const row = screen.getByText("acme/widgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets", { method: "DELETE" });
    expect(onRefreshConfig).toHaveBeenCalledTimes(1);
    expect(onOpenRepo).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("does not remove a repo when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(false));
    const onRefreshConfig = vi.fn();
    const user = userEvent.setup();
    const config = { targetRepos: ["acme/widgets"] };
    render(<RepoList tasks={tasks} config={config} onOpenRepo={() => {}} onOpenReleases={() => {}} onRefreshConfig={onRefreshConfig} showError={() => {}} />);

    const row = screen.getByText("acme/widgets").closest("li");
    await user.click(within(row).getByRole("button", { name: "Remove" }));

    expect(api).not.toHaveBeenCalled();
    expect(onRefreshConfig).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});
