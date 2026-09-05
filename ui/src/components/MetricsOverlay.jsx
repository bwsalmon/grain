import { Typography } from "@mui/material";
import Overlay from "./Overlay.jsx";
import MetricsPage from "./MetricsPage.jsx";

// MetricsOverlay is the throughput and latency report's own sidebar
// destination (grain/task-173), at /metrics.
//
// It was the fourth tab of SystemOverlay.jsx until now, on the reasoning
// that it is the same kind of thing Logs, Sandbox health and Top are: a
// read-only, deployment-wide view rather than a knob. That much is still
// true, but it is not the same *question*. The other three are opened
// because something is wrong right now -- a task is stuck, the machine
// is loaded, an agent is failing -- and read for as long as that lasts.
// This one is opened because nothing is wrong: how much did we get
// through this week, where is a task's wall-clock time going, is the
// backlog growing. Filing that under the word "debug" -- what that
// pane was called then -- two clicks and a tab strip deep, told an
// operator to open it at the wrong times and
// priced it wrong for the times it is actually for.
//
// So it takes a nav entry beside Settings and System, and MetricsPage
// itself no longer prints its own "Metrics" heading -- the pane header
// below is what names it now, the same way Settings' and System's own
// headers name theirs.
//
// onOpenTask is the one link out of the report: the backlog names the
// oldest queued task, and the useful thing to do with that is go and
// look at it. App closes this pane on the way, since two stacked panes
// would put the task behind the one it was opened from.
//
// pane, and nothing capping the width inside it, for the reason
// SystemOverlay gives: the latency and tool-use tables are more columns
// across than a centered dialog has room for.
export default function MetricsOverlay({ onClose, onOpenTask, showError }) {
  const header = (
    <Typography variant="h6" component="h2" sx={{ mt: 0 }}>
      Metrics
    </Typography>
  );
  return (
    <Overlay onClose={onClose} pane header={header}>
      <MetricsPage showError={showError} onOpenTask={onOpenTask} />
    </Overlay>
  );
}
