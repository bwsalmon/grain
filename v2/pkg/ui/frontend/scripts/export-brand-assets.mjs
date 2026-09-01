// node scripts/export-brand-assets.mjs
//
// Regenerates the checked-in brand assets from src/brand/grain-mark.js,
// which is the one definition of the mark. The static logo is the 2·3 (−)
// rosette (the pack's STATIC_SLOT), and the assets below are that one
// glyph in the two treatments the pack asks for -- solid where the mark
// is small, grains where it has room:
//
//   public/grain-mark-<theme>.svg   the rosette as a solid filled path.
//                                   Scale-free, so one file is sharp in
//                                   the 16px tab slot and the 180px
//                                   installed one alike. It is both the
//                                   favicon and the still GrainMark.jsx
//                                   shows whenever it is not animating.
//   public/grain-mark-<theme>.png   the same fill rasterized at 128px,
//                                   the favicon fallback for browsers
//                                   with no SVG-favicon support (Safari
//                                   before 16.4). Same geometry, so the
//                                   two cannot show different marks.
//   docs/brand/grain-hero-2-3minus-<theme>.svg
//                                   the rosette at hero scale as grains,
//                                   for anywhere a large still is wanted
//                                   (README, slides, print).
//
// All three are committed: `npm run build` copies public/ verbatim and
// the docs are read straight out of the repo, so neither has a build
// step that would regenerate them. Re-run this after changing the mark
// or the brand colours, and commit what it writes.
//
// Rendering here re-implements the module's draw calls rather than
// calling into them, because those paint through a canvas 2D context and
// node has none. The geometry comes from the module -- same frame, same
// field, same sampling -- so what this writes is what the browser paints.
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { deflateSync } from "node:zlib";

import {
  GLYPHS,
  STATIC_SLOT,
  THEMES,
  fillScalar,
  glyphZoom,
  grainSpec,
  sampleGlyph,
} from "../src/brand/grain-mark.js";

const here = dirname(fileURLToPath(import.meta.url));
const frontend = join(here, "..");
const repoRoot = join(frontend, "..", "..", "..", "..");

// The static logo: 2·3 (−), pulled out to 0.78 of the frame so the
// rosette sits inside the circle rather than running off it.
const LOGO = GLYPHS[STATIC_SLOT];

// The box the vector is authored in. Arbitrary -- the SVG is scale-free
// and carries no pixel commitment -- but it fixes the coordinate
// precision below, and 128 is round enough to read in a diff.
const VECTOR_PX = 128;
// 128px, not 32: the favicon is scaled down to whatever slot asks for it
// (16px tab, 32px bookmark, larger for an installed shortcut), and one
// oversized source scales down cleanly where a 32px one cannot scale up.
const ICON_PX = 128;
// The hero export's size, and the pack's own: `node export-svg.mjs` in
// the pack writes svg/grain-logo-{light,dark}.svg at exactly this size
// and seed, and this file reproduces those two byte for byte. That is
// what pins the vendoring -- if the module drifts from the pack, the
// hero assets stop matching and `npm run brand` shows it.
const HERO_PX = 440;

// sampleGlyph darts at Math.random, and the pack seeds it this way for
// its own exports; seeding it identically makes these reproducible, so a
// re-run leaves the committed files alone unless the mark itself changed.
function seedRandom(seed) {
  let s = seed;
  Math.random = () => {
    s = (s * 1664525 + 1013904223) % 4294967296;
    return s / 4294967296;
  };
}

// ---------- the solid fill, as geometry ----------

/**
 * The scalar the solid mark fills the positive region of, in pixels of a
 * W-sized box, forced negative outside the frame.
 *
 * Clamping to just outside the frame radius is what lets the contour
 * tracer below assume every contour closes inside the box: the glyph's
 * filled regions run past the circle and would otherwise have to be
 * closed along the box edge. The clip path in the SVG (and the frame
 * test in the PNG) then trims the 2% collar this leaves behind, so
 * nothing of it survives into the asset.
 */
function fillAt(W, x, y) {
  const c = W / 2;
  const R = W * 0.44;
  const [n, m, sg] = LOGO;
  const Rf = R * glyphZoom(n, m, sg);
  if (Math.hypot(x - c, y - c) > R * 1.02) return -1;
  return fillScalar(n, m, sg, (x - c) / Rf, (y - c) / Rf);
}

/**
 * Marching squares over `fillAt`, as closed loops in the W box.
 *
 * The standard 16-case table, with the two saddles (5 and 10) resolved
 * by the cell's mean value so a saddle is cut the way the field actually
 * runs through it rather than always the same way -- which on this glyph
 * is the difference between the rosette's arms meeting at the centre and
 * pinching off into separate blobs. Segments are collected first and
 * then chained end to end; the grid is fine enough that endpoints shared
 * between neighbouring cells are bit-identical, so the chaining is an
 * exact key lookup rather than a nearest-point search.
 */
function contours(W, res) {
  const step = W / res;
  const v = new Float64Array((res + 1) * (res + 1));
  for (let j = 0; j <= res; j++) {
    for (let i = 0; i <= res; i++) v[j * (res + 1) + i] = fillAt(W, i * step, j * step);
  }
  const at = (i, j) => v[j * (res + 1) + i];
  // Zero crossing along a cell edge, linearly interpolated.
  const cut = (a, b, va, vb) => {
    const t = va / (va - vb);
    return [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
  };

  const segs = [];
  for (let j = 0; j < res; j++) {
    for (let i = 0; i < res; i++) {
      const v00 = at(i, j);
      const v10 = at(i + 1, j);
      const v11 = at(i + 1, j + 1);
      const v01 = at(i, j + 1);
      const code = (v00 > 0 ? 1 : 0) | (v10 > 0 ? 2 : 0) | (v11 > 0 ? 4 : 0) | (v01 > 0 ? 8 : 0);
      if (code === 0 || code === 15) continue;
      const p00 = [i * step, j * step];
      const p10 = [(i + 1) * step, j * step];
      const p11 = [(i + 1) * step, (j + 1) * step];
      const p01 = [i * step, (j + 1) * step];
      const top = () => cut(p00, p10, v00, v10);
      const right = () => cut(p10, p11, v10, v11);
      const bottom = () => cut(p01, p11, v01, v11);
      const left = () => cut(p00, p01, v00, v01);
      // Which edges the contour crosses. Orientation is not tracked:
      // the loops are chained from either end below and filled with
      // evenodd, neither of which depends on a winding direction.
      switch (code) {
        case 1: case 14: segs.push([left(), top()]); break;
        case 2: case 13: segs.push([top(), right()]); break;
        case 3: case 12: segs.push([left(), right()]); break;
        case 4: case 11: segs.push([right(), bottom()]); break;
        case 6: case 9: segs.push([top(), bottom()]); break;
        case 7: case 8: segs.push([bottom(), left()]); break;
        case 5:
        case 10: {
          const mid = (v00 + v10 + v11 + v01) / 4;
          const joined = code === 5 ? mid > 0 : mid <= 0;
          if (joined) {
            segs.push([left(), top()], [right(), bottom()]);
          } else {
            segs.push([left(), bottom()], [right(), top()]);
          }
          break;
        }
      }
    }
  }

  // Chain the segments into loops. A cut point is computed from the same
  // two corner values in both cells that share the edge, so the doubled
  // endpoints are bit-identical and neighbours can be found by exact key
  // rather than by proximity. Each segment is walked from whichever end
  // the chain arrives at, which is what lets the case table above stay
  // orientation-free.
  const key = (p) => `${p[0]},${p[1]}`;
  const touching = new Map();
  segs.forEach((s, i) => {
    for (const end of s) {
      const k = key(end);
      if (!touching.has(k)) touching.set(k, []);
      touching.get(k).push(i);
    }
  });
  const loops = [];
  const used = new Uint8Array(segs.length);
  for (let start = 0; start < segs.length; start++) {
    if (used[start]) continue;
    used[start] = 1;
    const loop = [segs[start][0], segs[start][1]];
    for (;;) {
      const head = loop[loop.length - 1];
      const next = (touching.get(key(head)) || []).find((i) => !used[i]);
      if (next === undefined) break;
      used[next] = 1;
      const [a, b] = segs[next];
      loop.push(key(a) === key(head) ? b : a);
    }
    if (loop.length > 3) loops.push(loop);
  }
  return loops;
}

/** Douglas-Peucker: drop the points a straight run does not need. */
function simplify(points, tol) {
  if (points.length < 3) return points;
  const keep = new Uint8Array(points.length);
  keep[0] = 1;
  keep[points.length - 1] = 1;
  const stack = [[0, points.length - 1]];
  while (stack.length) {
    const [a, b] = stack.pop();
    const [ax, ay] = points[a];
    const [bx, by] = points[b];
    const dx = bx - ax;
    const dy = by - ay;
    const l2 = dx * dx + dy * dy;
    let far = -1;
    let fd = tol;
    for (let i = a + 1; i < b; i++) {
      const [px, py] = points[i];
      const t = l2 > 1e-12 ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / l2)) : 0;
      const d = Math.hypot(px - (ax + dx * t), py - (ay + dy * t));
      if (d > fd) {
        fd = d;
        far = i;
      }
    }
    if (far > 0) {
      keep[far] = 1;
      stack.push([a, far], [far, b]);
    }
  }
  return points.filter((_, i) => keep[i]);
}

/**
 * The solid mark as one SVG path.
 *
 * Traced at 512 and then simplified to a fifth of a pixel of the
 * authoring box: fine enough that the curve is smooth at any size the
 * favicon is asked for, coarse enough that the file stays a few KB
 * rather than the sixty a raw trace would be. `evenodd` is what makes
 * the rosette's holes holes without depending on the winding coming out
 * of the tracer.
 */
function logoPath() {
  const W = VECTOR_PX;
  return contours(W, 512)
    .map((loop) => simplify(loop, W / 640))
    .filter((loop) => loop.length > 3)
    .map((loop) => `M ${loop.map(([x, y]) => `${x.toFixed(2)} ${y.toFixed(2)}`).join(" L ")} Z`)
    .join(" ");
}

function renderLogoSVG(theme, d) {
  const W = VECTOR_PX;
  const R = W * 0.44;
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${W}">
  <defs><clipPath id="f"><circle cx="${W / 2}" cy="${W / 2}" r="${R}"/></clipPath></defs>
  <path clip-path="url(#f)" fill="${theme.grain}" fill-rule="evenodd" d="${d}"/>
</svg>
`;
}

// ---------- hero SVG (grains -- the pack's own export, reproduced) ----------

function renderHeroSVG(theme) {
  const W = HERO_PX;
  const frame = { W, H: W, cx: W / 2, cy: W / 2, R: W * 0.44 };
  const spec = grainSpec(W);
  seedRandom(42);
  const pts = sampleGlyph(frame, LOGO, spec.count);
  const r = (W * spec.radius).toFixed(2);
  const circles = pts
    .map((p) => `<circle cx="${p.x.toFixed(2)}" cy="${p.y.toFixed(2)}" r="${r}"/>`)
    .join("\n    ");
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${W}">
  <defs><clipPath id="f"><circle cx="${W / 2}" cy="${W / 2}" r="${W * 0.44}"/></clipPath></defs>
  <g clip-path="url(#f)" fill="${theme.grain}">
    ${circles}
  </g>
</svg>
`;
}

// ---------- PNG (no dependencies: node's own zlib plus a CRC) ----------

const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c;
  }
  return t;
})();

function crc32(buf) {
  let c = ~0;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return ~c >>> 0;
}

function chunk(type, data) {
  const head = Buffer.alloc(8);
  head.writeUInt32BE(data.length, 0);
  head.write(type, 4, "ascii");
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(Buffer.concat([head.subarray(4), data])), 0);
  return Buffer.concat([head, data, crc]);
}

/** 8-bit RGBA PNG from a width*height*4 byte buffer. */
function encodePNG(width, height, rgba) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // colour type: truecolour with alpha
  // Each scanline is prefixed with its filter type; 0 (none) leaves the
  // bytes as they are and costs nothing here -- the image is one colour
  // at varying alpha, which deflate already collapses.
  const raw = Buffer.alloc(height * (1 + width * 4));
  for (let y = 0; y < height; y++) {
    raw[y * (1 + width * 4)] = 0;
    rgba.copy(raw, y * (1 + width * 4) + 1, y * width * 4, (y + 1) * width * 4);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

/**
 * The solid mark as pixels: the module's own `style:'solid'` render,
 * which is the filled region supersampled 3x3 and clipped to the frame.
 */
function renderLogoPNG(size, theme) {
  const c = size / 2;
  const R = size * 0.44;
  const SS = 3;
  const rgb = theme.grainRGB;
  const out = Buffer.alloc(size * size * 4);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let cover = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const px = x + (sx + 0.5) / SS;
          const py = y + (sy + 0.5) / SS;
          if (Math.hypot(px - c, py - c) > R) continue;
          if (fillAt(size, px, py) > 0) cover++;
        }
      }
      const i = (y * size + x) * 4;
      out[i] = rgb[0];
      out[i + 1] = rgb[1];
      out[i + 2] = rgb[2];
      out[i + 3] = Math.round((cover / (SS * SS)) * 255);
    }
  }
  return encodePNG(size, size, out);
}

// ---------- write ----------

mkdirSync(join(frontend, "public"), { recursive: true });
mkdirSync(join(repoRoot, "docs", "brand"), { recursive: true });

// One trace, both themes: the two files differ only in the fill colour,
// and tracing once is what guarantees it.
const d = logoPath();

for (const [name, theme] of Object.entries(THEMES)) {
  const svg = join(frontend, "public", `grain-mark-${name}.svg`);
  writeFileSync(svg, renderLogoSVG(theme, d));
  console.log("wrote", svg);

  const png = join(frontend, "public", `grain-mark-${name}.png`);
  writeFileSync(png, renderLogoPNG(ICON_PX, theme));
  console.log("wrote", png);

  const hero = join(repoRoot, "docs", "brand", `grain-hero-2-3minus-${name}.svg`);
  writeFileSync(hero, renderHeroSVG(theme));
  console.log("wrote", hero);
}
