import { useRef } from "react";
import api from "../api.js";
import { STATE_LABELS } from "../state.js";
import Overlay from "./Overlay.jsx";

export default function DetailOverlay({ task: t, config, onClose, onOpenTask, act }) {
  return (
    <Overlay onClose={onClose}>
      <div className="detail-header">
        <h2>{t.id} {t.title}</h2>
        <span className={`badge badge-${t.state}`}>{STATE_LABELS[t.state] || t.state}</span>
        {/* Blocked reads as an annotation beside the state pill, not a
            replacement for it -- a blocked task is still queued
            (docs/data-model.md). */}
        {t.blocked && <span className="badge badge-blocked">Blocked</span>}
      </div>

      <div className="freshness">
        as of just now
        {t.pullRequest && <> · <span>{t.pullRequest}</span></>}
        {t.generatedFrom && (
          <>
            {" "}· generated from{" "}
            <a href="#" onClick={(e) => { e.preventDefault(); onOpenTask(t.generatedFrom); }}>{t.generatedFrom}</a>
          </>
        )}
      </div>

      <Declared t={t} />

      <div className="description">{t.description || "(no description)"}</div>

      <Actions t={t} act={act} />

      <CapabilityToggles t={t} config={config} act={act} />
      <Dependencies t={t} act={act} onOpenTask={onOpenTask} />
      <Comments t={t} act={act} />
    </Overlay>
  );
}

// Real columns on the task now, not directive lines parsed out of a
// body -- so they are rendered as fields rather than as the /repo,
// /base, /auto-merge syntax they used to have to be written in.
function Declared({ t }) {
  const parts = [];
  if (t.repo) parts.push(`repo ${t.repo}`);
  if (t.base) parts.push(`base ${t.base}`);
  if (t.reads && t.reads.length > 0) parts.push(`reads ${t.reads.join(", ")}`);
  parts.push(`auto-merge ${t.autoMerge}`);
  return <div className="declared">{parts.join("  ")}</div>;
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
function Dependencies({ t, act, onOpenTask }) {
  const inputRef = useRef(null);
  const dependsOn = t.dependsOn || [];
  const blockedBy = new Set(t.blockedBy || []);

  const add = () => {
    const id = inputRef.current.value.trim();
    if (!id) return;
    act(() => api(`/api/tasks/${t.id}/depends-on`, {
      method: "POST",
      body: JSON.stringify({ id, attach: true }),
    }), t.id).then(() => { inputRef.current.value = ""; });
  };

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
      <div className="dependency-add">
        <input type="text" placeholder="task id" ref={inputRef} />
        <button type="button" className="secondary" onClick={add}>Add</button>
      </div>
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
