import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import CapabilitiesPanel from "./CapabilitiesPanel.jsx";

describe("CapabilitiesPanel", () => {
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

  it("falls back to the id when no display name is given", () => {
    render(<CapabilitiesPanel capabilities={[{ id: "scratch-repo", description: "", ready: true }]} />);
    expect(screen.getByText("scratch-repo")).toBeInTheDocument();
  });
});
