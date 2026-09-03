import { useState } from "react";
import { Alert, Button } from "@mui/material";
import api from "../api.js";

// AgentPauseBanner is what config.agentPause (ui.AgentPauseStatus,
// orchestrator.Pause behind it) becomes on screen: the deployment's
// agent has said it has no budget left in this window, so every run in
// flight was cancelled and nothing is being dispatched until the window
// resets.
//
// It exists because that fact was otherwise invisible from the UI. An
// operator opening grain in the middle of a five-hour Claude window saw
// a queue of ready tasks with nothing running and no explanation
// anywhere except the daemon's journal or the detail of an attempt they
// would have to know to open (grain/task-132).
//
// A standing banner, not a five-second ErrorBanner toast, and pinned
// with ReconcilerDownBanner's own layout for the same reason: like a
// dead reconcile loop this describes the whole deployment for as long as
// it lasts, and it should stay on screen whichever page is showing.
// Warning rather than error, though -- unlike a dead loop, nothing here
// is broken or needs a restart: grain is waiting out a provider's limit
// on purpose, and will resume by itself.
export default function AgentPauseBanner({ pause, onLifted, showError }) {
  const [lifting, setLifting] = useState(false);

  async function lift() {
    setLifting(true);
    try {
      await api("/api/pause", { method: "DELETE" });
      // Refresh rather than hide this locally: the pause lives in the
      // daemon, and config is polled from one place (App.jsx). Letting
      // that poll be what clears the banner keeps the screen showing
      // what the deployment actually says -- including the case where a
      // run dispatched straight after the lift met the same limit again
      // and closed the gate a second time.
      await onLifted?.();
    } catch (err) {
      showError?.(err.message);
    } finally {
      setLifting(false);
    }
  }

  return (
    <Alert
      severity="warning"
      variant="filled"
      sx={{ position: "fixed", top: 0, left: 0, right: 0, zIndex: 5, borderRadius: 0, justifyContent: "center" }}
      action={
        // "Resume now" is for the operator who has just topped a plan
        // up, or moved this deployment onto the other agent framework:
        // they know something the daemon cannot, which is that the
        // credential behind the refusal is not the one the next run
        // would spend (ui.Client.LiftAgentPause).
        <Button color="inherit" size="small" disabled={lifting} onClick={lift}>
          {lifting ? "Resuming…" : "Resume now"}
        </Button>
      }
    >
      {pauseMessage(pause)}
    </Alert>
  );
}

// pauseMessage is the sentence on the banner: what grain is doing, until
// when, and -- last -- what the provider itself said, which is the part
// that names the framework and the window and is worth keeping verbatim
// rather than paraphrasing.
//
// The instant is shown in the reader's own locale as well as as a
// remaining duration: a countdown answers "is this nearly over?", and a
// wall-clock time is what somebody deciding whether to wait or to go to
// lunch actually plans around.
export function pauseMessage(pause) {
  const until = pause?.until ? new Date(pause.until) : null;
  const when = until && !Number.isNaN(until.getTime())
    ? `${until.toLocaleTimeString()} (${formatRemaining(pause.secondsRemaining)})`
    : "the provider's window resets";
  const reason = pause?.reason ? ` — ${pause.reason}` : "";
  return `Agent usage limit reached: nothing is being dispatched until ${when}.${reason}`;
}

// formatRemaining renders a countdown coarsely, in hours and minutes.
//
// Deliberately not MetricsPage's own formatSeconds, which carries
// seconds under an hour: this number is re-read every poll, and a banner
// whose text changes every three seconds is a banner that flickers
// while somebody is trying to read it. Nothing is decided on the last
// minute of a five-hour window anyway.
export function formatRemaining(seconds) {
  const s = Math.max(0, Math.round(seconds || 0));
  if (s < 60) return "less than a minute";
  const hours = Math.floor(s / 3600);
  const minutes = Math.floor((s % 3600) / 60);
  if (hours === 0) return `about ${minutes}m`;
  return `about ${hours}h ${minutes}m`;
}
