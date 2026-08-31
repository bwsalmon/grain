// node scripts/export-brand-assets.mjs
//
// Regenerates the checked-in brand assets from src/brand/grain-mark.js,
// which is the one definition of the mark. Two kinds come out of it:
//
//   public/grain-mark-<theme>.png   the fixed mark, 1·4 (−), rendered in
//                                   the module's `filled` style. This is
//                                   both the favicon and the still mark
//                                   the sidebar shows while nothing is
//                                   running -- at those sizes (16-32px)
//                                   the grains and the nodal lines are
//                                   thinner than a pixel and wash out,
//                                   so the positive region of the field
//                                   is what stays legible.
//   docs/brand/grain-1-4minus-<theme>.svg
//                                   the same figure at hero scale as
//                                   grains, for anywhere a large still
//                                   of the mark is wanted (README,
//                                   slides, print).
//
// Both are committed: `npm run build` copies public/ verbatim and the
// docs are read straight out of the repo, so neither has a build step
// that would regenerate them. Re-run this after changing the mark or the
// brand colours, and commit what it writes.
//
// Rendering here is a re-implementation of the module's own `filled`
// path rather than a call into it, because that path draws through a
// canvas 2D context and node has none. It is the same math -- the same
// frame, the same field, the same 3x supersample -- so the PNG matches
// what renderStatic(canvas, {style: 'filled'}) paints in a browser.
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { deflateSync } from "node:zlib";

import { THEMES, fieldVal, figurePoints, makeFrame, traceFigure } from "../src/brand/grain-mark.js";

const here = dirname(fileURLToPath(import.meta.url));
const frontend = join(here, "..");
const repoRoot = join(frontend, "..", "..", "..", "..");

// The fixed mark. MODES[0] in the module; the cycle the animation runs
// starts and ends here, so the still and the first frame of the
// animation are the same figure.
const FIXED_MODE = [1, 4, -1];

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

/** The module's `filled` render: positive field inside the frame, plus the ring. */
function renderFilled(size, theme) {
  const cx = size / 2;
  const cy = size / 2;
  const Rframe = size * 0.44;
  const frame = makeFrame(cx, cy, Rframe, "circle");
  // 0.05 of the width is what the module's own filled render uses for
  // the ring -- thick enough to survive the sizes this render is for.
  const ring = Math.max(1, size * 0.05) / 2;
  const SS = 3;
  const rgb = theme.grainRGB;
  const out = Buffer.alloc(size * size * 4);

  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let fill = 0;
      let edge = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const px = x + (sx + 0.5) / SS;
          const py = y + (sy + 0.5) / SS;
          if (Math.abs(Math.hypot(px - cx, py - cy) - Rframe) <= ring) edge++;
          if (!frame.contains(px, py)) continue;
          if (fieldVal((px - cx) / Rframe, (py - cy) / Rframe, ...FIXED_MODE) > 0) fill++;
        }
      }
      const f = fill / (SS * SS);
      // The ring is drawn over the fill at the module's own 0.9 opacity.
      const a = f + (edge / (SS * SS)) * 0.9 * (1 - f);
      const i = (y * size + x) * 4;
      out[i] = rgb[0];
      out[i + 1] = rgb[1];
      out[i + 2] = rgb[2];
      out[i + 3] = Math.round(a * 255);
    }
  }
  return encodePNG(size, size, out);
}

// ---------- hero SVG (grains, the pack's own export) ----------

// Math.random is what figurePoints jitters with; seeding it makes the
// export reproducible, so re-running this script leaves the committed
// SVGs byte-identical unless the mark itself changed.
function seedRandom(seed) {
  let s = seed;
  Math.random = () => {
    s = (s * 1664525 + 1013904223) % 4294967296;
    return s / 4294967296;
  };
}

function renderHeroSVG(theme) {
  const W = 440;
  const cx = W / 2;
  const cy = W / 2;
  const Rframe = W * 0.44;
  const frame = makeFrame(cx, cy, Rframe, "circle");
  seedRandom(42);
  const segs = traceFigure(frame, Rframe, FIXED_MODE, 64);
  const pts = figurePoints(segs, 2000, 0.9);
  const r = (W * 0.0065).toFixed(2);
  const circles = pts
    .map((p) => `<circle cx="${p.x.toFixed(2)}" cy="${p.y.toFixed(2)}" r="${r}"/>`)
    .join("\n    ");
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${W}" width="${W}" height="${W}">
  <defs><clipPath id="frame"><circle cx="${cx}" cy="${cy}" r="${Rframe}"/></clipPath></defs>
  <g clip-path="url(#frame)" fill="${theme.grain}">
    ${circles}
  </g>
  <circle cx="${cx}" cy="${cy}" r="${Rframe}" fill="none" stroke="${theme.grain}" stroke-opacity="0.9" stroke-width="${(W * 0.012).toFixed(2)}"/>
</svg>
`;
}

// ---------- write ----------

// 128px, not 32: the favicon is scaled down to whatever slot asks for it
// (16px tab, 32px bookmark, larger for an installed shortcut), and one
// oversized source scales down cleanly where a 32px one cannot scale up.
const ICON_PX = 128;

mkdirSync(join(frontend, "public"), { recursive: true });
mkdirSync(join(repoRoot, "docs", "brand"), { recursive: true });

for (const [name, theme] of Object.entries(THEMES)) {
  const png = join(frontend, "public", `grain-mark-${name}.png`);
  writeFileSync(png, renderFilled(ICON_PX, theme));
  console.log("wrote", png);

  const svg = join(repoRoot, "docs", "brand", `grain-1-4minus-${name}.svg`);
  writeFileSync(svg, renderHeroSVG(theme));
  console.log("wrote", svg);
}
