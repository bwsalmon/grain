import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import LogsPage from "./LogsPage.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("LogsPage", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("shows a note instead of a source picker when no log sources are configured", async () => {
    api.mockResolvedValueOnce({ enabled: false });
    render(<LogsPage showError={() => {}} />);

    expect(await screen.findByText(/not available/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Source/)).not.toBeInTheDocument();
  });

  it("fetches and shows the first source's log lines", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sources: ["daemon", "git-proxy-audit"] })
      .mockResolvedValueOnce({ lines: ["one", "two"] });
    render(<LogsPage showError={() => {}} />);

    expect(await screen.findByText(/one/)).toBeInTheDocument();
    expect(screen.getByText(/two/)).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/logs/daemon?lines=500");
  });

  it("re-fetches the selected source when Refresh is clicked", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sources: ["daemon"] })
      .mockResolvedValueOnce({ lines: ["first"] })
      .mockResolvedValueOnce({ lines: ["first", "second"] });
    const user = userEvent.setup();
    render(<LogsPage showError={() => {}} />);

    expect(await screen.findByText(/first/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(await screen.findByText(/second/)).toBeInTheDocument();
  });

  it("switches sources from the picker and re-fetches", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sources: ["daemon", "git-proxy-audit"] })
      .mockResolvedValueOnce({ lines: ["daemon line"] })
      .mockResolvedValueOnce({ lines: ["audit line"] });
    const user = userEvent.setup();
    render(<LogsPage showError={() => {}} />);

    expect(await screen.findByText(/daemon line/)).toBeInTheDocument();

    await user.click(screen.getByLabelText(/Source/));
    await user.click(await screen.findByRole("option", { name: "git-proxy-audit" }));

    expect(await screen.findByText(/audit line/)).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/logs/git-proxy-audit?lines=500");
  });

  it("shows a placeholder when the source has no lines", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, sources: ["daemon"] })
      .mockResolvedValueOnce({ lines: [] });
    render(<LogsPage showError={() => {}} />);

    expect(await screen.findByText("(no log lines)")).toBeInTheDocument();
  });
});
