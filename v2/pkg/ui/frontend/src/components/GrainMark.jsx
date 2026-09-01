import { useEffect, useRef, useState } from "react";
import { useTheme } from "@mui/material";

import { DEFAULTS, GLYPHS, STATIC_SLOT, createGrainMark, renderStatic } from "../brand/grain-mark.js";
import { DENSE_BELOW_PX, markSpec } from "../brand/grain-density.js";

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
//   animated  the mark moving between the four glyphs, which is what
//             grain shows while agents are actually working. It opens
//             on the rosette -- the figure the still was showing -- so
//             the change reads as the mark coming to life rather than
//             as a different image.
//
// The animation comes in two forms, split at the same 48px the grain
// density is (grain-density.js).
//
// Below it the mark **holds its dwell crisp**: it settles, sharpens into
// the solid glyph, dissolves back into sand to move, and re-forms. A
// stipple standing still at icon size is the thing that reads as mush,
// and this is what takes that away without giving up the flight.
//
// The crisp state is a second canvas under the first, carrying the
// module's own solid render of whichever glyph the mark last landed on.
// It cannot be the still <img> the idle mark uses, tempting as that is:
// that file is the rosette and only the rosette, so a mark that had just
// flown to the diamond would sharpen into the wrong figure. On the
// rosette the two are the same geometry either way -- one traced from
// `fillScalar`, one supersampled from it -- so a task that stops running
// still changes nothing about the picture, it only stops it moving.
//
// The fade is doing real work, not softening a seam. sampleGlyph places
// grain *centres* inside the region, so every grain bleeds a radius past
// its edge and the settled cloud is the solid glyph dilated by that
// much: at 20px it carries about three quarters again as much ink, with
// the rosette's holes nearly closed. Cutting between the two would be a
// visible pop four times a loop. Faded, the same difference is the point
// of the thing -- the mark reads as loose while the sand is in the air
// and tight once it lands.
//
// Above 48px the mark runs the pack's own uninterrupted cycle. Not
// because crisping would be invisible there -- the gap is a thin outline
// at hero scale and would fade cleanly -- but because at that size the
// stipple *is* the picture, and flattening it to a fill four times a
// loop would throw away the texture that makes the mark worth showing
// large.
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

// How long a small mark holds its glyph crisp between flights.
//
// Not the pack's 0.33s dwell, which is a beat between two grain fields
// rather than a state: held that briefly the solid glyph would read as a
// flicker in the flight rather than as the mark settling. At 0.9s the
// loop is about 2.6s a glyph and 10s round -- slower than the pack's
// 6.5s, which is the cost of the settled state being worth looking at,
// and no loss for a signal that only has to say "something is running".
const CRISP_MS = 900;

// The cross-fades. Settling is the slower of the two because it is the
// one being watched -- the grains have already stopped by then, so the
// fade is the whole of the motion. Dissolving runs under the first
// stretch of a flight that is already moving, and only has to not be a
// cut.
const SETTLE_MS = 240;
const DISSOLVE_MS = 160;

export default function GrainMark({ size = 28, animated = false, title, className }) {
  const theme = useTheme();
  const mode = theme.palette.mode === "dark" ? "dark" : "light";
  const reducedMotion = usePrefersReducedMotion();
  const canvasRef = useRef(null);
  const solidRef = useRef(null);
  // Below the threshold the animation crisps between flights, which is
  // what puts a second canvas under the grains to crisp onto.
  const crisps = size < DENSE_BELOW_PX;
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

    // Above the threshold the pack drives itself: one call, and it
    // cycles until told otherwise.
    if (!crisps) {
      const onVisibility = () => (document.hidden ? mark.stop() : mark.start());
      document.addEventListener("visibilitychange", onVisibility);
      onVisibility();
      return () => {
        document.removeEventListener("visibilitychange", onVisibility);
        mark.destroy();
      };
    }

    // Below it, grain drives the cycle a flight at a time so it knows
    // when each one lands. `setMode` runs a single flight and then
    // stops: the module's loop only keeps going while its auto flag is
    // set, and `start()` is what sets it, so never calling start hands
    // the clock over here. A parked module also stops painting, which
    // is what lets the canvas be faded out between flights and cost
    // nothing while it is.
    const solid = solidRef.current;
    solid.width = canvas.width;
    solid.height = canvas.height;
    let slot = STATIC_SLOT;
    let timer = null;

    const fade = (grains, ms) => {
      for (const [el, shown] of [
        [canvas, grains],
        [solid, !grains],
      ]) {
        el.style.transition = `opacity ${ms}ms ease`;
        el.style.opacity = shown ? "1" : "0";
      }
    };

    const crisp = () => {
      // Drawn per flight rather than once, because the glyph under the
      // grains is whichever one they just landed on.
      renderStatic(solid, { slot, style: "solid", theme: mode });
      fade(false, SETTLE_MS);
      timer = setTimeout(fly, CRISP_MS);
    };
    const fly = () => {
      fade(true, DISSOLVE_MS);
      slot = (slot + 1) % GLYPHS.length;
      mark.setMode(slot);
      // A beat past the flight's own length, so the grains have landed
      // before the mark starts sharpening onto them.
      timer = setTimeout(crisp, DEFAULTS.snapSeconds * 1000 + 60);
    };
    const halt = () => {
      clearTimeout(timer);
      timer = null;
      mark.stop();
    };

    // The pack's own note on this renderer is that it is a proof of
    // concept meant for a splash or a hero, not something left running
    // as a persistent UI element -- a fair warning for a mark that
    // animates for as long as anything is running, which on a busy
    // deployment is most of the day. Holding the dwell as a still is the
    // one thing here that gives some of that back: a crisped mark paints
    // nothing at all until its next flight. What it must still not do is
    // spend frames on a tab nobody is looking at.
    const onVisibility = () => (document.hidden ? halt() : timer || crisp());
    document.addEventListener("visibilitychange", onVisibility);
    onVisibility();

    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      clearTimeout(timer);
      mark.destroy();
    };
  }, [animated, reducedMotion, size, mode, crisps]);

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

  const layer = { position: "absolute", inset: 0, width: "100%", height: "100%" };

  return (
    <span
      className={className}
      role="img"
      aria-label={label}
      title={label}
      style={{ position: "relative", display: "block", width: size, height: size, flex: "none" }}
    >
      {/* Under the grains, and only where the mark crisps: the settled
          glyph, solid. It starts hidden so the first thing painted is
          the grain field the module lays out on construction, and the
          effect fades it in once they land. */}
      {crisps && <canvas ref={solidRef} style={{ ...layer, opacity: 0 }} />}
      <canvas ref={canvasRef} style={layer} />
    </span>
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
