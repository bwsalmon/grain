import GrainMark from "./GrainMark.jsx";

// The circle every per-task state badge opens with. Every state but one
// is the plain solid dot .badge::before already draws in CSS; a task
// that is actually running right now swaps it for the grain mark's
// glyph cycle instead -- the same "agents are working" signal the
// sidebar shows (Sidebar.jsx), now down at the row that is doing the
// work (bwsalmon/agents#586).
//
// 20px, well above the 11px .badge::before dot it replaces: the v2 mark
// is a filled region rather than a line figure, and under about 20px
// there are not enough pixels across the glyph for the four to be
// tellable apart. The task row is ~40px tall and its text line is
// ~20px, so this costs the row no height.
//
// live=false is for a badge recording a past "running" moment rather
// than the task's live status (Timeline's superseded entries) -- that
// one keeps the plain, non-spinning solid dot .badge-static draws, the
// same as every other historical state.
const MARK_SIZE = 20;

export function isLiveRunning(state, live = true) {
  return state === "running" && live;
}

export default function StateDot({ state, live = true, title }) {
  if (!isLiveRunning(state, live)) return null;
  return <GrainMark size={MARK_SIZE} animated title={title} />;
}
