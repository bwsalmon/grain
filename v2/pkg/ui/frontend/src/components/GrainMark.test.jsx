import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import GrainMark from "./GrainMark.jsx";

// The animated mark needs a 2D context, which jsdom does not have (see
// setupTests.js). This is the smallest object grain-mark.js actually
// touches: for the grain flight it clears, clips to the frame and fills
// a circle per grain; for the solid glyph a small mark crisps to, it
// builds an ImageData and puts it back. It reads nothing back.
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
    createImageData: (w, h) => ({ width: w, height: h, data: new Uint8ClampedArray(w * h * 4) }),
    putImageData: vi.fn(),
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

    const mark = screen.getByRole("img", { name: "grain — agents working" });
    expect(mark.querySelector("canvas")).not.toBeNull();
    // The mark drew its first frame on mount rather than waiting for an
    // animation frame that a test environment never delivers.
    expect(ctx.fill).toHaveBeenCalled();
  });

  it("stacks a solid glyph under a small mark's grains, as its settled state", () => {
    // Below 48px the mark crisps between flights rather than standing
    // still as a stipple, so it carries a second canvas holding the
    // solid render of the glyph the grains last landed on. It has to be
    // a render rather than the still <img>: that file is the rosette and
    // only the rosette, and the mark crisps onto all four glyphs.
    const ctx = stubCanvas();
    render(<GrainMark size={20} animated />);

    const mark = screen.getByRole("img", { name: "grain — agents working" });
    expect(mark.querySelectorAll("canvas")).toHaveLength(2);
    expect(ctx.putImageData).toHaveBeenCalled();
  });

  it("leaves a hero mark running the pack's own grain cycle", () => {
    // Above the threshold the stipple is the picture, so the hero
    // animates uninterrupted: one canvas, grains only, never a solid
    // frame.
    const ctx = stubCanvas();
    render(<GrainMark size={320} animated />);

    expect(ctx.fill).toHaveBeenCalled();
    expect(screen.getByRole("img", { name: "grain — agents working" }).querySelectorAll("canvas")).toHaveLength(1);
    expect(ctx.putImageData).not.toHaveBeenCalled();
  });

  it("opens crisp, dissolves to fly, and settles again", () => {
    // The loop the whole treatment is: it opens on the still it was
    // already showing, holds it, hands over to the grains for the
    // flight, and comes back. Reading it off the two layers' opacities
    // is the only place the sequence is observable without a real clock.
    vi.useFakeTimers();
    try {
      stubCanvas();
      render(<GrainMark size={20} animated />);
      const mark = screen.getByRole("img", { name: "grain — agents working" });
      const [solid, grains] = mark.querySelectorAll("canvas");

      // On mount: crisp, on the figure the still was already showing.
      expect(grains.style.opacity).toBe("0");
      expect(solid.style.opacity).toBe("1");

      // After the dwell it dissolves and flies.
      vi.advanceTimersByTime(1000);
      expect(grains.style.opacity).toBe("1");
      expect(solid.style.opacity).toBe("0");

      // And settles again once the flight has landed.
      vi.advanceTimersByTime(1500);
      expect(grains.style.opacity).toBe("0");
      expect(solid.style.opacity).toBe("1");
    } finally {
      vi.useRealTimers();
    }
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
