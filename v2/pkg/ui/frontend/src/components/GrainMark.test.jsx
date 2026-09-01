import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import GrainMark from "./GrainMark.jsx";

// The animated mark needs a 2D context, which jsdom does not have (see
// setupTests.js). This is the smallest object grain-mark.js actually
// touches: it clears, clips to the frame, fills a circle per grain, and
// reads nothing back.
function stubCanvas() {
  const ctx = {
    clearRect: vi.fn(),
    save: vi.fn(),
    restore: vi.fn(),
    beginPath: vi.fn(),
    closePath: vi.fn(),
    moveTo: vi.fn(),
    lineTo: vi.fn(),
    arc: vi.fn(),
    clip: vi.fn(),
    fill: vi.fn(),
    stroke: vi.fn(),
  };
  HTMLCanvasElement.prototype.getContext = () => ctx;
  return ctx;
}

function stubMatchMedia(reduced) {
  window.matchMedia = (query) => ({
    matches: query === "(prefers-reduced-motion: reduce)" && reduced,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  });
}

afterEach(() => {
  HTMLCanvasElement.prototype.getContext = () => null;
  delete window.matchMedia;
});

describe("GrainMark", () => {
  it("shows the fixed mark as an image when nothing is animating", () => {
    render(<GrainMark size={24} title="grain" />);

    const img = screen.getByTitle("grain");
    expect(img.tagName).toBe("IMG");
    expect(img).toHaveAttribute("src", "/grain-mark-light.svg");
  });

  it("paints an animated canvas while agents are working", () => {
    const ctx = stubCanvas();
    render(<GrainMark size={24} animated />);

    const canvas = screen.getByRole("img", { name: "grain — agents working" });
    expect(canvas.tagName).toBe("CANVAS");
    // The mark drew its first frame on mount rather than waiting for an
    // animation frame that a test environment never delivers.
    expect(ctx.fill).toHaveBeenCalled();
  });

  it("falls back to the fixed mark when the reader asked for less motion", () => {
    stubCanvas();
    stubMatchMedia(true);
    render(<GrainMark size={24} animated title="grain" />);

    expect(screen.getByTitle("grain").tagName).toBe("IMG");
  });

  it("falls back to the fixed mark when there is no canvas to paint on", () => {
    render(<GrainMark size={24} animated title="grain" />);

    expect(screen.getByTitle("grain").tagName).toBe("IMG");
  });
});
