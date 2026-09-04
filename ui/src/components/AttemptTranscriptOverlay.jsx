import { useCallback, useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// REFRESH_MS mirrors LogsOverlay's own polling interval (bwsalmon/
// agents#444). For a finished attempt this is just re-fetching the same
// Store.RunTranscript row RunDispatch wrote once, after framework.Run
// returned (pkg/orchestrator/run.go) -- but while an attempt is still
// running, the backend serves whatever its framework has mirrored to a
// live transcript file so far (ui.Config.LiveTranscripts,
// bwsalmon/agents#467), so each poll here genuinely picks up new content
// rather than re-reading an empty row until the attempt finishes. Leaving
// this open on a long-running attempt is what "checking up on a
// long-running agent" (bwsalmon/agents#446) means in practice.
const REFRESH_MS = 5000;

// AttemptTranscriptOverlay is what typing on a task attempt in
// DetailOverlay's own Timeline opens: the single scrolling pane over
// that attempt's whole agent transcript -- thinking, text and every tool
// call interleaved, in the order the agent produced them
// (pkg/agent/claude/transcript.go's own doc comment on how it is built)
// -- useful for debugging why an attempt failed, or for watching one
// still in flight without reaching for a shell (bwsalmon/agents#446,
// bwsalmon/agents#467).
export default function AttemptTranscriptOverlay({
  taskId,
  attempt,
  onClose,
  showError,
}) {
  const [transcript, setTranscript] = useState(null);

  const refresh = useCallback(async () => {
    try {
      const res = await api(
        `/api/tasks/${encodeURIComponent(taskId)}/attempts/${attempt.number}/transcript`,
      );
      setTranscript(res.transcript || "");
    } catch (err) {
      showError(err);
    }
  }, [taskId, attempt.number, showError]);

  useEffect(() => {
    refresh();
  }, [refresh]);

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
          : transcript ||
            (attempt.finishedAt
              ? "(no transcript recorded)"
              : "Still running -- nothing written yet.")}
      </pre>
    </Overlay>
  );
}
