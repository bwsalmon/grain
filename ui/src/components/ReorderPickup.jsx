import { useCallback, useEffect, useState } from "react";
import { Button } from "@mui/material";
import DragIndicatorIcon from "@mui/icons-material/DragIndicator";

// ReorderPickup is reordering for a finger: pick a row up, then tap
// where it goes (grain/task-23).
//
// # Why a second gesture at all
//
// Every list in this UI that has an order of its own -- the backlog in
// TaskList and TaskBoard, the four display orders in
// ListPrimitives.jsx's ReorderableList -- is reordered by dragging, and
// dragging here means the HTML5 drag events: draggable rows,
// dragstart/dragover/drop. A phone browser fires none of them. A touch
// on a draggable row scrolls the page, exactly as it would on any other
// row, and no amount of patience turns it into a drag.
//
// So the handle was hidden on phones and the order was simply not
// editable there (the rule this replaces in style.css said as much in
// as many words). That is a real hole: the backlog is the one order in
// grain that decides what actually runs next, and "which task runs
// next" is the archetypal thing somebody settles from a phone.
//
// # Why tapping rather than a touch drag
//
// The other way to close the hole is to reimplement dragging on pointer
// events -- long-press to lift, follow the finger, hit-test the row
// under it, auto-scroll near the edges. It is a lot of machinery, none
// of it is testable in jsdom (no layout, no elementFromPoint), and it
// fights the browser for the same gesture the page uses to scroll.
//
// Picking up and putting down costs two taps and needs none of that:
//
//   - it is two ordinary buttons, so it works with a finger, a mouse, a
//     keyboard and a screen reader without a line of code per input
//     device. Reordering was drag-only until now, which means it was
//     also unreachable from the keyboard;
//   - the drop targets are visible and the size of a thumb, rather than
//     a 2px rule you have to hold a finger over something to find;
//   - the list still scrolls while a row is picked up, which is what a
//     long list needs and what a touch drag takes away.
//
// The existing drag is untouched. On a mouse it remains the faster
// gesture for a short move, and every drag test still describes it; this
// is the gesture that works where that one cannot, offered on the same
// handle so there is one affordance to find rather than two.
//
// # What the pieces are
//
// usePickup holds "what is picked up", MoveHandle is the affordance that
// picks it up, MoveSlot is one of the places it can go, and MovingBar is
// the way out. The lists themselves keep deciding what a slot *means* --
// TaskList hands its neighbours to Store.Reorder, ReorderableList
// rewrites a stored display order -- because that is the part no two of
// them agree on.

// usePickup is the state behind the gesture: null when nothing is picked
// up, or { ids, scope } for the block of rows that is.
//
// `scope` is whatever the caller needs to know *where* the rows were
// lifted from -- TaskBoard passes the column id, because a card can only
// be put back into the column it came from (a task's state is derived,
// not set, so there is no such thing as dropping it into "Running"), and
// the lists with one flat order pass nothing.
//
// Escape cancels from anywhere, the way it closes an overlay: once a row
// is up, every slot on screen is a button, and a picked-up row somebody
// has changed their mind about should not need them to find the one
// button that says so.
export function usePickup() {
  const [picked, setPicked] = useState(null);

  useEffect(() => {
    if (!picked) return undefined;
    const onKey = (e) => {
      if (e.key === "Escape") setPicked(null);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [picked]);

  // cancel and toggle keep the same identity for the life of the list,
  // so a caller can drop them into an effect's dependencies -- which is
  // what every list here does to put a picked-up row down again when the
  // sort it was picked up under stops being the one on screen.
  const cancel = useCallback(() => setPicked(null), []);

  // toggle is what a handle does: pick this block up, or put it back
  // down untouched if it is the block already up. Tapping the same
  // handle twice is the obvious way to change your mind, and it is the
  // only way that needs nothing but the row you started from.
  const toggle = useCallback(
    (ids, scope = null) =>
      setPicked((p) => (p && sameIds(p.ids, ids) ? null : { ids, scope })),
    [],
  );

  return { picked, toggle, cancel };
}

function sameIds(a, b) {
  return a.length === b.length && a.every((id, i) => id === b[i]);
}

// gapsOf turns "these rows are picked up" into the drop slots worth
// offering, as indexes into `kept` (the rows in display order with the
// picked ones taken out): slot g puts the block between kept[g-1] and
// kept[g], and the slot at kept.length puts it at the end.
//
// The gap the block already occupies is left out. Offering it would be a
// button that visibly promises a move and does nothing, which reads as a
// bug in the list rather than as a no-op -- and for a single row picked
// up out of the middle of a list, that dead slot is one of the two
// sitting right next to the row somebody just touched.
export function gapsOf(order, pickedIds) {
  const picked = new Set(pickedIds);
  const kept = order.filter((id) => !picked.has(id));
  const gaps = [];
  for (let g = 0; g <= kept.length; g += 1) {
    const next = [...kept.slice(0, g), ...pickedIds, ...kept.slice(g)];
    if (!sameIds(next, order)) gaps.push(g);
  }
  return gaps;
}

// MoveHandle is the handle every reorderable row already carried, as a
// button: the same figure in the same column, and still the thing a
// mouse grabs to drag, but now also the thing a finger taps to lift the
// row. aria-pressed says which of the two states it is in, so a screen
// reader announces the pick-up rather than leaving the slots that appear
// underneath unexplained.
//
// The click never reaches the row (all five lists open the thing the row
// names when it is clicked), which is the same stopPropagation the plain
// icon needed before this was a button at all.
export function MoveHandle({ label, picked, onToggle }) {
  return (
    <button
      type="button"
      className={`task-drag-handle${picked ? " task-drag-handle-picked" : ""}`}
      aria-label={label}
      aria-pressed={picked}
      title={
        picked
          ? "Tap a “Move here” slot, or tap again to leave it where it is"
          : "Drag to reorder, or tap to pick this up and choose where it goes"
      }
      onClick={(e) => {
        e.stopPropagation();
        onToggle();
      }}
    >
      <DragIndicatorIcon fontSize="small" />
    </button>
  );
}

// MoveSlot is one place the picked-up rows can go: an <li> of its own
// between two rows, because that is where the block would land and a
// list is the only element allowed between a <ul> and its rows.
//
// It is a full-width button rather than a hairline, and its height is a
// touch target rather than a rule's: the whole point of this gesture is
// that the drop targets are things you can hit with a thumb.
export function MoveSlot({ label, className, onMove }) {
  return (
    <li className={`reorder-slot${className ? ` ${className}` : ""}`}>
      <button
        type="button"
        className="reorder-slot-button"
        aria-label={label}
        onClick={(e) => {
          e.stopPropagation();
          onMove();
        }}
      >
        Move here
      </button>
    </li>
  );
}

// MovingBar says out loud what is happening and offers the way out.
// role="status" so a screen reader hears it when it appears, and sticky
// (style.css) so the way out is still on screen after scrolling a long
// list looking for the right slot.
export function MovingBar({ count, noun = "task", onCancel }) {
  return (
    <div className="reorder-bar" role="status">
      <span className="reorder-bar-note">
        Moving {count} {count === 1 ? noun : `${noun}s`} — choose where{" "}
        {count === 1 ? "it goes" : "they go"}
      </span>
      <Button size="small" className="reorder-bar-cancel" onClick={onCancel}>
        Cancel
      </Button>
    </div>
  );
}
