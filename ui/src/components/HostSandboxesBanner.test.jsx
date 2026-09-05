import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import HostSandboxesBanner from "./HostSandboxesBanner.jsx";

describe("HostSandboxesBanner", () => {
  // The three things an operator has to be told, in one sentence each:
  // what is running where, why that is a problem outside a machine they
  // own, and what to do about it. A banner that only said "host mode"
  // would need a reader who already knew what this task is about.
  it("names what host mode means and how to get off it", () => {
    render(<HostSandboxesBanner />);

    expect(screen.getByText(/host mode/i)).toBeInTheDocument();
    expect(screen.getByText(/directly on the host/i)).toBeInTheDocument();
    expect(screen.getByText(/GRAIN_KONTUR_ENABLE=1/)).toBeInTheDocument();
  });
});
