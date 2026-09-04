import { describe, expect, it } from "vitest";

import { GLYPHS } from "./grain-mark.js";
import { ITEM_GLYPHS, ITEM_KINDS, itemFillAt } from "./item-glyphs.js";
import { ITEM_GLYPH_BOX, ITEM_GLYPH_PATHS } from "./item-glyph-paths.js";

// The item glyphs are the same kind of thing as the mark -- filled
// regions of a square-plate eigenmode -- so they can be pinned down the
// same way, without a canvas or a browser: what each list icon *is*, and
// that the committed path still draws it. See docs/brand.md.

const W = ITEM_GLYPH_BOX;

/** Loops of a committed glyph, parsed out of its one path. */
function loopsOf(kind) {
  return ITEM_GLYPH_PATHS[kind]
    .split("M ")
    .slice(1)
    .map((loop) =>
      loop
        .replace(/\s*Z\s*$/, "")
        .split(" L ")
        .map((p) => p.trim().split(/\s+/).map(Number)),
    );
}

/** Even-odd crossing count, the fill rule the glyphs are drawn with. */
function insideLoops(loops, x, y) {
  let inside = false;
  for (const loop of loops) {
    for (let i = 0, j = loop.length - 1; i < loop.length; j = i++) {
      const [xi, yi] = loop[i];
      const [xj, yj] = loop[j];
      if (yi > y !== yj > y && x < ((xj - xi) * (y - yi)) / (yj - yi) + xi)
        inside = !inside;
    }
  }
  return inside;
}

/** The glyph as a boolean mask of the authoring box, off the field. */
function fieldMask(kind, res) {
  const spec = ITEM_GLYPHS[kind];
  const step = W / res;
  const mask = [];
  for (let j = 0; j < res; j++) {
    const row = [];
    for (let i = 0; i < res; i++)
      row.push(itemFillAt(spec, W, (i + 0.5) * step, (j + 0.5) * step) > 0);
    mask.push(row);
  }
  return mask;
}

describe("the item glyphs", () => {
  it("gives every kind of list a figure of its own", () => {
    // The nav rail's four list entries, and the four the app has.
    expect(ITEM_KINDS).toEqual(["repos", "schedules", "templates", "suites"]);
    for (const kind of ITEM_KINDS) {
      expect(ITEM_GLYPH_PATHS[kind]).toMatch(/^M /);
      expect(ITEM_GLYPHS[kind].figure).toBeTruthy();
    }
  });

  it("takes no mode the mark's own cycle is already using", () => {
    // A list icon should never be the figure the sidebar mark happens to
    // be showing at that moment -- the mark says whether agents are
    // working, and these say what a row is. Sign counts: 1·2 (−) is the
    // hourglass and 1·2 (+) is the mark's diamond, which are different
    // figures of the same resonance.
    const spinner = new Set(GLYPHS.map(String));
    for (const kind of ITEM_KINDS)
      expect(spinner.has(String(ITEM_GLYPHS[kind].glyph))).toBe(false);
  });

  it("is four integer resonances at four distinct framings", () => {
    const seen = new Set();
    for (const kind of ITEM_KINDS) {
      const { glyph, zoom } = ITEM_GLYPHS[kind];
      const [n, m, sg] = glyph;
      // Only integer mode numbers are real resonances; a fractional one
      // piles spurious features into the field (docs/brand.md).
      expect(Number.isInteger(n) && Number.isInteger(m)).toBe(true);
      expect(Math.abs(sg)).toBe(1);
      expect(zoom).toBeGreaterThan(0);
      seen.add(`${glyph}@${zoom}`);
    }
    expect(seen.size).toBe(ITEM_KINDS.length);
  });

  it("draws four pictures a reader can tell apart", () => {
    // Distinct modes are not the point -- distinct *figures* are, and at
    // this size two framings of two different resonances can still land
    // on much the same silhouette. The least alike pair here (the
    // hourglass and the four lobes) differs over 29% of the frame, so a
    // fifth is a floor with room under it -- but well above the couple
    // of percent two near-identical framings would leave, which is the
    // failure worth catching.
    const res = 48;
    const masks = Object.fromEntries(
      ITEM_KINDS.map((kind) => [kind, fieldMask(kind, res)]),
    );
    for (const a of ITEM_KINDS) {
      for (const b of ITEM_KINDS) {
        if (a >= b) continue;
        let differing = 0;
        let inFrame = 0;
        for (let j = 0; j < res; j++) {
          for (let i = 0; i < res; i++) {
            const dx = (i + 0.5) / res - 0.5;
            const dy = (j + 0.5) / res - 0.5;
            if (Math.hypot(dx, dy) > 0.44) continue;
            inFrame++;
            if (masks[a][j][i] !== masks[b][j][i]) differing++;
          }
        }
        expect({
          pair: `${a}/${b}`,
          differing: differing / inFrame > 0.2,
        }).toEqual({
          pair: `${a}/${b}`,
          differing: true,
        });
      }
    }
  });

  it("commits, for each kind, the path its field draws", () => {
    // item-glyph-paths.js is generated (npm run brand:assets), so the
    // failure this exists to catch is the same one grain-mark.test.js
    // catches for the still: a figure changed in item-glyphs.js without
    // the export being re-run, which would leave the nav showing the
    // previous icons.
    //
    // The path is traced from the field, so away from the boundary the
    // two have to agree exactly; the only pixels where they can differ
    // are the ones the trace's straight segments cut across, and those
    // are exactly the pixels with a sign change in reach.
    for (const kind of ITEM_KINDS) {
      const spec = ITEM_GLYPHS[kind];
      const loops = loopsOf(kind);
      const field = (x, y) => itemFillAt(spec, W, x, y) > 0;
      let tested = 0;
      const disagreed = [];
      for (let y = 0; y < W; y++) {
        for (let x = 0; x < W; x++) {
          const px = x + 0.5;
          const py = y + 0.5;
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
      expect({ kind, tested: tested > 7000 }).toEqual({ kind, tested: true });
      expect({ kind, disagreed }).toEqual({ kind, disagreed: [] });
    }
  });
});
