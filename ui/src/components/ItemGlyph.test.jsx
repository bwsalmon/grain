import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ITEM_GLYPH_PATHS } from "../brand/item-glyph-paths.js";
import { ITEM_KINDS } from "../brand/item-glyphs.js";
import ItemGlyph from "./ItemGlyph.jsx";

const glyphOf = (container) => container.querySelector("svg");

describe("ItemGlyph", () => {
  it("draws each kind's own figure", () => {
    const drawn = new Set();
    for (const kind of ITEM_KINDS) {
      const { container } = render(<ItemGlyph kind={kind} />);
      const svg = glyphOf(container);
      expect(svg).toHaveAttribute("data-glyph", kind);
      const d = svg.querySelector("path").getAttribute("d");
      expect(d).toBe(ITEM_GLYPH_PATHS[kind]);
      drawn.add(d);
    }
    // Four kinds, four different paths -- not one figure reused.
    expect(drawn.size).toBe(ITEM_KINDS.length);
  });

  it("takes the colour of the text it sits beside", () => {
    // The whole reason these are inline SVG rather than an <img>: a
    // selected nav entry brightens its label, and the glyph goes with
    // it, in either theme and with no second asset.
    const { container } = render(<ItemGlyph kind="repos" />);
    expect(glyphOf(container).querySelector("path")).toHaveAttribute(
      "fill",
      "currentColor",
    );
  });

  it("is decorative beside its own label, and named when it stands alone", () => {
    const { container: beside } = render(<ItemGlyph kind="suites" />);
    expect(glyphOf(beside)).toHaveAttribute("aria-hidden", "true");

    const { container: alone } = render(
      <ItemGlyph kind="suites" title="Suites" />,
    );
    expect(glyphOf(alone)).not.toHaveAttribute("aria-hidden");
    expect(glyphOf(alone)).toHaveAttribute("aria-label", "Suites");
  });

  it("renders at the size asked for", () => {
    const { container } = render(<ItemGlyph kind="schedules" size={24} />);
    expect(glyphOf(container)).toHaveAttribute("width", "24");
    expect(glyphOf(container)).toHaveAttribute("height", "24");
  });

  it("draws nothing for a kind with no figure", () => {
    // A new list page renders before anyone has cut it a glyph, rather
    // than throwing on the way past.
    const { container } = render(<ItemGlyph kind="nothing-yet" />);
    expect(glyphOf(container)).toBeNull();
  });
});
