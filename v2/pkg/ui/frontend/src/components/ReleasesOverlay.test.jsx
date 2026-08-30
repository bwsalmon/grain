import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import ReleasesOverlay from "./ReleasesOverlay.jsx";
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

function current(container) {
  return within(container.querySelector(".candidate-current"));
}

describe("ReleasesOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads the first repo from the configs list and shows its current candidate", async () => {
    api
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate]);
    const { container } = render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(current(container).getByText("v1.1.0-rc1")).toBeInTheDocument();
    expect(current(container).getByText(/active/)).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/release-configs");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/release-config");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates");
  });

  it("enables Promote but not Cut when the current candidate is still active", async () => {
    api
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate]);
    render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Promote current RC" })).toBeEnabled();
  });

  it("shows a note and no candidate history when the repo has no release config yet", async () => {
    api
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce({ configured: false, prodBranch: "", rcBranch: "", releaseBranchPrefix: "", majorVersion: 0 })
      .mockResolvedValueOnce([]);
    render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/has no release configuration yet/)).toBeInTheDocument();
    expect(screen.getByText("No release candidate cut yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
  });

  it("saves the release config form and reloads it", async () => {
    api
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ ...releaseConfig, prodBranch: "production" })
      .mockResolvedValueOnce([activeCandidate])
      .mockResolvedValueOnce([{ repo: "acme/widgets" }]);
    const user = userEvent.setup();
    render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
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
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([promotedCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate, promotedCandidate]);
    const user = userEvent.setup();
    const { container } = render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Cut new RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates", { method: "POST" });
    expect(await within(container.querySelector(".candidate-current")).findByText("v1.1.0-rc1")).toBeInTheDocument();
  });

  it("promotes the current RC when eligible", async () => {
    api
      .mockResolvedValueOnce([{ repo: "acme/widgets" }])
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([activeCandidate])
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce(releaseConfig)
      .mockResolvedValueOnce([{ ...activeCandidate, status: "promoted" }]);
    const user = userEvent.setup();
    const { container } = render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Promote current RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates/promote", { method: "POST" });
    expect(await current(container).findByText(/promoted/)).toBeInTheDocument();
  });

  it("shows no config form for a manually entered repo that isn't owner/name", async () => {
    api.mockResolvedValueOnce([]);
    const user = userEvent.setup();
    render(<ReleasesOverlay config={null} onClose={() => {}} showError={() => {}} />);
    await screen.findByRole("combobox");

    await user.type(screen.getByRole("combobox"), "notarepo");

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });
});
