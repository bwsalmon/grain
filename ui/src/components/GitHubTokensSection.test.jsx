import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import GitHubTokensSection from "./GitHubTokensSection.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const bot = {
  name: "bot",
  default: true,
  patterns: ["*"],
  present: true,
  offered: false,
  needsRestart: false,
};
const releaseBot = {
  name: "release-bot",
  capability: "github-credential:release-bot",
  present: true,
  offered: true,
  needsRestart: false,
};

const listing = (tokens, extra) => ({
  enabled: true,
  dir: "/data/secrets/github",
  tokens,
  patterns: [{ pattern: "*", credential: "bot" }],
  defaultName: "bot",
  restartRequired: false,
  ...extra,
});

describe("GitHubTokensSection", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("says so, and offers nothing to type into, without a credential directory", async () => {
    api.mockResolvedValueOnce({ enabled: false, tokens: [] });
    render(<GitHubTokensSection showError={() => {}} />);

    expect(
      await screen.findByText(/no local GitHub credential directory/i),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Token name")).not.toBeInTheDocument();
  });

  it("lists the default credential and each token's capability id", async () => {
    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    render(<GitHubTokensSection showError={() => {}} />);

    // "bot" is on screen twice now: as the credential itself, and as the
    // ladder entry naming it.
    expect((await screen.findAllByText("bot")).length).toBe(2);
    expect(screen.getByText("deployment default")).toBeInTheDocument();
    // The pattern that makes it the default, and the capability id a task
    // holds for the other one -- the two things an operator is choosing
    // between when they wonder which token a push will use. "*" is on
    // screen twice now: as this credential's own pattern chip, and as
    // the ladder entry below that can be repointed.
    expect(screen.getAllByText("*").length).toBeGreaterThan(0);
    expect(
      screen.getByText("github-credential:release-bot"),
    ).toBeInTheDocument();
  });

  it("stores a pasted token and clears the form", async () => {
    api.mockResolvedValueOnce(listing([bot]));
    const user = userEvent.setup();
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findAllByText("bot");

    api.mockResolvedValueOnce(
      listing([bot, { ...releaseBot, offered: false, needsRestart: true }], {
        restartRequired: true,
      }),
    );
    await user.type(screen.getByLabelText("Token name"), "release-bot");
    await user.type(screen.getByLabelText("Token"), "ghp-fake");
    await user.click(screen.getByRole("button", { name: "Save token" }));

    expect(api).toHaveBeenCalledWith("/api/github-tokens/release-bot", {
      method: "PUT",
      body: JSON.stringify({ value: "ghp-fake" }),
    });
    // The restart is the whole caveat of this pane: the token is on
    // disk, and the running daemon is still offering the capabilities it
    // started with.
    expect(await screen.findByText(/Restart the daemon/i)).toBeInTheDocument();
    expect(screen.getByText("restart needed")).toBeInTheDocument();
    // Write-only: the value does not stay on screen once stored.
    expect(screen.getByLabelText("Token")).toHaveValue("");
    expect(screen.getByLabelText("Token name")).toHaveValue("");
  });

  it("deletes a token nothing in the ladder names", async () => {
    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    const user = userEvent.setup();
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findByText("release-bot");

    api.mockResolvedValueOnce(
      listing([bot, { ...releaseBot, present: false, needsRestart: true }], {
        restartRequired: true,
      }),
    );
    await user.click(
      screen.getByRole("button", { name: "delete release-bot" }),
    );

    expect(api).toHaveBeenCalledWith("/api/github-tokens/release-bot", {
      method: "DELETE",
    });
    expect(await screen.findByText("no credential file")).toBeInTheDocument();
  });

  it("will not delete a credential a repo pattern still points at", async () => {
    api.mockResolvedValueOnce(listing([bot]));
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findAllByText("bot");

    // The credential's own delete button is disabled: removing it while
    // the ladder still names it would fail every push this deployment
    // makes, which is not a click away.
    const button = screen.getByRole("button", { name: "delete bot" });
    expect(button).toBeDisabled();
    await waitFor(() => expect(api).toHaveBeenCalledTimes(1));
  });

  // grain/task-4: the ladder itself. Without these controls, an operator
  // who has done everything this pane offers still has a deployment that
  // fails every clone, and nothing on screen says why.
  it("warns when nothing in the ladder covers a repo by default", async () => {
    api.mockResolvedValueOnce(
      listing([{ ...bot, default: false, patterns: [] }], {
        patterns: [],
        defaultName: "",
      }),
    );
    render(<GitHubTokensSection showError={() => {}} />);

    expect(
      await screen.findByText(/No default credential/i),
    ).toBeInTheDocument();
  });

  it("points a repo pattern at a credential", async () => {
    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    const user = userEvent.setup();
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findAllByText("bot");

    api.mockResolvedValueOnce(
      listing([bot, releaseBot], {
        patterns: [
          { pattern: "*", credential: "bot" },
          { pattern: "acme/widgets", credential: "release-bot" },
        ],
      }),
    );
    await user.type(screen.getByLabelText("Repo pattern"), "acme/widgets");
    await user.click(screen.getByLabelText("Credential"));
    await user.click(screen.getByRole("option", { name: "release-bot" }));
    await user.click(screen.getByRole("button", { name: "Save entry" }));

    expect(api).toHaveBeenLastCalledWith("/api/github-credential-patterns", {
      method: "PUT",
      body: JSON.stringify({
        pattern: "acme/widgets",
        credential: "release-bot",
      }),
    });
    expect(await screen.findByText("acme/widgets")).toBeInTheDocument();
    // No restart banner: a ladder change is live for the git proxy the
    // moment it is written.
    expect(screen.queryByText(/Restart the daemon/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Repo pattern")).toHaveValue("");
  });

  it("removes a ladder entry", async () => {
    api.mockResolvedValueOnce(
      listing([bot, releaseBot], {
        patterns: [
          { pattern: "*", credential: "bot" },
          { pattern: "acme/widgets", credential: "release-bot" },
        ],
      }),
    );
    const user = userEvent.setup();
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findByText("acme/widgets");

    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    await user.click(
      screen.getByRole("button", { name: "delete ladder entry acme/widgets" }),
    );

    expect(api).toHaveBeenLastCalledWith(
      "/api/github-credential-patterns?pattern=acme%2Fwidgets",
      { method: "DELETE" },
    );
    await waitFor(() =>
      expect(screen.queryByText("acme/widgets")).not.toBeInTheDocument(),
    );
  });

  // grain/task-16: the gap the ladder form exists to close, named where
  // it can be closed. /api/settings computes it; this section only shows
  // it, since /api/github-tokens knows nothing about target repos.
  it("names every target repo the ladder does not cover", async () => {
    api.mockResolvedValueOnce(listing([bot]));
    render(
      <GitHubTokensSection
        showError={() => {}}
        targetReposMissingCredentials={["acme/widgets", "other/thing"]}
      />,
    );

    expect(
      await screen.findByText(/Nothing in the ladder covers/i),
    ).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("other/thing")).toBeInTheDocument();
  });

  it("says nothing about uncovered repos when every one is covered", async () => {
    api.mockResolvedValueOnce(listing([bot]));
    render(
      <GitHubTokensSection
        showError={() => {}}
        targetReposMissingCredentials={[]}
      />,
    );
    await screen.findAllByText("bot");

    expect(
      screen.queryByText(/Nothing in the ladder covers/i),
    ).not.toBeInTheDocument();
  });

  // The list comes from settings, so the pane holding them has to be
  // told when a change here could have closed the gap it is showing --
  // otherwise the warning stays on screen naming a repo the entry just
  // added now covers.
  it("asks for a settings re-read after a ladder entry is added", async () => {
    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    const onLadderChanged = vi.fn();
    const user = userEvent.setup();
    render(
      <GitHubTokensSection
        showError={() => {}}
        targetReposMissingCredentials={["acme/widgets"]}
        onLadderChanged={onLadderChanged}
      />,
    );
    await screen.findAllByText("bot");

    api.mockResolvedValueOnce(
      listing([bot, releaseBot], {
        patterns: [
          { pattern: "*", credential: "bot" },
          { pattern: "acme/widgets", credential: "release-bot" },
        ],
      }),
    );
    await user.type(screen.getByLabelText("Repo pattern"), "acme/widgets");
    await user.click(screen.getByLabelText("Credential"));
    await user.click(screen.getByRole("option", { name: "release-bot" }));
    await user.click(screen.getByRole("button", { name: "Save entry" }));

    await waitFor(() => expect(onLadderChanged).toHaveBeenCalled());
  });

  it("does not ask for a settings re-read when the write failed", async () => {
    api.mockResolvedValueOnce(listing([bot, releaseBot]));
    const onLadderChanged = vi.fn();
    const showError = vi.fn();
    const user = userEvent.setup();
    render(
      <GitHubTokensSection
        showError={showError}
        onLadderChanged={onLadderChanged}
      />,
    );
    await screen.findAllByText("bot");

    api.mockRejectedValueOnce(new Error("unknown credential"));
    await user.type(screen.getByLabelText("Repo pattern"), "acme/widgets");
    await user.click(screen.getByLabelText("Credential"));
    await user.click(screen.getByRole("option", { name: "release-bot" }));
    await user.click(screen.getByRole("button", { name: "Save entry" }));

    await waitFor(() => expect(showError).toHaveBeenCalled());
    expect(onLadderChanged).not.toHaveBeenCalled();
  });

  it("flags a ladder entry whose credential is gone", async () => {
    api.mockResolvedValueOnce(
      listing([bot], {
        patterns: [
          { pattern: "*", credential: "bot" },
          { pattern: "acme/*", credential: "vanished", missing: true },
        ],
      }),
    );
    render(<GitHubTokensSection showError={() => {}} />);

    expect(await screen.findByText("no such credential")).toBeInTheDocument();
  });
});
