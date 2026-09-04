import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import StateRepoPanel from "./StateRepoPanel.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const local = {
  available: true,
  mode: "local",
  branch: "main",
  dir: "/var/lib/grain/state-repo",
  head: "abcdef1234567890",
  schemaVersion: 16,
  buildSchemaVersion: 16,
  secretsPublicKey: "grain-secret-pub-v1:AAAA",
  secretsKeyFile: "/var/lib/grain/secrets/secrets.key",
};

describe("StateRepoPanel", () => {
  afterEach(() => api.mockReset());

  it("says where state lives, and which key file to back up", async () => {
    api.mockResolvedValueOnce(local);
    render(<StateRepoPanel showError={() => {}} />);

    expect(await screen.findByText("local only")).toBeInTheDocument();
    expect(
      screen.getByText(/\/var\/lib\/grain\/state-repo/),
    ).toBeInTheDocument();
    // The public key is shown -- it is how an operator checks the key they
    // hold is the one this deployment encrypts to -- and the private half
    // is named as a path, never rendered.
    expect(screen.getByText("grain-secret-pub-v1:AAAA")).toBeInTheDocument();
    expect(
      screen.getByText(/\/var\/lib\/grain\/secrets\/secrets.key/),
    ).toBeInTheDocument();
  });

  it("says so, and offers nothing, when this UI manages no repository", async () => {
    api.mockResolvedValueOnce({ available: false });
    render(<StateRepoPanel showError={() => {}} />);

    expect(
      await screen.findByText(/not running inside a daemon/i),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Repository URL")).not.toBeInTheDocument();
  });

  it("adopts a repository, and does not keep the pasted token", async () => {
    api.mockResolvedValueOnce(local);
    const user = userEvent.setup();
    render(<StateRepoPanel showError={() => {}} />);
    await screen.findByText("local only");

    await user.type(
      screen.getByLabelText("Repository URL"),
      "https://github.com/owner/grain-state.git",
    );
    await user.type(screen.getByLabelText(/Push token/), "ghp_x");
    await user.type(
      screen.getByLabelText(/Secrets key/),
      "grain-secret-key-v1:AAAA",
    );
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://github.com/owner/grain-state.git",
    });
    await user.click(screen.getByRole("button", { name: "Adopt repository" }));

    await waitFor(() =>
      expect(api).toHaveBeenLastCalledWith("/api/state-repo", {
        method: "POST",
        body: JSON.stringify({
          mode: "remote",
          remote: "https://github.com/owner/grain-state.git",
          branch: "main",
          token: "ghp_x",
          secretsKey: "grain-secret-key-v1:AAAA",
        }),
      }),
    );
    expect(
      await screen.findByText("https://github.com/owner/grain-state.git"),
    ).toBeInTheDocument();
    // Neither pasted credential is kept in the form once the daemon has it.
    expect(screen.getByLabelText(/Push token/)).toHaveValue("");
    expect(screen.getByLabelText(/Secrets key/)).toHaveValue("");
  });

  it("says when this host's secrets file is sealed to a key it lacks", async () => {
    api.mockResolvedValueOnce({
      ...local,
      secretsError: "secrets: this file is encrypted to a different key",
      secretsFileRecipient: "grain-secret-pub-v1:BBBB",
    });
    render(<StateRepoPanel showError={() => {}} />);

    expect(
      await screen.findByText(/cannot read this host's secrets file/i),
    ).toBeInTheDocument();
    // Which key it wants is the question the operator has to answer, so
    // the pane answers it rather than leaving them to guess.
    expect(screen.getByText("grain-secret-pub-v1:BBBB")).toBeInTheDocument();
  });

  it("imports a private key, and does not keep it", async () => {
    api.mockResolvedValueOnce({
      ...local,
      secretsError: "secrets: this file is encrypted to a different key",
      secretsFileRecipient: "grain-secret-pub-v1:BBBB",
    });
    const user = userEvent.setup();
    render(<StateRepoPanel showError={() => {}} />);
    await screen.findByText(/cannot read this host's secrets file/i);

    await user.type(
      screen.getByLabelText(/Import a private key/),
      "grain-secret-key-v1:BBBB",
    );
    api.mockResolvedValueOnce({
      ...local,
      secretsPublicKey: "grain-secret-pub-v1:BBBB",
    });
    await user.click(screen.getByRole("button", { name: "Import key" }));

    await waitFor(() =>
      expect(api).toHaveBeenLastCalledWith("/api/state-repo/secrets-key", {
        method: "POST",
        body: JSON.stringify({ key: "grain-secret-key-v1:BBBB" }),
      }),
    );
    expect(screen.getByLabelText(/Import a private key/)).toHaveValue("");
    expect(
      screen.queryByText(/cannot read this host's secrets file/i),
    ).not.toBeInTheDocument();
  });

  it("offers dropping the remote only when there is one", async () => {
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://example.invalid/x.git",
    });
    const user = userEvent.setup();
    render(<StateRepoPanel showError={() => {}} />);
    await screen.findByText("https://example.invalid/x.git");

    api.mockResolvedValueOnce(local);
    await user.click(
      screen.getByRole("button", { name: "Stop using the remote" }),
    );

    await waitFor(() =>
      expect(api).toHaveBeenLastCalledWith("/api/state-repo", {
        method: "POST",
        body: JSON.stringify({ mode: "local" }),
      }),
    );
    expect(await screen.findByText("local only")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Stop using the remote" }),
    ).not.toBeInTheDocument();
  });

  it("says a merged change is waiting, and what to do about it", async () => {
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://example.invalid/x.git",
      remoteAhead: true,
    });
    render(<StateRepoPanel showError={() => {}} />);

    expect(
      await screen.findByText(/restart it to load this one/i),
    ).toBeInTheDocument();
  });

  it("says outright when the deployment has diverged and stopped syncing", async () => {
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://example.invalid/x.git",
      diverged: true,
      error: "commit 1a2b3c4d was authored by someone <someone@example.com>",
    });
    render(<StateRepoPanel showError={() => {}} />);

    expect(
      await screen.findByText(/diverged from its remote and is not syncing/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/authored by someone/)).toBeInTheDocument();
  });

  it("says when grain could not install the check that runs on pull requests", async () => {
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://example.invalid/x.git",
      workflowRefused: true,
      workflowRefusedAt: "2026-09-04T09:30:00Z",
      workflowFile: ".github/workflows/grain-state-check.yml",
    });
    render(<StateRepoPanel showError={() => {}} />);

    // The condition itself, and the two ways out of it: install the file
    // by hand, or tell grain to stop offering it. A deployment syncing
    // happily with nothing validating its pull requests is the whole
    // reason this sentence exists.
    expect(
      await screen.findByText(/are not being checked/i),
    ).toBeInTheDocument();
    expect(screen.getByText("grain state ci")).toBeInTheDocument();
    expect(
      screen.getByText(".github/workflows/grain-state-check.yml"),
    ).toBeInTheDocument();
  });

  // Every other deployment: the check is installed, or was never offered
  // in the first place, and a pane that mentioned it anyway would be
  // teaching operators to ignore the one that means something.
  it("says nothing about the check when there is nothing wrong with it", async () => {
    api.mockResolvedValueOnce({
      ...local,
      mode: "remote",
      remote: "https://example.invalid/x.git",
    });
    render(<StateRepoPanel showError={() => {}} />);

    await screen.findByText("https://example.invalid/x.git");
    expect(
      screen.queryByText(/are not being checked/i),
    ).not.toBeInTheDocument();
  });

  it("warns when the repository was written by a different schema", async () => {
    api.mockResolvedValueOnce({
      ...local,
      schemaVersion: 15,
      buildSchemaVersion: 16,
    });
    render(<StateRepoPanel showError={() => {}} />);

    expect(
      await screen.findByText(/does not migrate a dump/i),
    ).toBeInTheDocument();
  });
});
