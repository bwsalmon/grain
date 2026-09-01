import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import {
  GLYPHS,
  STATIC_SLOT,
  THEMES,
  fillScalar,
  glyphZoom,
  grainSpec,
  sampleGlyph,
} from "./grain-mark.js";
import { DENSE_BELOW_PX, markSpec } from "./grain-density.js";

// The geometry half of the mark is pure maths -- no canvas, no React --
// so it can be pinned down directly. The module itself is a verbatim
// copy of the design pack (see its header), so what is worth testing is
// not the pack's arithmetic but the three things grain leans on: that
// the static logo is the glyph the rest of the app thinks it is, that
// the committed favicon is still that same glyph, and that the grain
// spec behaves across the sizes the app actually renders at.
// See docs/brand.md for the design.

const here = dirname(fileURLToPath(import.meta.url));
const publicDir = join(here, "..", "..", "public");

const LOGO = GLYPHS[STATIC_SLOT];
const frameOf = (W) => ({ W, H: W, cx: W / 2, cy: W / 2, R: W * 0.44 });

/** Loops of the committed still, parsed out of its one SVG path. */
function stillLoops() {
  const svg = readFileSync(join(publicDir, "grain-mark-light.svg"), "utf8");
  return svg
    .match(/ d="([^"]+)"/)[1]
    .split("M ")
    .slice(1)
    .map((loop) =>
      loop
        .replace(/\s*Z\s*$/, "")
        .split(" L ")
        .map((p) => p.trim().split(/\s+/).map(Number)),
    );
}

/** Even-odd crossing count, the fill rule the still is drawn with. */
function insideLoops(loops, x, y) {
  let inside = false;
  for (const loop of loops) {
    for (let i = 0, j = loop.length - 1; i < loop.length; j = i++) {
      const [xi, yi] = loop[i];
      const [xj, yj] = loop[j];
      if (yi > y !== yj > y && x < ((xj - xi) * (y - yi)) / (yj - yi) + xi) inside = !inside;
    }
  }
  return inside;
}

describe("the static logo", () => {
  it("is the 2·3 (−) rosette, pulled out to sit inside the frame", () => {
    // GrainMark.jsx opens the animation on STATIC_SLOT so the mark comes
    // to life on the figure the still was showing; the export script
    // draws the still from the same slot. Both would silently disagree
    // with the pack if this moved.
    expect(LOGO).toEqual([2, 3, -1]);
    expect(glyphZoom(...LOGO)).toBe(0.78);
  });

  it("keeps every grain inside the frame and inside the filled region", () => {
    const f = frameOf(128);
    const Rf = f.R * glyphZoom(...LOGO);
    for (const p of sampleGlyph(f, LOGO, grainSpec(f.W).count)) {
      expect(Math.hypot(p.x - f.cx, p.y - f.cy)).toBeLessThanOrEqual(f.R);
      expect(fillScalar(...LOGO, (p.x - f.cx) / Rf, (p.y - f.cy) / Rf)).toBeGreaterThan(0);
    }
  });

  it("is the shape the committed still draws", () => {
    // The still under public/ is the favicon and every non-animating
    // mark in the app, and it is generated rather than drawn -- so the
    // failure it exists to catch is a mark that changed without
    // `npm run brand` being re-run, which would leave the tab and the
    // sidebar showing the previous logo.
    //
    // The path is traced from the field, so away from the boundary the
    // two have to agree exactly; the only pixels where they can differ
    // are the ones the trace's straight segments cut across, and those
    // are exactly the pixels with a sign change in reach. Skipping those
    // is what lets this assert equality rather than a percentage.
    const loops = stillLoops();
    const W = 128;
    const f = frameOf(W);
    const Rf = f.R * glyphZoom(...LOGO);
    const field = (x, y) => fillScalar(...LOGO, (x - f.cx) / Rf, (y - f.cy) / Rf) > 0;
    let tested = 0;
    const disagreed = [];
    for (let y = 0; y < W; y++) {
      for (let x = 0; x < W; x++) {
        const px = x + 0.5;
        const py = y + 0.5;
        if (Math.hypot(px - f.cx, py - f.cy) > f.R - 1) continue;
        const filled = field(px, py);
        const onEdge = [
          [0.75, 0],
          [-0.75, 0],
          [0, 0.75],
          [0, -0.75],
        ].some(([dx, dy]) => field(px + dx, py + dy) !== filled);
        if (onEdge) continue;
        tested++;
        if (filled !== insideLoops(loops, px, py)) disagreed.push([px, py]);
      }
    }
    expect(tested).toBeGreaterThan(7000);
    expect(disagreed).toEqual([]);
  });

  it("ships a still per theme, in that theme's grain colour", () => {
    for (const [name, theme] of Object.entries(THEMES)) {
      expect(readFileSync(join(publicDir, `grain-mark-${name}.svg`), "utf8")).toContain(
        `fill="${theme.grain}"`,
      );
    }
  });
});

describe("grain spec", () => {
  it("draws the same picture at 1x and 2x", () => {
    // GrainMark.jsx reads the spec at the CSS size and lets the radius
    // -- a fraction of canvas.width -- scale itself with the backing
    // store. Reading it at the backing size instead would take four
    // times the grains at half the size on a 2x display: a different
    // stipple, not a sharper one. These are the numbers that deviation
    // is worth carrying for.
    const cssPx = 20;
    const spec = grainSpec(cssPx);
    for (const dpr of [1, 2, 3]) {
      const backing = cssPx * dpr;
      expect(spec.count).toBe(grainSpec(cssPx).count);
      // Grain radius on screen, in CSS pixels: canvas.width * radius / dpr.
      expect((backing * spec.radius) / dpr).toBeCloseTo(cssPx * spec.radius, 10);
    }
    expect(grainSpec(cssPx * 2).count).not.toBe(spec.count);
  });

  it("scales the pack's count with area, so density is flat", () => {
    // The pack's own curve, which grain follows from 48px up: count goes
    // as the square of the size, so a mark that halves keeps a quarter
    // of its grains.
    expect(grainSpec(48).count).toBe(411);
    expect(grainSpec(96).count / grainSpec(48).count).toBeCloseTo(4, 2);
    expect(grainSpec(320).count).toBe(3200);
  });
});

describe("grain's own density below 48px", () => {
  it("holds the 48px count all the way down", () => {
    // The deviation grain-density.js exists for: the pack's count leaves
    // a small glyph too sparse to read as a shape, so under the
    // threshold the count stops falling and the shrinking area raises
    // the density instead.
    const dense = grainSpec(DENSE_BELOW_PX).count;
    for (const px of [14, 20, 24, 32, 44]) {
      expect(markSpec(px).count).toBe(dense);
      expect(markSpec(px).count).toBeGreaterThan(grainSpec(px).count);
    }
  });

  it("meets the pack exactly at the threshold, and defers to it above", () => {
    // No seam at 48: the count held below it is the pack's own count at
    // that size, so the two curves join rather than step.
    expect(markSpec(DENSE_BELOW_PX)).toEqual(grainSpec(DENSE_BELOW_PX));
    expect(markSpec(DENSE_BELOW_PX - 1).count).toBe(grainSpec(DENSE_BELOW_PX).count);
    for (const px of [64, 128, 320]) expect(markSpec(px)).toEqual(grainSpec(px));
  });

  it("leaves the grain radius to the pack", () => {
    // Only the count changes. The radius is what keeps a grain a grain
    // at each size, and it is the pack's business.
    for (const px of [14, 20, 32, 44, 64]) {
      expect(markSpec(px).radius).toBe(grainSpec(px).radius);
    }
  });

  it("packs the small marks densely enough to fill the glyph", () => {
    // The point of the whole deviation, stated as area: at the badge
    // size the grains cover the filled region several times over, which
    // is what makes the rosette read solid at rest and grainy only in
    // flight.
    for (const px of [20, 32]) {
      const { count, radius } = markSpec(px);
      const grainArea = count * Math.PI * (px * radius) ** 2;
      const frameArea = Math.PI * (px * 0.44) ** 2;
      expect(grainArea / frameArea).toBeGreaterThan(1.5);
    }
  });
});

describe("the glyph cycle", () => {
  it("is four distinct eigenmodes, the logo among them", () => {
    expect(GLYPHS).toHaveLength(4);
    expect(new Set(GLYPHS.map(String)).size).toBe(4);
    expect(GLYPHS[STATIC_SLOT]).toBe(LOGO);
    for (const [n, m] of GLYPHS) {
      // Only integer mode numbers are real resonances -- interpolating
      // one to get between two figures piles up spurious features in the field.
      expect(Number.isInteger(n) && Number.isInteger(m)).toBe(true);
    }
  });

  it("fills each glyph's region without spilling out of the frame", () => {
    const f = frameOf(64);
    for (const glyph of GLYPHS) {
      const pts = sampleGlyph(f, glyph, grainSpec(f.W).count);
      expect(pts.length).toBe(grainSpec(f.W).count);
      for (const p of pts) {
        expect(Math.hypot(p.x - f.cx, p.y - f.cy)).toBeLessThanOrEqual(f.R);
      }
    }
  });
});
