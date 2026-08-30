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

const unconfiguredQualificationPlan = {
  configured: false, repo: "acme/widgets", requireApproval: false, autoPromote: false, items: [],
};

const smokeTemplate = { id: "template-1", name: "Smoke test", title: "Smoke test", repo: "acme/widgets", autoMerge: false, capabilities: [] };
const otherRepoTemplate = { id: "template-2", name: "Unrelated", title: "Unrelated", repo: "acme/other", autoMerge: false, capabilities: [] };

// Distinct labels for the active vs. promoted candidate below -- a real
// cut always produces a fresh label, and giving the two fixtures the
// same one only makes "current candidate" vs. "history" harder to tell
// apart in a rendered tree that shows the current one in both places.
const activeCandidate = { id: 2, label: "v1.1.0-rc1", status: "active", branch: "rc", releaseBranch: "" };
const promotedCandidate = { id: 1, label: "v1.0.0-rc1", status: "promoted", branch: "rc", releaseBranch: "release/v1" };

// queueRefresh queues exactly the sequence of api() calls RepoReleases'
// own refresh() makes: release-config, candidates and the qualification
// plan concurrently (Promise.all, so in that array order), then --
// only when there is a current candidate -- its qualification run.
function queueRefresh(cfg, candidates, plan = unconfiguredQualificationPlan, run = null) {
  api.mockResolvedValueOnce(cfg).mockResolvedValueOnce(candidates).mockResolvedValueOnce(plan);
  if (candidates.length > 0) {
    api.mockResolvedValueOnce(run);
  }
}

function current() {
  return within(document.body.querySelector(".candidate-current"));
}

describe("RepoReleases", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads the given repo's config and shows its current candidate", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("heading", { name: "acme/widgets releases" })).toBeInTheDocument();
    expect(current().getByText("v1.1.0-rc1")).toBeInTheDocument();
    expect(current().getByText(/active/)).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/release-config");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/qualification-plan");
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates/2/qualification");
  });

  it("enables Promote but not Cut when the current candidate is still active", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Promote current RC" })).toBeEnabled();
  });

  it("shows a note and no candidate history when the repo has no release config yet", async () => {
    queueRefresh({ configured: false, prodBranch: "", rcBranch: "", releaseBranchPrefix: "", majorVersion: 0 }, []);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/has no release configuration yet/)).toBeInTheDocument();
    expect(screen.getByText("No release candidate cut yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
  });

  it("saves the release config form and reloads it", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    api.mockResolvedValueOnce({});
    queueRefresh({ ...releaseConfig, prodBranch: "production" }, [activeCandidate]);
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
    queueRefresh(releaseConfig, [promotedCandidate]);
    api.mockResolvedValueOnce({});
    queueRefresh(releaseConfig, [activeCandidate, promotedCandidate], unconfiguredQualificationPlan, null);
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Cut new RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates", { method: "POST" });
    expect(await current().findByText("v1.1.0-rc1")).toBeInTheDocument();
  });

  it("promotes the current RC when eligible", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    api.mockResolvedValueOnce({});
    queueRefresh(releaseConfig, [{ ...activeCandidate, status: "promoted" }]);
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Promote current RC" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates/promote", { method: "POST" });
    expect(await current().findByText(/promoted/)).toBeInTheDocument();
  });

  it("calls onBack when the back button is clicked", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    const onBack = vi.fn();
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={onBack} showError={() => {}} />);
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: /Repos/ }));

    expect(onBack).toHaveBeenCalledTimes(1);
  });

  it("shows a pending-approval banner and approves the run's tasks in bulk", async () => {
    const run = {
      id: 5, candidateId: 2, createdAt: "2026-08-27T12:00:00Z", status: "pending_approval",
      tasks: [{ taskId: "10", templateId: "template-1", templateName: "Smoke test", instanceIndex: 1, repeat: 1, approved: false, state: "proposed" }],
    };
    queueRefresh(releaseConfig, [activeCandidate], unconfiguredQualificationPlan, run);
    api.mockResolvedValueOnce({});
    queueRefresh(releaseConfig, [activeCandidate], unconfiguredQualificationPlan, { ...run, status: "running", tasks: [{ ...run.tasks[0], approved: true, state: "queued" }] });
    const user = userEvent.setup();
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);
    await screen.findByText("Smoke test");

    expect(screen.getByText(/need approval before any of them can run/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Approve all" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/candidates/2/qualification/approve", { method: "POST" });
    expect(await screen.findByText("Running")).toBeInTheDocument();
  });

  it("shows a ready-to-promote banner once qualification succeeds", async () => {
    const run = {
      id: 5, candidateId: 2, createdAt: "2026-08-27T12:00:00Z", status: "succeeded",
      tasks: [{ taskId: "10", templateId: "template-1", templateName: "Smoke test", instanceIndex: 1, repeat: 1, approved: true, state: "completed" }],
    };
    queueRefresh(releaseConfig, [activeCandidate], unconfiguredQualificationPlan, run);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/ready to promote/)).toBeInTheDocument();
  });

  it("shows a failed banner, with the failing task's own badge, when qualification fails", async () => {
    const run = {
      id: 5, candidateId: 2, createdAt: "2026-08-27T12:00:00Z", status: "failed",
      tasks: [
        { taskId: "10", templateId: "template-1", templateName: "Smoke test", instanceIndex: 1, repeat: 2, approved: true, state: "failed" },
        { taskId: "11", templateId: "template-1", templateName: "Smoke test", instanceIndex: 2, repeat: 2, approved: true, state: "completed" },
      ],
    };
    queueRefresh(releaseConfig, [activeCandidate], unconfiguredQualificationPlan, run);
    render(<RepoReleases repo="acme/widgets" onBack={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/Qualification failed/)).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    // The failed instance's own "1/2" -- the ui.QualificationRun's own
    // failures-first ordering already sorted it first among the two.
    expect(screen.getByText("(1/2)")).toBeInTheDocument();
  });

  it("only offers templates that target this repo, and saves a new qualification item", async () => {
    queueRefresh(releaseConfig, [activeCandidate]);
    api.mockResolvedValueOnce({});
    queueRefresh(releaseConfig, [activeCandidate]);
    const user = userEvent.setup();
    render(
      <RepoReleases repo="acme/widgets" templates={[smokeTemplate, otherRepoTemplate]} onBack={() => {}} showError={() => {}} />
    );
    await screen.findByRole("button", { name: "Add item" });

    await user.click(screen.getByRole("button", { name: "Add item" }));
    await user.click(screen.getByLabelText("Template"));
    expect(screen.getByRole("option", { name: "Smoke test" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Unrelated" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("option", { name: "Smoke test" }));
    await user.click(screen.getByRole("button", { name: "Save qualification plan" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/qualification-plan", {
      method: "PUT",
      body: JSON.stringify({
        requireApproval: false,
        autoPromote: false,
        items: [{ templateId: "template-1", repeat: 1, dependsOn: [] }],
      }),
    });
  });
});
