import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import RepoReleases from "./RepoReleases.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const unconfiguredQualificationPlan = {
  configured: false,
  repo: "acme/widgets",
  requireApproval: false,
  autoPromote: false,
  items: [],
};

const smokeTemplate = {
  id: "template-1",
  name: "Smoke test",
  title: "Smoke test",
  autoMerge: false,
  capabilities: [],
};
const unrelatedTemplate = {
  id: "template-2",
  name: "Unrelated",
  title: "Unrelated",
  autoMerge: false,
  capabilities: [],
};

const activeRelease = {
  repo: "acme/widgets",
  name: "myfeat",
  latestBranch: "myfeat.latest",
  prodBranch: "myfeat",
  status: "active",
};

// Distinct branches for the active vs. promoted candidate below -- a real
// cut always produces a fresh branch, and giving the two fixtures the
// same one only makes "current candidate" vs. "history" harder to tell
// apart in a rendered tree that shows the current one in both places.
const activeCandidate = {
  id: 2,
  release: "myfeat",
  branch: "myfeat.rc.2",
  status: "active",
};
const promotedCandidate = {
  id: 1,
  release: "myfeat",
  branch: "myfeat.rc.1",
  status: "promoted",
};

// setupApi wires a routing fake covering every endpoint RepoReleases
// touches, backed by mutable state, the same way App.test.jsx's own
// setupApi does -- unlike a finite chain of api.mockResolvedValueOnce
// calls, this keeps answering correctly no matter how many times the
// component's own poll (bwsalmon/agents#530) re-fetches in the
// background while a test is still running.
function setupApi({
  releases: releasesInit = [],
  candidates = [],
  qualificationPlan = unconfiguredQualificationPlan,
  qualificationRuns = {},
  nextCut = null,
} = {}) {
  let releasesState = [...releasesInit];
  let candidatesState = [...candidates];
  let qualificationPlanState = { ...qualificationPlan };
  let runsState = { ...qualificationRuns };

  api.mockImplementation((path, opts) => {
    const method = opts?.method || "GET";

    if (path === "/api/repos/acme/widgets/releases" && method === "POST") {
      const created = {
        repo: "acme/widgets",
        latestBranch: `${JSON.parse(opts.body).name}.latest`,
        prodBranch: JSON.parse(opts.body).name,
        status: "provisioning",
        name: JSON.parse(opts.body).name,
      };
      releasesState = [...releasesState, created];
      return Promise.resolve(created);
    }
    if (path === "/api/repos/acme/widgets/releases" && method === "GET") {
      return Promise.resolve(releasesState);
    }
    if (
      /^\/api\/repos\/acme\/widgets\/releases\/[^/]+\/candidates\/promote$/.test(
        path,
      ) &&
      method === "POST"
    ) {
      candidatesState = candidatesState.map((c, i) =>
        i === 0 ? { ...c, status: "promoted" } : c,
      );
      return Promise.resolve(candidatesState[0]);
    }
    if (
      /^\/api\/repos\/acme\/widgets\/releases\/[^/]+\/candidates$/.test(path)
    ) {
      if (method === "POST") {
        if (nextCut) candidatesState = [nextCut, ...candidatesState];
        return Promise.resolve(candidatesState[0]);
      }
      return Promise.resolve(candidatesState);
    }
    if (
      /^\/api\/repos\/acme\/widgets\/releases\/[^/]+\/merge$/.test(path) &&
      method === "POST"
    ) {
      releasesState = releasesState.map((r) => ({
        ...r,
        status: "merged",
        pullRequestUrl: "https://example/pull/1",
      }));
      return Promise.resolve(releasesState[0]);
    }
    const approveMatch = path.match(
      /^\/api\/repos\/[^/]+\/[^/]+\/candidates\/([^/]+)\/qualification\/approve$/,
    );
    if (approveMatch && method === "POST") {
      const run = runsState[approveMatch[1]];
      if (run) {
        runsState = {
          ...runsState,
          [approveMatch[1]]: {
            ...run,
            status: "running",
            tasks: run.tasks.map((t) => ({ ...t, approved: true })),
          },
        };
      }
      return Promise.resolve(runsState[approveMatch[1]] || null);
    }
    const runMatch = path.match(
      /^\/api\/repos\/[^/]+\/[^/]+\/candidates\/([^/]+)\/qualification$/,
    );
    if (runMatch && method === "GET") {
      return Promise.resolve(runsState[runMatch[1]] || null);
    }
    if (/^\/api\/repos\/[^/]+\/[^/]+\/qualification-plan$/.test(path)) {
      if (method === "PUT") {
        qualificationPlanState = {
          ...qualificationPlanState,
          ...JSON.parse(opts.body),
        };
      }
      return Promise.resolve(qualificationPlanState);
    }
    return Promise.resolve(null);
  });

  return {
    get candidatesState() {
      return candidatesState;
    },
  };
}

function current() {
  return within(document.body.querySelector(".candidate-current"));
}

describe("RepoReleases", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("loads the given repo's releases and shows the current candidate", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(
      screen.getByRole("heading", { name: "acme/widgets releases" }),
    ).toBeInTheDocument();
    expect(current().getByText("myfeat.rc.2")).toBeInTheDocument();
    expect(current().getByText(/active/)).toBeInTheDocument();
    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/releases");
    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/releases/myfeat/candidates",
    );
    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/qualification-plan",
    );
    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/candidates/2/qualification",
    );
  });

  it("enables Promote but not Cut when the current candidate is still active", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    expect(screen.getByRole("button", { name: "Cut new RC" })).toBeDisabled();
    expect(
      screen.getByRole("button", { name: "Promote current RC" }),
    ).toBeEnabled();
  });

  it("shows a note and no release picker when the repo has no releases yet", async () => {
    setupApi({ releases: [], candidates: [] });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );

    expect(await screen.findByText(/has no releases yet/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Release")).not.toBeInTheDocument();
  });

  it("creates a new release from the form", async () => {
    setupApi({ releases: [] });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByLabelText(/New release name/);

    await user.type(screen.getByLabelText(/New release name/), "myfeat");
    await user.click(screen.getByRole("button", { name: "Create release" }));

    expect(api).toHaveBeenCalledWith("/api/repos/acme/widgets/releases", {
      method: "POST",
      body: JSON.stringify({ name: "myfeat" }),
    });
    await screen.findByLabelText("Release");
  });

  it("cuts a new RC when eligible", async () => {
    setupApi({
      releases: [activeRelease],
      candidates: [promotedCandidate],
      nextCut: activeCandidate,
    });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "Cut new RC" }));

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/releases/myfeat/candidates",
      { method: "POST" },
    );
    expect(await current().findByText("myfeat.rc.2")).toBeInTheDocument();
  });

  it("promotes the current RC when eligible", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(
      screen.getByRole("button", { name: "Promote current RC" }),
    );

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/releases/myfeat/candidates/promote",
      { method: "POST" },
    );
    expect(await current().findByText(/promoted/)).toBeInTheDocument();
  });

  it("requests a merge back to the default branch", async () => {
    setupApi({ releases: [activeRelease], candidates: [promotedCandidate] });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", {
      name: "Merge myfeat into the default branch",
    });

    await user.click(
      screen.getByRole("button", {
        name: "Merge myfeat into the default branch",
      }),
    );

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/releases/myfeat/merge",
      { method: "POST" },
    );
    expect(await screen.findByText(/Merge requested/)).toBeInTheDocument();
  });

  // A release whose prod branch carried nothing the default branch did
  // not already have merges without a pull request at all, and says why
  // -- otherwise this pane showed a bare "Merge requested." next to a
  // link that was never coming.
  it("says why a merged release has no pull request when there was nothing to merge back", async () => {
    const settled = {
      ...activeRelease,
      status: "merged",
      mergeNote:
        "myfeat carried no commits main did not already have, so GitHub had no pull request to open.",
    };
    setupApi({ releases: [settled], candidates: [promotedCandidate] });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );

    expect(
      await screen.findByText(/Nothing to merge back/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/carried no commits main did not already have/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Merge requested/)).not.toBeInTheDocument();
  });

  // The back button names the repo, not the repo list: this pane opens
  // from that repo's own page now (grain/task-111), which is one step
  // up rather than two.
  it("calls onBack when the back button is clicked", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    const onBack = vi.fn();
    const user = userEvent.setup();
    render(
      <RepoReleases repo="acme/widgets" onBack={onBack} showError={() => {}} />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    await user.click(screen.getByRole("button", { name: "← acme/widgets" }));

    expect(onBack).toHaveBeenCalledTimes(1);
  });

  it("shows a pending-approval banner and approves the run's tasks in bulk", async () => {
    const run = {
      id: 5,
      candidateId: 2,
      createdAt: "2026-08-27T12:00:00Z",
      status: "pending_approval",
      tasks: [
        {
          taskId: "10",
          templateId: "template-1",
          templateName: "Smoke test",
          instanceIndex: 1,
          repeat: 1,
          approved: false,
          state: "proposed",
        },
      ],
    };
    setupApi({
      releases: [activeRelease],
      candidates: [activeCandidate],
      qualificationRuns: { 2: run },
    });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByText("Smoke test");

    expect(
      screen.getByText(/need approval before any of them can run/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Approve all" }));

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/candidates/2/qualification/approve",
      { method: "POST" },
    );
    expect(await screen.findByText("Running")).toBeInTheDocument();
  });

  it("shows a ready-to-promote banner once qualification succeeds", async () => {
    const run = {
      id: 5,
      candidateId: 2,
      createdAt: "2026-08-27T12:00:00Z",
      status: "succeeded",
      tasks: [
        {
          taskId: "10",
          templateId: "template-1",
          templateName: "Smoke test",
          instanceIndex: 1,
          repeat: 1,
          approved: true,
          state: "completed",
        },
      ],
    };
    setupApi({
      releases: [activeRelease],
      candidates: [activeCandidate],
      qualificationRuns: { 2: run },
    });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );

    expect(await screen.findByText(/ready to promote/)).toBeInTheDocument();
  });

  it("shows a failed banner, with the failing task's own badge, when qualification fails", async () => {
    const run = {
      id: 5,
      candidateId: 2,
      createdAt: "2026-08-27T12:00:00Z",
      status: "failed",
      tasks: [
        {
          taskId: "10",
          templateId: "template-1",
          templateName: "Smoke test",
          instanceIndex: 1,
          repeat: 2,
          approved: true,
          state: "failed",
        },
        {
          taskId: "11",
          templateId: "template-1",
          templateName: "Smoke test",
          instanceIndex: 2,
          repeat: 2,
          approved: true,
          state: "completed",
        },
      ],
    };
    setupApi({
      releases: [activeRelease],
      candidates: [activeCandidate],
      qualificationRuns: { 2: run },
    });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );

    expect(await screen.findByText(/Qualification failed/)).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    // The failed instance's own "1/2" -- the ui.QualificationRun's own
    // failures-first ordering already sorted it first among the two.
    expect(screen.getByText("(1/2)")).toBeInTheDocument();
  });

  it("offers every template, and saves a new qualification item", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    const user = userEvent.setup();
    render(
      <RepoReleases
        repo="acme/widgets"
        templates={[smokeTemplate, unrelatedTemplate]}
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Add item" });

    await user.click(screen.getByRole("button", { name: "Add item" }));
    await user.click(screen.getByLabelText("Template"));
    expect(
      screen.getByRole("option", { name: "Smoke test" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("option", { name: "Unrelated" }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("option", { name: "Smoke test" }));
    await user.click(
      screen.getByRole("button", { name: "Save qualification plan" }),
    );

    expect(api).toHaveBeenCalledWith(
      "/api/repos/acme/widgets/qualification-plan",
      {
        method: "PUT",
        body: JSON.stringify({
          requireApproval: false,
          autoPromote: false,
          items: [{ templateId: "template-1", repeat: 1, dependsOn: [] }],
        }),
      },
    );
  });

  it("polls the candidate and qualification state on an interval", async () => {
    setupApi({ releases: [activeRelease], candidates: [activeCandidate] });
    render(
      <RepoReleases
        repo="acme/widgets"
        onBack={() => {}}
        showError={() => {}}
      />,
    );
    await screen.findByRole("button", { name: "Promote current RC" });

    const callsBefore = api.mock.calls.filter(
      (c) => c[0] === "/api/repos/acme/widgets/releases/myfeat/candidates",
    ).length;

    await waitFor(
      () => {
        const callsAfter = api.mock.calls.filter(
          (c) => c[0] === "/api/repos/acme/widgets/releases/myfeat/candidates",
        ).length;
        expect(callsAfter).toBeGreaterThan(callsBefore);
      },
      { timeout: 4000 },
    );
  }, 6000);
});
