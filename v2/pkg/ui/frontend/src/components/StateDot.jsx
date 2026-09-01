import GrainMark from "./GrainMark.jsx";

// The circle every per-task state badge opens with. Every state but one
// is the plain solid dot .badge::before already draws in CSS; a task
// that is actually running right now swaps it for the grain mark's tiny
// cycle instead -- the same "agents are working" signal the sidebar
// shows (Sidebar.jsx), now down at the row that is doing the work
// (bwsalmon/agents#586). docs/brand.md already lists "badge" among the
// tiny tier's intended homes.
//
// live=false is for a badge recording a past "running" moment rather
// than the task's live status (Timeline's superseded entries) -- that
// one keeps the plain, non-spinning solid dot .badge-static draws, the
// same as every other historical state.
const MARK_SIZE = 14;

export function isLiveRunning(state, live = true) {
  return state === "running" && live;
}

export default function StateDot({ state, live = true, title }) {
  if (!isLiveRunning(state, live)) return null;
  return <GrainMark size={MARK_SIZE} animated title={title} />;
}
