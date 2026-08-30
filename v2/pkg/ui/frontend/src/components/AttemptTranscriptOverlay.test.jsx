import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import AttemptTranscriptOverlay from "./AttemptTranscriptOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("AttemptTranscriptOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("fetches and shows the attempt's transcript", async () => {
    api.mockResolvedValueOnce({ transcript: "> read_file(out.txt)\nPONG\n\nfound it" });
    render(
      <AttemptTranscriptOverlay
        taskId="12"
        attempt={{ number: 1, finishedAt: "2026-08-28T12:10:00Z" }}
        onClose={() => {}}
        showError={() => {}}
      />
    );

    expect(await screen.findByText(/found it/)).toBeInTheDocument();
    expect(screen.getByText("Attempt #1 transcript")).toBeInTheDocument();
    expect(api).toHaveBeenLastCalledWith("/api/tasks/12/attempts/1/transcript");
  });

  it("shows a placeholder for a finished attempt with nothing recorded", async () => {
    api.mockResolvedValueOnce({ transcript: "" });
    render(
      <AttemptTranscriptOverlay
        taskId="12"
        attempt={{ number: 1, finishedAt: "2026-08-28T12:10:00Z" }}
        onClose={() => {}}
        showError={() => {}}
      />
    );

    expect(await screen.findByText("(no transcript recorded)")).toBeInTheDocument();
  });

  it("tells a still-running attempt apart from a finished one with nothing recorded", async () => {
    api.mockResolvedValueOnce({ transcript: "" });
    render(
      <AttemptTranscriptOverlay
        taskId="12"
        attempt={{ number: 2 }}
        onClose={() => {}}
        showError={() => {}}
      />
    );

    expect(await screen.findByText(/Still running/)).toBeInTheDocument();
  });

  it("reports a fetch error through showError", async () => {
    const showError = vi.fn();
    api.mockRejectedValueOnce(new Error("boom"));
    render(
      <AttemptTranscriptOverlay
        taskId="12"
        attempt={{ number: 1, finishedAt: "2026-08-28T12:10:00Z" }}
        onClose={() => {}}
        showError={showError}
      />
    );

    await waitFor(() => expect(showError).toHaveBeenCalledWith(new Error("boom")));
  });
});
