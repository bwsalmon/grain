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
//
// Two sizes, and no more: 16 inline -- a menu row, a chip, a value in
// the task pane's property column -- and 20 for a list heading
// (ListPrimitives' own `icon`). The figures are framed to read as one
// shape at 16px (item-glyphs.js's zoom), so a third size would be a
// third framing question nobody has answered.
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

// GlyphLabel is one glyph in front of one label: the placement every
// glyph outside the nav rail and the four list headings uses -- a menu
// row naming a template or a suite, a repo in a picker's results, the
// review template named in a task's property column. Inline at 16px,
// decorative (the label is right there), and laid out here rather than
// at each call site so ten of them cannot drift into ten spacings.
//
// `kind` may be null, for a row sitting in a list with the glyphed ones
// but naming something that is not one of the four kinds -- "A task"
// beside "A suite", "None -- fill in the fields below" beside the
// templates. The slot is held open and empty instead, the same trick
// TaskList's own drag placeholder plays: an icon on some rows and
// nothing at all on others starts the labels of one menu in two
// different columns, which is worse than a missing figure.
export function GlyphLabel({ kind, size = 16, children }) {
  return (
    <span
      className="glyph-label"
      style={{ display: "inline-flex", alignItems: "center", minWidth: 0 }}
    >
      {kind ? (
        <ItemGlyph kind={kind} size={size} />
      ) : (
        <span
          aria-hidden="true"
          style={{ width: size, height: size, flex: "none" }}
        />
      )}
      <span style={{ marginLeft: "0.45rem", minWidth: 0 }}>{children}</span>
    </span>
  );
}
