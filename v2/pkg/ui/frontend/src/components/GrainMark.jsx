import { useEffect, useRef, useState } from "react";
import { useTheme } from "@mui/material";

import { STATIC_SLOT, createGrainMark } from "../brand/grain-mark.js";
import { markSpec } from "../brand/grain-density.js";

// GrainMark is the brand mark: a Chladni glyph -- the filled regions of a
// square-plate eigenmode, stippled into grains -- inside an invisible
// circular clip. See docs/brand.md.
//
// It renders one of two ways, and which one is the whole point of the
// component:
//
//   still     the static logo, the 2·3 (−) rosette, as a solid fill.
//             It is the SVG scripts/export-brand-assets.mjs writes out
//             of the same module the animation runs, and it is what the
//             mark shows everywhere nothing is happening.
//   animated  grains scattering and flying between the four glyphs,
//             which is what grain shows while agents are actually
//             working. It opens on the rosette -- the figure the still
//             was showing -- so the change reads as the mark coming to
//             life rather than as a different image.
//
// The still is an <img> rather than a canvas render on purpose: it is
// the same file the favicon points at, so the mark in the tab and the
// mark in the sidebar cannot drift apart, and it costs nothing to paint
// on a page that is idle -- which, since "idle" is exactly when it is
// shown, is most of the time. It is a scale-free vector, so one file is
// sharp at every size the app asks for.
//
// The still is solid rather than grains at every size, including the
// hero. The pack draws its own large stills as grains and only asks for
// the solid fill below 32px, but a still that changed treatment partway
// up the size range would make the sidebar mark and the loading screen
// two different pictures of the same thing; the grain rendering of the
// logo at hero scale is still exported, for a README or a slide, as
// docs/brand/grain-hero-2-3minus-{light,dark}.svg.
const STILL_SRC = { light: "/grain-mark-light.svg", dark: "/grain-mark-dark.svg" };

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
    canvas.width = Math.max(1, Math.round(size * dpr));
    canvas.height = canvas.width;

    // The module picks grain count and radius off canvas.width, which on
    // a 2x display is twice the size the mark is *seen* at: a 20px mark
    // backing onto a 40px canvas would take four times the grains at
    // half the size, a visibly finer stipple rather than a sharper one.
    // Reading the spec at the CSS size instead pins the picture to what
    // the reader sees and leaves the backing store free to be as dense
    // as the display allows -- the radius is a fraction of canvas.width,
    // so it already scales itself and needs no dpr correction.
    //
    // markSpec, not the module's grainSpec: below 48px grain stipples
    // the glyph more thickly than the pack does, so the small marks read
    // as shapes rather than as hatching. See grain-density.js.
    const spec = markSpec(size);
    const mark = createGrainMark(canvas, {
      theme: mode,
      count: spec.count,
      radius: spec.radius,
      // Open on the figure the still is already showing.
      slot: STATIC_SLOT,
    });
    mark.start();

    // The pack's own note on this renderer is that it is a proof of
    // concept meant for a splash or a hero, not something left running
    // as a persistent UI element -- a fair warning for a mark that
    // animates for as long as anything is running, which on a busy
    // deployment is most of the day. The denser small marks make that
    // bill bigger, not smaller -- a 20px badge is 411 filled arcs a
    // frame now rather than 117, and a task list can hold several of
    // them at once -- so what it still must not do is keep spending
    // them on a tab nobody is looking at.
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
