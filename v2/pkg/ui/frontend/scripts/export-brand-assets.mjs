// node scripts/export-brand-assets.mjs
//
// Regenerates the checked-in brand assets from src/brand/grain-mark.js,
// which is the one definition of the mark. The fixed figure is 2·3 (+),
// and the pack renders it differently per size tier -- so the three
// assets below are the same eigenmode, not three different marks:
//
//   public/grain-mark-<theme>.svg   the tiny-tier glyph (the "plus":
//                                   2·3 (+) at the pack's 1.5x tiny zoom,
//                                   as stroke vectors). Scale-free, which
//                                   is what the pack asks a static
//                                   favicon to be -- it is sharp at the
//                                   16px tab slot and the 180px installed
//                                   one alike. Also the still the sidebar
//                                   shows while nothing is running.
//   public/grain-mark-<theme>.png   the same glyph rasterized at 128px,
//                                   the favicon fallback for browsers
//                                   with no SVG-favicon support (Safari
//                                   before 16.4). Same geometry, so the
//                                   two cannot show different marks.
//   docs/brand/grain-hero-2-3plus-<theme>.svg
//                                   the full-tier figure at hero scale as
//                                   grains, for anywhere a large still is
//                                   wanted (README, slides, print).
//
// All three are committed: `npm run build` copies public/ verbatim and
// the docs are read straight out of the repo, so neither has a build
// step that would regenerate them. Re-run this after changing the mark
// or the brand colours, and commit what it writes.
//
// Rendering here re-implements the module's draw calls rather than
// calling into them, because those paint through a canvas 2D context and
// node has none. The geometry comes from the module -- same frame, same
// field, same chains -- so what this writes is what the browser paints.
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { deflateSync } from "node:zlib";

import { THEMES, figurePoints, makeFrame, modeZoom, tierFor } from "../src/brand/grain-mark.js";

const here = dirname(fileURLToPath(import.meta.url));
const frontend = join(here, "..");
const repoRoot = join(frontend, "..", "..", "..", "..");

// The fixed mark: MODES[1] and TINY_MODES[3] are the same figure, which
// is why one mode number covers every size. Below 40px the pack zooms it
// 1.5x and keeps only the chains inside 0.97 R -- the plus and its centre
// ring, with the outer square cropped away.
const FIXED_MODE = [2, 3, 1];

// The design size the tiny glyph is defined at. The pack exports its
// glyphs here and they are scale-free vectors; the PNG below is this
// same geometry rasterized larger, not a separate drawing.
const GLYPH_PX = 24;
// 128px, not 32: the favicon is scaled down to whatever slot asks for it
// (16px tab, 32px bookmark, larger for an installed shortcut), and one
// oversized source scales down cleanly where a 32px one cannot scale up.
const ICON_PX = 128;
const HERO_PX = 440;

// Math.random is what figurePoints jitters with, and what the pack seeds
// its own exports with; seeding it the same way makes these exports
// reproducible and byte-identical to the pack's, so a re-run leaves the
// committed files alone unless the mark itself changed.
function seedRandom(seed) {
  let s = seed;
  Math.random = () => {
    s = (s * 1664525 + 1013904223) % 4294967296;
    return s / 4294967296;
  };
}

/** The tiny glyph's polylines, in a W-sized box. Geometry is the 24px design at any W. */
function glyphChains(W) {
  const R = W * 0.44;
  const frame = makeFrame(W / 2, W / 2, R, "circle");
  seedRandom(7);
  const pts = figurePoints(
    frame,
    R * modeZoom(...FIXED_MODE, GLYPH_PX),
    FIXED_MODE,
    90,
    64,
    0,
    GLYPH_PX * 0.44,
  );
  return pts.chains;
}

// ---------- glyph SVG (the pack's own favicon export) ----------

function renderGlyphSVG(theme) {
  const W = GLYPH_PX;
  const R = W * 0.44;
  const d = glyphChains(W)
    .map((c) => "M " + c.chain.map((p) => `${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(" L "))
    .join(" ");
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${W}">
  <defs><clipPath id="f"><circle cx="${W / 2}" cy="${W / 2}" r="${R}"/></clipPath></defs>
  <path clip-path="url(#f)" d="${d}" fill="none" stroke="${theme.grain}" stroke-width="${(W * 0.028).toFixed(2)}" stroke-linecap="round" stroke-linejoin="round"/>
  <circle cx="${W / 2}" cy="${W / 2}" r="${R}" fill="none" stroke="${theme.grain}" stroke-opacity="0.9" stroke-width="${(W * 0.05).toFixed(2)}"/>
</svg>
`;
}

// ---------- hero SVG (grains, the pack's own export) ----------

function renderHeroSVG(theme) {
  const W = HERO_PX;
  const R = W * 0.44;
  const frame = makeFrame(W / 2, W / 2, R, "circle");
  const tier = tierFor(W);
  seedRandom(42);
  const pts = figurePoints(
    frame,
    R * modeZoom(...FIXED_MODE, W),
    FIXED_MODE,
    tier.count,
    64,
    tier.jitter,
  );
  const r = (W * tier.radius).toFixed(2);
  const circles = pts
    .map((p) => `<circle cx="${p.x.toFixed(2)}" cy="${p.y.toFixed(2)}" r="${r}"/>`)
    .join("\n    ");
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${W}">
  <defs><clipPath id="f"><circle cx="${W / 2}" cy="${W / 2}" r="${R}"/></clipPath></defs>
  <g clip-path="url(#f)" fill="${theme.grain}">
    ${circles}
  </g>
  <circle cx="${W / 2}" cy="${W / 2}" r="${R}" fill="none" stroke="${theme.grain}" stroke-opacity="0.9" stroke-width="${(W * 0.012).toFixed(2)}"/>
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

/** Squared distance from a point to a segment. */
function distSqToSeg(px, py, ax, ay, bx, by) {
  const dx = bx - ax;
  const dy = by - ay;
  const l2 = dx * dx + dy * dy;
  const t = l2 > 1e-12 ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / l2)) : 0;
  const qx = ax + dx * t;
  const qy = ay + dy * t;
  return (px - qx) ** 2 + (py - qy) ** 2;
}

/**
 * The glyph as pixels: the stroked chains plus the ring, supersampled.
 *
 * Stroking through a distance field rather than a polygon sweep is what
 * gives the round caps and joins the SVG asks for -- a point is inside
 * the stroke exactly when it is within half a stroke width of some
 * segment, which is the definition of a round-capped line.
 */
function renderGlyphPNG(size, theme) {
  const chains = glyphChains(size);
  const c = size / 2;
  const R = size * 0.44;
  const halfStroke = (size * 0.028) / 2;
  const halfRing = (size * 0.05) / 2;
  const SS = 3;
  const rgb = theme.grainRGB;
  const out = Buffer.alloc(size * size * 4);

  // Only pixels within a stroke width of some segment can be covered;
  // a per-segment bounding box keeps this a few million tests instead of
  // every pixel against every segment.
  const cover = new Float32Array(size * size);
  const pad = halfStroke + 1;
  for (const { chain } of chains) {
    for (let i = 1; i < chain.length; i++) {
      const a = chain[i - 1];
      const b = chain[i];
      const x0 = Math.max(0, Math.floor(Math.min(a.x, b.x) - pad));
      const x1 = Math.min(size - 1, Math.ceil(Math.max(a.x, b.x) + pad));
      const y0 = Math.max(0, Math.floor(Math.min(a.y, b.y) - pad));
      const y1 = Math.min(size - 1, Math.ceil(Math.max(a.y, b.y) + pad));
      for (let y = y0; y <= y1; y++) {
        for (let x = x0; x <= x1; x++) {
          let hits = 0;
          for (let sy = 0; sy < SS; sy++) {
            for (let sx = 0; sx < SS; sx++) {
              const px = x + (sx + 0.5) / SS;
              const py = y + (sy + 0.5) / SS;
              if (distSqToSeg(px, py, a.x, a.y, b.x, b.y) <= halfStroke * halfStroke) hits++;
            }
          }
          if (hits) {
            const i2 = y * size + x;
            const f = hits / (SS * SS);
            if (f > cover[i2]) cover[i2] = f;
          }
        }
      }
    }
  }

  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let edge = 0;
      let inFrame = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const px = x + (sx + 0.5) / SS;
          const py = y + (sy + 0.5) / SS;
          const d = Math.hypot(px - c, py - c);
          if (Math.abs(d - R) <= halfRing) edge++;
          if (d <= R) inFrame++;
        }
      }
      // The figure is clipped to the frame, the ring is drawn over it at
      // the module's own 0.9 opacity.
      const f = cover[y * size + x] * (inFrame / (SS * SS));
      const e = (edge / (SS * SS)) * 0.9;
      const a = f + e * (1 - f);
      const i = (y * size + x) * 4;
      out[i] = rgb[0];
      out[i + 1] = rgb[1];
      out[i + 2] = rgb[2];
      out[i + 3] = Math.round(a * 255);
    }
  }
  return encodePNG(size, size, out);
}

// ---------- write ----------

mkdirSync(join(frontend, "public"), { recursive: true });
mkdirSync(join(repoRoot, "docs", "brand"), { recursive: true });

for (const [name, theme] of Object.entries(THEMES)) {
  const svg = join(frontend, "public", `grain-mark-${name}.svg`);
  writeFileSync(svg, renderGlyphSVG(theme));
  console.log("wrote", svg);

  const png = join(frontend, "public", `grain-mark-${name}.png`);
  writeFileSync(png, renderGlyphPNG(ICON_PX, theme));
  console.log("wrote", png);

  const hero = join(repoRoot, "docs", "brand", `grain-hero-2-3plus-${name}.svg`);
  writeFileSync(hero, renderHeroSVG(theme));
  console.log("wrote", hero);
}
