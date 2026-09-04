import { ITEM_GLYPH_BOX, ITEM_GLYPH_PATHS } from "../brand/item-glyph-paths.js";

// ItemGlyph draws one item glyph: the Chladni figure standing for a kind
// of thing grain keeps a list of -- a repo, a schedule, a template, a
// suite. src/brand/item-glyphs.js is which figure each kind gets and
// why; docs/brand.md is the design.
//
// Inline SVG rather than an <img> or a mask, for one reason: `fill:
// currentColor` makes the glyph the colour of the text beside it, so a
// selected nav entry's icon brightens with its label and both themes
// are served without a second asset. It is also one path with no
// clipPath -- the frame is met while tracing (item-glyphs.js's
// itemFillAt) -- so there is no id to collide with on a page rendering
// several of these.
//
// Decorative by default: every glyph in the app today sits next to its
// own word ("Repos", "Schedules"), and a screen reader announcing the
// figure as well would only say the label twice. Pass `title` where one
// stands alone.
export default function ItemGlyph({ kind, size = 16, title, className }) {
  const d = ITEM_GLYPH_PATHS[kind];
  if (!d) return null;
  return (
    <svg
      className={className}
      data-glyph={kind}
      width={size}
      height={size}
      viewBox={`0 0 ${ITEM_GLYPH_BOX} ${ITEM_GLYPH_BOX}`}
      role="img"
      aria-hidden={title ? undefined : "true"}
      aria-label={title || undefined}
      focusable="false"
      style={{ display: "block", flex: "none" }}
    >
      {title ? <title>{title}</title> : null}
      <path d={d} fill="currentColor" fillRule="evenodd" />
    </svg>
  );
}
