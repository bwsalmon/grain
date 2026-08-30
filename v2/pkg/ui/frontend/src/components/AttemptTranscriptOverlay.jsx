import { useCallback, useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// REFRESH_MS mirrors LogsOverlay's own polling interval (bwsalmon/
// agents#444) -- but unlike a system log, an attempt's transcript is
// written once, by RunDispatch's own SetRunTranscript call, only after
// framework.Run returns (pkg/orchestrator/run.go). Polling while the
// attempt is still running is not tailing a growing file; it is waiting
// for that one write to land, which is what actually makes "checking up
// on a long-running agent" (bwsalmon/agents#446) work from here: leave
// this open and the transcript appears the moment the attempt finishes.
const REFRESH_MS = 5000;

// AttemptTranscriptOverlay is what typing on a task attempt in
// DetailOverlay's own Timeline opens: the single scrolling pane over
// that attempt's whole agent transcript -- thinking, text and every tool
// call interleaved, in the order the agent produced them
// (pkg/agent/claude/transcript.go's own doc comment on how it is built)
// -- useful for debugging why an attempt failed, or for watching one
// still in flight without reaching for a shell (bwsalmon/agents#446).
export default function AttemptTranscriptOverlay({ taskId, attempt, onClose, showError }) {
  const [transcript, setTranscript] = useState(null);

  const refresh = useCallback(async () => {
    try {
      const res = await api(`/api/tasks/${encodeURIComponent(taskId)}/attempts/${attempt.number}/transcript`);
      setTranscript(res.transcript || "");
    } catch (err) {
      showError(err);
    }
  }, [taskId, attempt.number, showError]);

  useEffect(() => { refresh(); }, [refresh]);

  useEffect(() => {
    if (attempt.finishedAt) return;
    const interval = setInterval(refresh, REFRESH_MS);
    return () => clearInterval(interval);
  }, [attempt.finishedAt, refresh]);

  return (
    <Overlay onClose={onClose} wide>
      <h2>Attempt #{attempt.number} transcript</h2>
      <pre className="logs-view">
        {transcript === null
          ? "Loading…"
          : transcript || (attempt.finishedAt
            ? "(no transcript recorded)"
            : "Still running -- the transcript appears once this attempt finishes.")}
      </pre>
    </Overlay>
  );
}
