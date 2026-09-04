import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import PromptOverlay from "./PromptOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("PromptOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("fetches and shows the prompt the agent was given", async () => {
    api.mockResolvedValueOnce({
      prompt: "Fix the thing\n\nWork in acme/widgets.",
      attempt: 2,
    });
    render(<PromptOverlay taskId="12" onClose={() => {}} />);

    expect(
      await screen.findByText(/Work in acme\/widgets\./),
    ).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/tasks/12/prompt");
  });

  // Which attempt's prompt this is matters: a redispatched task's prompt
  // grows with its conversation, so "attempt #3" is part of the answer.
  it("names the attempt the prompt belongs to", async () => {
    api.mockResolvedValueOnce({ prompt: "the prompt", attempt: 3 });
    render(<PromptOverlay taskId="12" onClose={() => {}} />);

    expect(await screen.findByText(/attempt #3/)).toBeInTheDocument();
  });

  it("explains a task that has never been dispatched rather than showing nothing", async () => {
    api.mockResolvedValueOnce({ prompt: "", attempt: 0 });
    render(<PromptOverlay taskId="12" onClose={() => {}} />);

    expect(
      await screen.findByText(/No prompt recorded yet/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/attempt #/)).not.toBeInTheDocument();
  });

  it("reports a fetch failure in the pane itself", async () => {
    api.mockRejectedValueOnce(new Error("boom"));
    render(<PromptOverlay taskId="12" onClose={() => {}} />);

    expect(
      await screen.findByText(/Could not load this task's prompt: boom/),
    ).toBeInTheDocument();
  });
});
