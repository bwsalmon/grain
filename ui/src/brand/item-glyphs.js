import { fillScalar } from "./grain-mark.js";

// The item glyphs: one Chladni figure per kind of thing grain keeps a
// list of. The nav rail's four list entries -- Repos, Schedules,
// Templates, Suites -- carried an invisible spacer dot each
// (`dot dot-all`), so the only thing telling them apart was their label.
//
// They are cut from the same plate as the brand mark (docs/brand.md):
// the filled regions of a square-plate eigenmode, inside the same
// circular clip. Four **modes the mark's own cycle does not use**, so a
// list icon is never the figure the sidebar mark is currently showing --
// grain-mark.js's GLYPHS are 1·3 (+), 1·2 (+), 2·3 (−) and 2·3 (+), and
// item-glyphs.test.js holds these four off them.
//
// Zoom is the only other dial, and the pack's own reason for it stands:
// mode numbers are resonances, so a figure is framed by scaling the
// field (grain-mark.js's `glyphZoom` does the same for two of the four
// mark glyphs) and never by interpolating n or m. Each zoom below is
// the framing where the figure reads as one shape at 16px rather than
// as texture.
//
//   repos      2·4 (+) at 2.2   a closed ring. A repo is the boundary
//                               everything else here sits inside: a
//                               task, a schedule and a suite all name
//                               one, and nothing names two. It is the
//                               only figure in the set that closes a
//                               single loop around its own middle.
//   schedules  1·2 (−) at 1.5   an hourglass: two triangles meeting at
//                               a waist, sand running through it. The
//                               mark's diamond is 1·2 (+) -- this is
//                               the same mode with the sign flipped,
//                               which is the plate's other half and a
//                               different figure, not the diamond
//                               reused. A schedule is the one thing in
//                               grain a clock fires.
//   templates  4·5 (+) at 1.2   an even lattice: one cell stamped
//                               across the whole frame. A template is
//                               exactly that -- one task definition,
//                               used again and again.
//   suites     3·4 (−) at 1.6   four heavy lobes meeting at one centre:
//                               several tasks dispatched as a single
//                               run.
//
// The figures are traced to paths by `npm run brand:assets`, into
// item-glyph-paths.js beside this file. This module is the design; that
// one is what it draws.
export const ITEM_GLYPHS = {
  repos: { glyph: [2, 4, 1], zoom: 2.2, figure: "ring" },
  schedules: { glyph: [1, 2, -1], zoom: 1.5, figure: "hourglass" },
  templates: { glyph: [4, 5, 1], zoom: 1.2, figure: "lattice" },
  suites: { glyph: [3, 4, -1], zoom: 1.6, figure: "four lobes on one centre" },
};

/** The kinds that have a glyph, in nav order. */
export const ITEM_KINDS = Object.keys(ITEM_GLYPHS);

/**
 * The scalar an item glyph fills the positive region of, in pixels of a
 * W-sized box: the mode's own filled region (grain-mark.js's
 * `fillScalar`, so the fill rule is the mark's and not a second
 * definition of it) intersected with the frame.
 *
 * The circle is met as a scalar rather than as a clip, so a traced path
 * closes along the frame itself and every glyph is one self-contained
 * `<path>` -- no clipPath, and so no document-unique id, in an icon the
 * page renders eight or ten of.
 */
export function itemFillAt({ glyph, zoom }, W, x, y) {
  const c = W / 2;
  const R = W * 0.44;
  const [n, m, sg] = glyph;
  const Rf = R * zoom;
  // Both terms are positive inside their own region and cross zero at
  // its edge, so the smaller of the two is positive exactly on the
  // intersection -- and near the frame it is the frame's own distance,
  // which is what keeps the traced arc on the circle rather than a grid
  // cell away from it.
  const frame = (R - Math.hypot(x - c, y - c)) / R;
  return Math.min(fillScalar(n, m, sg, (x - c) / Rf, (y - c) / Rf), frame);
}
