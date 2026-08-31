import { useEffect, useRef, useState } from "react";
import { useTheme } from "@mui/material";

import { createGrainMark } from "../brand/grain-mark.js";

// GrainMark is the brand mark: a Chladni resonance figure -- grains
// settling onto the nodal lines of a vibrating plate -- inside a circular
// frame. See docs/brand.md.
//
// It renders one of two ways, and which one is the whole point of the
// component:
//
//   still     the fixed 1·4 (−) figure, as the PNG scripts/
//             export-brand-assets.mjs writes out of the same module the
//             animation runs. Everywhere a fixed icon is wanted.
//   animated  grains scattering and snapping between the four modes of
//             the cycle, which is what grain shows while agents are
//             actually working. The cycle starts on 1·4 (−), so the
//             animation begins on the figure the still was showing.
//
// The still is an <img> rather than a second canvas render on purpose:
// it is the same file the favicon points at, so the mark in the tab and
// the mark in the sidebar cannot drift apart, and it costs nothing to
// paint on a page that is idle -- which, since "idle" is exactly when it
// is shown, is most of the time.
const STILL_SRC = { light: "/grain-mark-light.png", dark: "/grain-mark-dark.png" };

// The pack's own size tiers are keyed to canvas pixels, which on a 2x
// display are not the size the mark is *seen* at. These are keyed to CSS
// pixels instead and converted below, so a mark reads the same whatever
// the device pixel ratio is: a grain is a fixed size on screen rather
// than a fixed fraction of the backing store.
//
// GRAIN_RADIUS_CSS is deliberately sub-pixel at icon sizes -- the mark is
// sand, and grains that each round up to a whole pixel read as a dotted
// line instead.
const GRAIN_RADIUS_CSS = 0.9;
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
      grainCount: hero ? 2000 : 520,
      grainRadius: (hero ? size * 0.0065 : GRAIN_RADIUS_CSS) / size,
      jitter: hero ? 0.9 * dpr : 0,
      // The still is drawn in the module's `filled` style, whose ring is
      // four times the width the grain styles draw. Matching it here is
      // what keeps the swap between the two reading as the figure coming
      // to life rather than the frame jumping.
      frameWidth: 0.05,
    });
    mark.start();

    // The pack's own note on this renderer is that it is a proof of
    // concept meant for a splash or a hero, not something left running
    // as a persistent UI element -- a fair warning for a mark that
    // animates for as long as anything is running, which on a busy
    // deployment is most of the day. At this size it is a few hundred
    // filled arcs a frame, cheap enough to keep; what it should not do
    // is keep spending them on a tab nobody is looking at.
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
