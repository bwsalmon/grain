import { useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// PromptOverlay is the whole prompt a task's agent was actually handed
// (grain/task-91): its title and description plus everything grain adds
// to them on the way out -- which repo and branch to push, the
// conversation so far, the attachments materialized into the sandbox,
// each capability's own section, and the follow-on task etiquette
// (pkg/orchestrator's BuildPrompt and prepareCapabilities). None of that
// is visible anywhere else, so a task that behaved oddly could only be
// explained by guessing at what it was told.
//
// It shows one attempt's prompt, the most recent one that recorded any
// (ui.Client.TaskPrompt): the prompt is assembled per run, out of a task
// that may since have been edited and a conversation that has since
// grown, so there is no single prompt for a task -- only the latest
// answer to "what would/did an agent see".
//
// Fetched once, not polled: unlike AttemptTranscriptOverlay's own
// transcript, a run's prompt is written before its agent's first turn
// and never changes after (Store.SetRunPrompt), so there is nothing for
// a second fetch to pick up.
export default function PromptOverlay({ taskId, onClose }) {
  const [prompt, setPrompt] = useState(null);
  const [attempt, setAttempt] = useState(0);
  // Reported in the pane itself rather than through App.jsx's own
  // showError banner: an error about the prompt belongs where the prompt
  // was going to be, in the pane the reader just opened, rather than in
  // a banner behind the task page this opens over.
  const [error, setError] = useState(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await api(
          `/api/tasks/${encodeURIComponent(taskId)}/prompt`,
        );
        if (cancelled) return;
        setPrompt(res.prompt || "");
        setAttempt(res.attempt || 0);
      } catch (err) {
        if (!cancelled) setError(err);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [taskId]);

  return (
    <Overlay onClose={onClose} wide>
      <h2>
        Prompt for {taskId}
        {attempt > 0 && <span className="hint"> · attempt #{attempt}</span>}
      </h2>
      <pre className="logs-view">
        {error
          ? `Could not load this task's prompt: ${error.message}`
          : prompt === null
            ? "Loading…"
            : prompt ||
              "No prompt recorded yet -- nothing has been dispatched for this task, " +
                "so grain has not built one. A prompt is recorded the moment a run hands it to its agent."}
      </pre>
    </Overlay>
  );
}
