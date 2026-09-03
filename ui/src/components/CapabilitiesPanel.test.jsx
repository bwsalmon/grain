import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import CapabilitiesPanel from "./CapabilitiesPanel.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("CapabilitiesPanel", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("shows an empty note with no capabilities", () => {
    render(<CapabilitiesPanel capabilities={[]} />);
    expect(screen.getByText("No capabilities known.")).toBeInTheDocument();
  });

  it("marks a ready capability Ready with no missing hints", () => {
    render(
      <CapabilitiesPanel
        capabilities={[{ id: "self-debug", name: "Self debug", description: "Read grain's own source", ready: true }]}
      />,
    );
    expect(screen.getByText("Self debug")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.queryByText(/Needs:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Missing secrets:/)).not.toBeInTheDocument();
  });

  it("shows what's missing for a capability that isn't ready", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          {
            id: "gcp-key",
            name: "GCP key",
            description: "Mint a short-lived GCP service-account key for this task",
            ready: false,
            missingConfig: ["GCP project", "GCP service account email"],
            missingSecrets: ["gcp-key-minter"],
          },
        ]}
      />,
    );
    expect(screen.getByText("Not ready")).toBeInTheDocument();
    expect(screen.getByText(/Needs: GCP project, GCP service account email/)).toBeInTheDocument();
    expect(screen.getByText(/Missing secrets: gcp-key-minter/)).toBeInTheDocument();
  });

  // A capability can be perfectly configured and still be one no task
  // can attach -- the gap that leaves a "Ready" gcp-key placing nothing
  // in any sandbox, because the task capability picker never offered it.
  it("flags a ready capability no task can be granted", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          { id: "gcp-key", name: "GCP key", description: "Mint a key", ready: true, grantable: false },
        ]}
      />,
    );
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("Not grantable")).toBeInTheDocument();
    expect(screen.getByText(/No task can be granted this/)).toBeInTheDocument();
  });

  it("says nothing about grantability for a capability a task can be granted", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          { id: "gemini-key", name: "Gemini key", description: "Mint a key", ready: true, grantable: true },
        ]}
      />,
    );
    expect(screen.queryByText("Not grantable")).not.toBeInTheDocument();
    expect(screen.queryByText(/No task can be granted this/)).not.toBeInTheDocument();
  });

  // grain/task-14: a capability every new task is filed holding is
  // reported as such here, and one that is defaulted while not ready is
  // the deployment-wide problem this pane exists to surface -- every
  // task filed will fail to dispatch on it, not just the ones somebody
  // ticked it on.
  it("marks a defaulted capability, and warns when a defaulted one is not ready", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          {
            id: "gcp-key",
            name: "GCP key",
            description: "Mint a key",
            ready: false,
            grantable: true,
            default: true,
            missingConfig: ["GCP project"],
          },
        ]}
      />,
    );
    expect(screen.getByText("Default")).toBeInTheDocument();
    expect(screen.getByText(/Every new task is filed holding this/)).toBeInTheDocument();
  });

  it("says nothing about defaults for a capability that is not one", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          { id: "gemini-key", name: "Gemini key", description: "Mint a key", ready: false, grantable: true },
        ]}
      />,
    );
    expect(screen.queryByText("Default")).not.toBeInTheDocument();
    expect(screen.queryByText(/Every new task is filed holding this/)).not.toBeInTheDocument();
  });

  // grain/task-24: with a second, per-repo layer, the pane has to say
  // which layer defaults a capability -- "Default" alone would describe
  // a deployment-wide default that only some tasks actually get.
  it("names the repos a capability is defaulted on, separately from a deployment-wide default", () => {
    render(
      <CapabilitiesPanel
        capabilities={[
          {
            id: "gcp-key",
            name: "GCP key",
            description: "Mint a key",
            ready: true,
            grantable: true,
            default: false,
            defaultRepos: ["acme/widgets", "acme/gadgets"],
          },
        ]}
      />,
    );
    expect(screen.queryByText("Default")).not.toBeInTheDocument();
    expect(screen.getByText("Default in 2 repos")).toBeInTheDocument();
    expect(screen.getByText(/acme\/widgets, acme\/gadgets/)).toBeInTheDocument();
  });

  // grain/task-110: a capability's own credentials are set from the row
  // that reports them missing, rather than from a Secrets tab that never
  // said which name belonged to which capability.
  it("offers a write-only field for each credential a capability resolves", async () => {
    api.mockResolvedValueOnce({});
    const onSecretsChanged = vi.fn();
    const user = userEvent.setup();
    render(
      <CapabilitiesPanel
        capabilities={[
          {
            id: "gcp-key",
            name: "GCP key",
            description: "Mint a key",
            ready: false,
            grantable: true,
            missingSecrets: ["gcp-key-minter"],
            secrets: [{ name: "gcp-key-minter", secret: "gcp-key-minter", key: "value", set: false }],
          },
        ]}
        showError={() => {}}
        onSecretsChanged={onSecretsChanged}
      />,
    );

    await user.type(screen.getByLabelText("gcp-key-minter"), "a-key");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(api).toHaveBeenCalledWith("/api/secrets/gcp-key-minter/value", {
      method: "PUT",
      body: JSON.stringify({ value: "a-key" }),
    });
    expect(onSecretsChanged).toHaveBeenCalled();
  });

  // No colocated secrets store means the API reports no secrets for any
  // capability, so there is nothing to offer -- a field whose every use
  // would 404 is worse than none.
  it("offers no secret field for a capability that reports none", () => {
    render(
      <CapabilitiesPanel
        capabilities={[{ id: "github-sandbox", name: "GitHub sandbox", description: "Sandbox repo", ready: true }]}
        showError={() => {}}
        onSecretsChanged={() => {}}
      />,
    );

    expect(screen.queryByRole("button", { name: "Set" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Credentials? this needs:/)).not.toBeInTheDocument();
  });

  it("falls back to the id when no display name is given", () => {
    render(<CapabilitiesPanel capabilities={[{ id: "some-new-capability", description: "", ready: true }]} />);
    expect(screen.getByText("some-new-capability")).toBeInTheDocument();
  });
});
