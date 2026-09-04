import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import AgentPauseBanner, {
  formatRemaining,
  pauseMessage,
} from "./AgentPauseBanner.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const pause = {
  paused: true,
  until: "2026-09-03T17:00:00Z",
  reason: "claude: usage limit reached; resets at 2026-09-03T17:00:00Z",
  secondsRemaining: 7200,
};

describe("AgentPauseBanner", () => {
  it("says what grain is doing, until when, and what the provider said", () => {
    render(<AgentPauseBanner pause={pause} />);

    expect(
      screen.getByText(/nothing is being dispatched/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/about 2h 0m/)).toBeInTheDocument();
    // The provider's own sentence verbatim: it names the framework and
    // the window, which is the half an operator can act on.
    expect(screen.getByText(/claude: usage limit reached/)).toBeInTheDocument();
  });

  it("lifts the pause and lets the caller re-read the config", async () => {
    api.mockResolvedValue({
      enabled: true,
      lifted: true,
      pause: { paused: false },
    });
    const onLifted = vi.fn().mockResolvedValue(undefined);
    render(<AgentPauseBanner pause={pause} onLifted={onLifted} />);

    fireEvent.click(screen.getByRole("button", { name: /resume now/i }));

    await waitFor(() =>
      expect(api).toHaveBeenCalledWith("/api/pause", { method: "DELETE" }),
    );
    await waitFor(() => expect(onLifted).toHaveBeenCalled());
  });

  it("reports a failed lift rather than pretending dispatch resumed", async () => {
    api.mockRejectedValue(new Error("nope"));
    const showError = vi.fn();
    const onLifted = vi.fn();
    render(
      <AgentPauseBanner
        pause={pause}
        onLifted={onLifted}
        showError={showError}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /resume now/i }));

    await waitFor(() => expect(showError).toHaveBeenCalledWith("nope"));
    expect(onLifted).not.toHaveBeenCalled();
    // Still on screen, and still offering the button: the gate is shut
    // either way, and the banner is what says so.
    expect(screen.getByRole("button", { name: /resume now/i })).toBeEnabled();
  });

  it("still says something useful when the pause names no instant", () => {
    expect(pauseMessage({ paused: true })).toMatch(
      /until the provider's window resets/,
    );
  });

  it("rounds a countdown to hours and minutes", () => {
    // Coarse on purpose: config is polled every few seconds, and a
    // banner whose text changes on every poll is one nobody can read.
    expect(formatRemaining(30)).toBe("less than a minute");
    expect(formatRemaining(600)).toBe("about 10m");
    expect(formatRemaining(18000)).toBe("about 5h 0m");
    expect(formatRemaining(-5)).toBe("less than a minute");
  });
});
