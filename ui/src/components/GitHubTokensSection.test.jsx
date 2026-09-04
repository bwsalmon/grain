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

    expect(await screen.findByText("bot")).toBeInTheDocument();
    expect(screen.getByText("deployment default")).toBeInTheDocument();
    // The pattern that makes it the default, and the capability id a task
    // holds for the other one -- the two things an operator is choosing
    // between when they wonder which token a push will use.
    expect(screen.getByText("*")).toBeInTheDocument();
    expect(
      screen.getByText("github-credential:release-bot"),
    ).toBeInTheDocument();
  });

  it("stores a pasted token and clears the form", async () => {
    api.mockResolvedValueOnce(listing([bot]));
    const user = userEvent.setup();
    render(<GitHubTokensSection showError={() => {}} />);
    await screen.findByText("bot");

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
    await screen.findByText("bot");

    // The only delete button on screen is the default credential's, and
    // it is disabled: removing it would fail every push this deployment
    // makes, which is not a click away.
    const button = screen.getByRole("button", { name: "delete bot" });
    expect(button).toBeDisabled();
    await waitFor(() => expect(api).toHaveBeenCalledTimes(1));
  });
});
