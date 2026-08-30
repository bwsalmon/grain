import { useRef } from "react";
import api from "../api.js";
import { STATE_LABELS } from "../state.js";
import Overlay from "./Overlay.jsx";
import TaskPicker from "./TaskPicker.jsx";

// The panel splits like Plane's own issue peek: title, description and
// the conversation in a main column, everything about the task's current
// state and its declared shape (repo, capabilities, dependencies) in a
// narrow property column beside it.
export default function DetailOverlay({ task: t, tasks, config, onClose, onOpenTask, act }) {
  return (
    <Overlay onClose={onClose} className="panel-detail">
      <div className="detail-layout">
        <div className="detail-main">
          <div className="detail-header">
            <h2>{t.id} {t.title}</h2>
          </div>

          <div className="freshness">
            as of just now
            {t.pullRequest && <> · <a href={pullRequestUrl(t.pullRequest)} target="_blank" rel="noopener noreferrer">{t.pullRequest}</a></>}
            {t.generatedFrom && (
              <>
                {" "}· generated from{" "}
                <a href="#" onClick={(e) => { e.preventDefault(); onOpenTask(t.generatedFrom); }}>{t.generatedFrom}</a>
              </>
            )}
          </div>

          <div className="description">{t.description || "(no description)"}</div>

          {t.state === "failed" && (
            <div className="failure-summary">
              <strong>{t.failedAttempts} consecutive failed attempt{t.failedAttempts === 1 ? "" : "s"}.</strong>
              {t.lastFailureReason && <> Last failure: {t.lastFailureReason}</>}
            </div>
          )}

          <Comments t={t} act={act} />
        </div>

        <div className="detail-side">
          <div className="detail-state">
            <span className={`badge badge-${t.state}`}>{STATE_LABELS[t.state] || t.state}</span>
            {/* Blocked reads as an annotation beside the state dot, not a
                replacement for it -- a blocked task is still queued
                (docs/data-model.md). */}
            {t.blocked && <span className="badge badge-blocked">Blocked</span>}
          </div>

          <Actions t={t} act={act} />
          <Attempts t={t} />
          <Declared t={t} />
          <CapabilityToggles t={t} config={config} act={act} />
          <Dependencies t={t} tasks={tasks} act={act} onOpenTask={onOpenTask} />
        </div>
      </div>
    </Overlay>
  );
}

// pullRequestUrl turns "owner/name#123" (t.pullRequest, straight off
// model.PullRequestRef.String()) into the GitHub URL it names.
function pullRequestUrl(ref) {
  const [repo, number] = ref.split("#");
  return `https://github.com/${repo}/pull/${number}`;
}

// Real columns on the task now, not directive lines parsed out of a
// body -- so they are rendered as fields rather than as the /repo,
// /base, /auto-merge syntax they used to have to be written in.
function Declared({ t }) {
  const rows = [];
  if (t.repo) rows.push(["Repo", t.repo]);
  if (t.base) rows.push(["Base", t.base]);
  if (t.reads && t.reads.length > 0) rows.push(["Reads", t.reads.join(", ")]);
  rows.push(["Auto-merge", String(t.autoMerge)]);
  return (
    <div className="declared">
      {rows.map(([key, value]) => (
        <div className="declared-row" key={key}>
          <span className="declared-key">{key}</span>
          <span className="declared-value">{value}</span>
        </div>
      ))}
    </div>
  );
}

function Actions({ t, act }) {
  return (
    <div className="actions">
      {t.state === "proposed" && (
        <button className="primary" onClick={() => act(() => api(`/api/tasks/${t.id}/approve`, { method: "POST" }), t.id)}>
          Approve
        </button>
      )}
      {/* Once a task's run has produced a pull request, submitting is
          what puts it on the merge queue for automatic conflict
          resolution and merging. Already-submitted tasks (autoMerge
          already true) have nothing left for this button to do. */}
      {t.pullRequest && !t.autoMerge && (
        <button className="primary" onClick={() => act(() => api(`/api/tasks/${t.id}/submit`, { method: "POST" }), t.id)}>
          Submit
        </button>
      )}
      {t.state === "failed" && (
        <button className="primary" onClick={() => act(() => api(`/api/tasks/${t.id}/retry`, { method: "POST" }), t.id)}>
          Retry
        </button>
      )}
      {t.state === "closed" ? (
        <button className="secondary" onClick={() => act(() => api(`/api/tasks/${t.id}/reopen`, { method: "POST" }), t.id)}>
          Reopen
        </button>
      ) : t.state === "running" ? (
        // Closing a running task stops it from ever being re-dispatched
        // or opened as a pull request, and cancels the run in flight --
        // "Cancel" is that same close call, surfaced under a name that
        // matches what a running task's close button actually does.
        <button
          className="danger secondary"
          onClick={() => {
            if (!confirm("Cancel this job? Its run will be abandoned: no pull request will be opened for it.")) return;
            act(() => api(`/api/tasks/${t.id}/close`, { method: "POST" }), t.id);
          }}
        >
          Cancel
        </button>
      ) : (
        <button className="danger secondary" onClick={() => act(() => api(`/api/tasks/${t.id}/close`, { method: "POST" }), t.id)}>
          Close
        </button>
      )}
    </div>
  );
}

// OUTCOME_LABELS and OUTCOME_BADGES cover model.Run's own outcome
// vocabulary (orchestrator.outcomeOf, orchestrator.run's "cancelled"),
// plus the empty string a run still in flight (no finishedAt yet) comes
// back as -- the one case that isn't itself an outcome. An outcome this
// doesn't recognise falls back to the raw string, capitalised, rather
// than disappearing, so a value added on the backend later still shows
// up here before this map catches up with it.
const OUTCOME_LABELS = { "": "Running", succeeded: "Succeeded", failed: "Failed", cancelled: "Cancelled" };
const OUTCOME_BADGES = { "": "running", succeeded: "completed", failed: "failed", cancelled: "closed" };

// outcome is undefined, not "", for a still-running attempt: the API's
// own Attempt.Outcome carries `omitempty`, so the wire form drops the
// key entirely rather than sending an empty string.
function outcomeLabel(outcome) {
  outcome = outcome || "";
  return OUTCOME_LABELS[outcome] || (outcome.charAt(0).toUpperCase() + outcome.slice(1));
}

// Attempts is every run this task has had, oldest first, each with its
// own status and timing -- bwsalmon/agents#445's "show attempts and
// their status in order in the task view", the full history
// t.failedAttempts only counts and t.lastFailureReason only explains for
// the most recent one.
function Attempts({ t }) {
  const attempts = t.attempts || [];
  if (attempts.length === 0) return null;
  return (
    <fieldset>
      <legend>Attempts ({attempts.length})</legend>
      <ul className="attempts">
        {attempts.map((a) => (
          <li className="attempt" key={a.number}>
            <div className="attempt-header">
              <span className="attempt-number">#{a.number}</span>
              <span className={`badge badge-${OUTCOME_BADGES[a.outcome || ""] || "queued"}`}>{outcomeLabel(a.outcome)}</span>
            </div>
            <div className="attempt-meta">
              started {new Date(a.startedAt).toLocaleString()}
              {a.finishedAt && <> · finished {new Date(a.finishedAt).toLocaleString()}</>}
            </div>
            {a.detail && <div className="attempt-detail">{a.detail}</div>}
          </li>
        ))}
      </ul>
    </fieldset>
  );
}

function CapabilityToggles({ t, config, act }) {
  return (
    <fieldset>
      <legend>Capabilities</legend>
      {(config?.capabilities || []).map((c) => (
        <label key={c.id} className="checkbox" title={c.description}>
          <input
            type="checkbox"
            checked={t.capabilities.includes(c.id)}
            onChange={(e) => act(() => api(`/api/tasks/${t.id}/capabilities`, {
              method: "POST",
              body: JSON.stringify({ id: c.id, attach: e.target.checked }),
            }), t.id)}
          />
          {c.name}
        </label>
      ))}
    </fieldset>
  );
}

// Dependencies is the "definition" and "signal" this whole feature is
// about, together: what a task has declared it depends on (chips,
// removable), which of those are still open (the "blocked" styling on a
// chip), and a way to add another -- attach/detach through /depends-on.
function Dependencies({ t, tasks, act, onOpenTask }) {
  const dependsOn = t.dependsOn || [];
  const blockedBy = new Set(t.blockedBy || []);

  const add = (picked) => act(() => api(`/api/tasks/${t.id}/depends-on`, {
    method: "POST",
    body: JSON.stringify({ id: picked.id, attach: true }),
  }), t.id);

  return (
    <fieldset>
      <legend>Depends on</legend>
      {dependsOn.length === 0 && <p className="hint">No dependencies.</p>}
      <div className="chips dependency-chips">
        {dependsOn.map((id) => {
          const blocking = blockedBy.has(id);
          return (
            <span key={id} className={`chip dependency-chip${blocking ? " chip-blocking" : ""}`}>
              <span onClick={() => onOpenTask(id)}>{id}{blocking ? " (open)" : ""}</span>
              <button
                type="button"
                className="chip-remove"
                title={`Remove dependency on ${id}`}
                onClick={(e) => {
                  e.stopPropagation();
                  act(() => api(`/api/tasks/${t.id}/depends-on`, {
                    method: "POST",
                    body: JSON.stringify({ id, attach: false }),
                  }), t.id);
                }}
              >
                ×
              </button>
            </span>
          );
        })}
      </div>
      <TaskPicker
        tasks={tasks || []}
        exclude={[t.id, ...dependsOn]}
        onPick={add}
        placeholder="Add a dependency…"
      />
    </fieldset>
  );
}

function Comments({ t, act }) {
  const textareaRef = useRef(null);

  const send = async () => {
    const body = textareaRef.current.value;
    if (!body.trim()) return;
    await act(() => api(`/api/tasks/${t.id}/comments`, { method: "POST", body: JSON.stringify({ body }) }), t.id);
    textareaRef.current.value = "";
  };

  return (
    <div className="comments">
      <h3>Conversation</h3>
      {(t.comments || []).map((c, i) => {
        // onBehalfOf is set when grain relayed somebody else's words --
        // a question from a dispatched run reads as grain speaking for
        // an agent, not as grain's own.
        const who = c.onBehalfOf ? `${c.author} on behalf of ${c.onBehalfOf}` : c.author;
        return (
          <div className="comment" key={i}>
            <div className="meta">{who} · {c.authorKind}</div>
            <div>{c.body}</div>
          </div>
        );
      })}
      {/* Uncontrolled on purpose: a poll landing mid-reply re-renders
          this component with fresh props, but never touches the
          textarea's own DOM value, so an unsent draft survives it. */}
      <div className="comment-form">
        <textarea rows="2" placeholder="Reply..." ref={textareaRef} />
        <button className="secondary" onClick={send}>Comment</button>
      </div>
    </div>
  );
}
