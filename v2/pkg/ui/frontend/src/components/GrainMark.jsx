import { useEffect, useRef, useState } from "react";
import { useTheme } from "@mui/material";

import { MODES, TINY_MODES, createGrainMark } from "../brand/grain-mark.js";

// GrainMark is the brand mark: a Chladni resonance figure -- grains
// settling onto the nodal lines of a vibrating plate -- inside a circular
// frame. See docs/brand.md.
//
// It renders one of two ways, and which one is the whole point of the
// component:
//
//   still     the fixed 2·3 (+) figure, as the SVG scripts/
//             export-brand-assets.mjs writes out of the same module the
//             animation runs. Everywhere a fixed icon is wanted.
//   animated  grains scattering and snapping around the cycle, which is
//             what grain shows while agents are actually working. It
//             opens on the figure the still was showing, so the change
//             reads as the mark coming to life rather than as a
//             different image.
//
// The still is an <img> rather than a second canvas render on purpose:
// it is the same file the favicon points at, so the mark in the tab and
// the mark in the sidebar cannot drift apart, and it costs nothing to
// paint on a page that is idle -- which, since "idle" is exactly when it
// is shown, is most of the time. It is the scale-free glyph vector, so
// one file is sharp at every size the app asks for.
const STILL_SRC = { light: "/grain-mark-light.svg", dark: "/grain-mark-dark.svg" };

// The fixed figure. MODES[1] and TINY_MODES[3] are the same eigenmode --
// the pack draws it as grains above 40px and as the "plus" glyph below,
// which is one mark rendered per tier rather than two marks.
const FIXED_MODE = [2, 3, 1];
const sameMode = (a, b) => a[0] === b[0] && a[1] === b[1] && a[2] === b[2];

// Which slot of the cycle the still is showing. The two cycles run in
// lockstep on one clock but arrive at the fixed figure at different
// slots, so a mark opens on whichever slot its own tier draws it with.
const FIXED_SLOT = {
  full: MODES.findIndex((m) => sameMode(m, FIXED_MODE)),
  tiny: TINY_MODES.findIndex((m) => sameMode(m, FIXED_MODE)),
};

// The pack's tier boundaries, in CSS pixels. Below TINY_PX the mark runs
// the companion glyph cycle -- a simpler set of figures that survives at
// icon size -- and at HERO_PX and up the grains gain jitter.
const TINY_PX = 40;
const HERO_PX = 300;

export default function GrainMark({ size = 28, animated = false, title, className }) {
  const theme = useTheme();
  const mode = theme.palette.mode === "dark" ? "dark" : "light";
  const reducedMotion = usePrefersReducedMotion();
  const canvasRef = useRef(null);
  // A mark that cannot paint (a canvas-less environment -- jsdom under
  // vitest, most obviously) falls back to the still rather than leaving
  // a blank box where the brand should be.
  const [canPaint, setCanPaint] = useState(true);
  const spinning = animated && !reducedMotion && canPaint;

  useEffect(() => {
    if (!animated || reducedMotion) return undefined;
    const canvas = canvasRef.current;
    if (!canvas) return undefined;
    let ctx = null;
    try {
      ctx = canvas.getContext("2d");
    } catch {
      ctx = null;
    }
    if (!ctx) {
      setCanPaint(false);
      return undefined;
    }

    const dpr = window.devicePixelRatio || 1;
    const px = Math.max(1, Math.round(size * dpr));
    canvas.width = px;
    canvas.height = px;

    const hero = size >= HERO_PX;
    const mark = createGrainMark(canvas, {
      theme: mode,
      // Tier by the size the mark is *seen* at rather than the backing
      // store's, which is the deviation the vendored module carries for
      // exactly this: on a 2x display a 24px mark backs onto a 48px
      // canvas, and tiering off that would run the full-tier figures
      // instead of the glyph cycle -- the wrong picture, not merely the
      // wrong density. Grain count and radius both follow from it.
      sizePx: size,
      // Open on the figure the still is already showing.
      slot: size < TINY_PX ? FIXED_SLOT.tiny : FIXED_SLOT.full,
      // Jitter is the one tier value the module reads as raw pixels
      // rather than a fraction of the width, so it is the one that has
      // to be converted to stay the same size on screen at 2x.
      ...(hero ? { jitter: 0.9 * dpr } : {}),
      // The still's ring is the glyph export's 0.05 of the width, four
      // times what the animation draws by default. Matching it here is
      // what keeps the swap between the two reading as the figure coming
      // to life rather than the frame jumping.
      frameWidth: 0.05,
    });
    mark.start();

    // The pack's own note on this renderer is that it is a proof of
    // concept meant for a splash or a hero, not something left running
    // as a persistent UI element -- a fair warning for a mark that
    // animates for as long as anything is running, which on a busy
    // deployment is most of the day. The glyph tier makes that cheaper
    // than it was -- 90 filled arcs a frame at 24px rather than 520 --
    // but what it still should not do is keep spending them on a tab
    // nobody is looking at.
    const onVisibility = () => (document.hidden ? mark.stop() : mark.start());
    document.addEventListener("visibilitychange", onVisibility);
    onVisibility();

    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      mark.destroy();
    };
  }, [animated, reducedMotion, size, mode]);

  const label = title || (spinning ? "grain — agents working" : "grain");

  if (!spinning) {
    return (
      <img
        className={className}
        src={STILL_SRC[mode]}
        width={size}
        height={size}
        alt=""
        title={label}
        style={{ width: size, height: size, display: "block", flex: "none" }}
      />
    );
  }

  return (
    <canvas
      className={className}
      ref={canvasRef}
      role="img"
      aria-label={label}
      title={label}
      style={{ width: size, height: size, display: "block", flex: "none" }}
    />
  );
}

// The mark's whole reason for animating is to signal activity, so a
// reader who has asked for less motion gets the still -- the sidebar
// still says how many tasks are running, in words, right below it.
function usePrefersReducedMotion() {
  const [reduced, setReduced] = useState(() => matches());
  useEffect(() => {
    const mq = window.matchMedia?.("(prefers-reduced-motion: reduce)");
    if (!mq?.addEventListener) return undefined;
    const onChange = () => setReduced(mq.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return reduced;
}

function matches() {
  try {
    return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  } catch {
    // matchMedia is not everywhere (jsdom needs it stubbed); no query
    // answered means no preference expressed.
    return false;
  }
}
