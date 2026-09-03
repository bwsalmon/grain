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
    expect(screen.getByText(/\/var\/lib\/grain\/state-repo/)).toBeInTheDocument();
    // The public key is shown -- it is how an operator checks the key they
    // hold is the one this deployment encrypts to -- and the private half
    // is named as a path, never rendered.
    expect(screen.getByText("grain-secret-pub-v1:AAAA")).toBeInTheDocument();
    expect(screen.getByText(/\/var\/lib\/grain\/secrets\/secrets.key/)).toBeInTheDocument();
  });

  it("says so, and offers nothing, when this UI manages no repository", async () => {
    api.mockResolvedValueOnce({ available: false });
    render(<StateRepoPanel showError={() => {}} />);

    expect(await screen.findByText(/not running inside a daemon/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("Repository URL")).not.toBeInTheDocument();
  });

  it("adopts a repository, and does not keep the pasted token", async () => {
    api.mockResolvedValueOnce(local);
    const user = userEvent.setup();
    render(<StateRepoPanel showError={() => {}} />);
    await screen.findByText("local only");

    await user.type(screen.getByLabelText("Repository URL"), "https://github.com/owner/grain-state.git");
    await user.type(screen.getByLabelText(/Push token/), "ghp_x");
    api.mockResolvedValueOnce({ ...local, mode: "remote", remote: "https://github.com/owner/grain-state.git" });
    await user.click(screen.getByRole("button", { name: "Adopt repository" }));

    await waitFor(() => expect(api).toHaveBeenLastCalledWith("/api/state-repo", {
      method: "POST",
      body: JSON.stringify({
        mode: "remote",
        remote: "https://github.com/owner/grain-state.git",
        branch: "main",
        token: "ghp_x",
      }),
    }));
    expect(await screen.findByText("https://github.com/owner/grain-state.git")).toBeInTheDocument();
    expect(screen.getByLabelText(/Push token/)).toHaveValue("");
  });

  it("offers dropping the remote only when there is one", async () => {
    api.mockResolvedValueOnce({ ...local, mode: "remote", remote: "https://example.invalid/x.git" });
    const user = userEvent.setup();
    render(<StateRepoPanel showError={() => {}} />);
    await screen.findByText("https://example.invalid/x.git");

    api.mockResolvedValueOnce(local);
    await user.click(screen.getByRole("button", { name: "Stop using the remote" }));

    await waitFor(() => expect(api).toHaveBeenLastCalledWith("/api/state-repo", {
      method: "POST",
      body: JSON.stringify({ mode: "local" }),
    }));
    expect(await screen.findByText("local only")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Stop using the remote" })).not.toBeInTheDocument();
  });

  it("warns when the repository was written by a different schema", async () => {
    api.mockResolvedValueOnce({ ...local, schemaVersion: 15, buildSchemaVersion: 16 });
    render(<StateRepoPanel showError={() => {}} />);

    expect(await screen.findByText(/does not migrate a dump/i)).toBeInTheDocument();
  });
});
