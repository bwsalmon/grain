import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import RepoReleases from "./RepoReleases.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const releaseConfig = {
  configured: true,
  prodBranch: "main",
  rcBranch: "rc",
  releaseBranchPrefix: "release/",
  majorVersion: 1,
};

// Distinct labels for the active vs. promoted candidate below -- a real
// cut always produces a fresh label, and giving the two fixtures the
// same one only makes "current candidate" vs. "history" harder to tell
// apart in a rendered tree that shows the current one in both places.
const activeCandidate = { id: "c2", label: "v1.1.0-rc1", status: "active", branch: "rc", releaseBranch: "" };
const promotedCandidate = { id: "c1", label: "v1.0.0-rc1", status: "promoted", branch: "rc", releaseBranch: "release/v1" };

function current() {
  return within(document.body.querySelector(".candidate-current"));
}

describe("RepoReleases", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads the given repo's config and shows its current candidate", async () => {
    api.mockResolvedValueOnce(releaseConfig).mockResolvedValueOnce([activeCandidate]);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("heading", { name: "acme/widgets releases" })).toBeInTheDocument();
    expect(current().getByText("v1.1.0-rc1")).toBeInTheDocument();
    expect(current().getByText(/active/)).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/release-config");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates");
  });

  it("enables Promote but not Cut when the current candidate is still active", async () => {
    api.mockResolvedValueOnce(releaseConfig).mockResolvedValueOnce([activeCandidate]);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Promote current RC" })).toBeEnabled();
  });

  it("shows a note and no candidate history when the repo has no release config yet", async () => {
    api
      .mockResolvedValueOnce({ configured: false, prodBranch: "", rcBranch: "", releaseBranchPrefix: "", majorVersion: 0 })
      .mockResolvedValueOnce([]);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/has no release configuration yet/)).toBeInTheDocument();
    expect(screen.getByText("No release candidate cut yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
  });

  it("saves the release config form and reloads it", async () => {
    api
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ ...releaseConfig, prodBranch: "production" })
      .mockResolvedValueOnce([activeCandidate]);
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByLabelText(/Prod branch/);

    const prodInput = screen.getByLabelText(/Prod branch/);
    await user.clear(prodInput);
    await user.type(prodInput, "production");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/release-config", {
      method: "PUT",
      body: JSON.stringify({ prodBranch: "production", rcBranch: "rc", releaseBranchPrefix: "release/", majorVersion: 1 }),
    });
  });

  it("cuts a new RC when eligible", async () => {
    api
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([promotedCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate, promotedCandidate]);
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Cut new RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates", { method: "POST" });
    expect(await current().findByText("v1.1.0-rc1")).toBeInTheDocument();
  });

  it("promotes the current RC when eligible", async () => {
    api
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([{ ...activeCandidate, status: "promoted" }]);
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Promote current RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates/promote", { method: "POST" });
    expect(await current().findByText(/promoted/)).toBeInTheDocument();
  });

  it("calls onBack when the back button is clicked", async () => {
    api.mockResolvedValueOnce(releaseConfig).mockResolvedValueOnce([activeCandidate]);
    const onBack = vi.fn();
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={onBack} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: /Repos/ }));

    expect(onBack).toHaveBeenCalledTimes(1);
  });
});
