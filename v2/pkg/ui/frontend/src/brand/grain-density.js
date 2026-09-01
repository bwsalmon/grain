import { grainSpec } from "./grain-mark.js";

// How many grains grain draws, which below 48px is not what the pack
// says. Kept out of grain-mark.js so that file stays a verbatim copy of
// the pack (see its header) and this deviation stays visible as one.
//
// The pack scales the count with area, so density is constant across its
// middle band and a mark that halves in size loses three quarters of its
// grains. That is right for a hero and wrong for an icon: by the sidebar
// and badge sizes there are too few grains across the glyph to fill it,
// and the rosette reads as a hatched disc rather than as a shape.
//
// So below 48px the count stops falling: a small mark keeps the grain
// count of a 48px one, and the shrinking area does the rest. Density
// then rises as the mark gets smaller, which is exactly the direction it
// needs to go, and there is no seam at the threshold because that is the
// pack's own count at exactly that size.
//
// 48px is where the two agree, and also where the glyphs stop needing
// the help. Past about this count the picture stops changing -- the
// grains are already touching, and the glyph reads solid at rest and
// grainy only in flight, which is the behaviour worth having at these
// sizes.
export const DENSE_BELOW_PX = 48;

/** Grain count and radius for a mark seen at `cssPx`. */
export function markSpec(cssPx) {
  const spec = grainSpec(cssPx);
  if (cssPx >= DENSE_BELOW_PX) return spec;
  return { count: grainSpec(DENSE_BELOW_PX).count, radius: spec.radius };
}
