import { describe, expect, it } from "vitest";

import { MODES, TINY_MODES, figurePoints, makeFrame, modeZoom, tierFor } from "./grain-mark.js";

// The geometry half of the mark is pure maths -- no canvas, no React --
// which is where the vendored module's deviations from the design pack
// live and so where they are worth pinning down. See the module header
// for what those deviations are and docs/brand.md for the design.

const FIXED_MODE = [2, 3, 1];

/**
 * The figure a mark of `cssPx` would draw, backed by a `backingPx` canvas,
 * as points normalized to the frame radius -- so two renders at different
 * scales are directly comparable.
 */
function figureAt(cssPx, backingPx, mode) {
  const R = backingPx * 0.44;
  const frame = makeFrame(backingPx / 2, backingPx / 2, R, "circle");
  const tier = tierFor(cssPx);
  // The module seeds nothing itself; jitter is zero at every size these
  // tests use, so the geometry is deterministic without stubbing Math.random.
  const pts = figurePoints(
    frame,
    R * modeZoom(...mode, cssPx),
    mode,
    tier.count,
    64,
    tier.jitter,
    cssPx * 0.44,
  );
  return pts.map((p) => [
    Number(((p.x - backingPx / 2) / R).toFixed(6)),
    Number(((p.y - backingPx / 2) / R).toFixed(6)),
  ]);
}

describe("grain-mark geometry", () => {
  it("draws the same figure on a 2x canvas as on a 1x one", () => {
    // The deviation this exists for: the pack tiers off canvas.width, so
    // without sizePx a 24px mark on a retina display backs onto a 48px
    // canvas, lands in the 40-80px tier and draws the full-tier figure --
    // a different picture, not a denser one.
    expect(figureAt(24, 48, FIXED_MODE)).toEqual(figureAt(24, 24, FIXED_MODE));
  });

  it("tiers by the size the mark is seen at, not the canvas backing it", () => {
    expect(tierFor(24).tiny).toBe(true);
    expect(tierFor(48).tiny).toBe(false);
    // 90 grains at 24px is the pack's tiny-tier density (3.75 per px).
    expect(tierFor(24).count).toBe(90);
  });

  it("puts the fixed figure in both cycles, so still and animation agree", () => {
    // 2·3 (+) is the fixed mark. Each tier draws it at its own slot, and
    // GrainMark.jsx opens the animation on whichever slot its tier uses;
    // if either lookup stopped finding it the mark would come to life on
    // a figure the still was not showing.
    const at = (cycle) => cycle.findIndex((m) => m.every((v, i) => v === FIXED_MODE[i]));
    expect(at(MODES)).toBe(1);
    expect(at(TINY_MODES)).toBe(3);
  });

  it("crops the tiny 2·3 (+) down to the plus and its centre ring", () => {
    // The pack zooms this figure 1.5x below 40px and drops every chain
    // reaching past 0.97 R, which is what takes the outer square
    // off-frame and leaves the plus. At full size the square is part of
    // the figure, so the two tiers differ in chain count as well as scale.
    expect(modeZoom(...FIXED_MODE, 24)).toBe(1.5);
    expect(modeZoom(...FIXED_MODE, 300)).toBe(1);

    const R = 24 * 0.44;
    const frame = makeFrame(12, 12, R, "circle");
    const pts = figurePoints(frame, R * 1.5, FIXED_MODE, 90, 64, 0, R);
    expect(pts.chains).toHaveLength(2);
    for (const { chain } of pts.chains) {
      for (const p of chain) {
        expect(Math.hypot(p.x - 12, p.y - 12)).toBeLessThan(R * 0.97);
      }
    }
  });
});
