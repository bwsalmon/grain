import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import UpgradePanel from "./UpgradePanel.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("UpgradePanel", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("shows a note instead of a form when the deployment has no upgrader configured", async () => {
    api.mockResolvedValueOnce({ enabled: false });
    render(<UpgradePanel showError={() => {}} />);

    expect(await screen.findByText(/not available/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Branch/)).not.toBeInTheDocument();
  });

  it("submits the entered branch and shows the resulting status", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, phase: "", branch: "" })
      .mockResolvedValueOnce({ enabled: true, phase: "running", branch: "grain/issue-396" });
    const user = userEvent.setup();
    render(<UpgradePanel showError={() => {}} />);

    const input = await screen.findByLabelText(/Branch/);
    await user.type(input, "grain/issue-396");
    await user.click(screen.getByRole("button", { name: "Upgrade" }));

    expect(api).toHaveBeenLastCalledWith("/api/upgrade", {
      method: "POST",
      body: JSON.stringify({ branch: "grain/issue-396" }),
    });
    expect(await screen.findByText("running")).toBeInTheDocument();
    expect(screen.getByText("grain/issue-396")).toBeInTheDocument();
  });

  it("reports a failed upgrade's detail", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      branch: "main",
      phase: "failed",
      detail: "checkout: git fetch: exit status 128",
    });
    render(<UpgradePanel showError={() => {}} />);

    expect(await screen.findByText("failed")).toBeInTheDocument();
    expect(screen.getByText("checkout: git fetch: exit status 128")).toBeInTheDocument();
  });
});
