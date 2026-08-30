import { useRef, useState } from "react";
import { Alert, Box, Button, Checkbox, Chip, FormControl, Link, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { STATE_LABELS } from "../state.js";
import AttemptTranscriptOverlay from "./AttemptTranscriptOverlay.jsx";
import Overlay from "./Overlay.jsx";
import TaskPicker from "./TaskPicker.jsx";

// The panel splits like Plane's own issue peek: title, description and
// the conversation in a main column, everything about the task's current
// state and its declared shape (repo, capabilities, dependencies) in a
// narrow property column beside it.
export default function DetailOverlay({ task: t, tasks, config, onClose, onOpenTask, act, showError }) {
  return (
    <Overlay onClose={onClose} wide>
      <div className="detail-layout">
        <div className="detail-main">
          <div className="detail-header">
            <Typography variant="h6" component="h2" sx={{ m: 0, fontWeight: 600, fontSize: "1.15rem" }}>{t.id} {t.title}</Typography>
          </div>

          <div className="freshness">
            as of just now
            {t.pullRequest && <> · <Link href={pullRequestUrl(t.pullRequest)} target="_blank" rel="noopener noreferrer">{t.pullRequest}</Link></>}
            {t.generatedFrom && (
              <>
                {" "}· generated from{" "}
                <Link href="#" onClick={(e) => { e.preventDefault(); onOpenTask(t.generatedFrom); }}>{t.generatedFrom}</Link>
              </>
            )}
          </div>

          <div className="description">{t.description || "(no description)"}</div>

          {t.state === "failed" && (
            <Alert severity="error" sx={{ mb: 2 }}>
              <strong>{t.failedAttempts} consecutive failed attempt{t.failedAttempts === 1 ? "" : "s"}.</strong>
              {t.lastFailureReason && <> Last failure: {t.lastFailureReason}</>}
            </Alert>
          )}

          <Timeline t={t} act={act} showError={showError} />
        </div>

        <div className="detail-side">
          <div className="detail-state">
            <span className={`badge badge-${t.state}`}>{STATE_LABELS[t.state] || t.state}</span>
            {/* Blocked reads as an annotation beside the state dot, not a
                replacement for it -- a blocked task is still queued
                (docs/data-model.md). */}
            {t.blocked && <Chip size="small" color="error" label="Blocked" />}
          </div>

          <Actions t={t} act={act} />
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
    <Stack className="actions" spacing={1}>
      {t.state === "proposed" && (
        <Button variant="contained" onClick={() => act(() => api(`/api/tasks/${t.id}/approve`, { method: "POST" }), t.id)}>
          Approve
        </Button>
      )}
      {/* Once a task's run has produced a pull request, submitting is
          what puts it on the merge queue for automatic conflict
          resolution and merging. Already-submitted tasks (autoMerge
          already true) have nothing left for this button to do. */}
      {t.pullRequest && !t.autoMerge && (
        <Button variant="contained" onClick={() => act(() => api(`/api/tasks/${t.id}/submit`, { method: "POST" }), t.id)}>
          Submit
        </Button>
      )}
      {t.state === "failed" && (
        <Button variant="contained" onClick={() => act(() => api(`/api/tasks/${t.id}/retry`, { method: "POST" }), t.id)}>
          Retry
        </Button>
      )}
      {t.state === "closed" ? (
        <Button variant="outlined" onClick={() => act(() => api(`/api/tasks/${t.id}/reopen`, { method: "POST" }), t.id)}>
          Reopen
        </Button>
      ) : t.state === "running" ? (
        // Closing a running task stops it from ever being re-dispatched
        // or opened as a pull request, and cancels the run in flight --
        // "Cancel" is that same close call, surfaced under a name that
        // matches what a running task's close button actually does.
        <Button
          variant="outlined"
          color="error"
          onClick={() => {
            if (!confirm("Cancel this job? Its run will be abandoned: no pull request will be opened for it.")) return;
            act(() => api(`/api/tasks/${t.id}/close`, { method: "POST" }), t.id);
          }}
        >
          Cancel
        </Button>
      ) : (
        <Button variant="outlined" color="error" onClick={() => act(() => api(`/api/tasks/${t.id}/close`, { method: "POST" }), t.id)}>
          Close
        </Button>
      )}
    </Stack>
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

// timelineEvents merges everything grain has recorded about a task --
// every state transition (bwsalmon/agents#452's t.transitions, which
// already covers filed/queued/running/awaiting_reply/failed/completed/
// closed), every attempt's own result (bwsalmon/agents#445's
// t.attempts, with the outcome and detail a bare state change doesn't
// carry), and every comment -- into one list, oldest first, each
// carrying the timestamp it happened at. There is no unified "history"
// on the wire: this just interleaves TaskDetail's own separate arrays by
// their own timestamps, client-side.
//
// t.transitions omits a state the record has no timestamp for -- most
// notably a past awaiting_reply period once its question has been
// answered (see model.Transitions' own doc comment on why that one is
// unrecoverable) -- so this can skip a state without that being a bug to
// chase.
function timelineEvents(t) {
  const events = [];
  const transitions = t.transitions || [];
  const lastTransitionIndex = transitions.length - 1;

  transitions.forEach((tr, i) => {
    events.push({
      key: `transition-${i}`,
      at: new Date(tr.at),
      badge: tr.state,
      // Only the most recent transition can still be "now" -- a running
      // entry earlier in the list is over and done, so its dot should
      // read as static rather than keep spinning as if the task were
      // still running that far in the past.
      current: i === lastTransitionIndex,
      render: () => <div className="timeline-title">{STATE_LABELS[tr.state] || tr.state}</div>,
    });
  });

  (t.attempts || []).forEach((a) => {
    events.push({
      key: `attempt-${a.number}`,
      at: new Date(a.startedAt),
      badge: OUTCOME_BADGES[a.outcome || ""] || "queued",
      // attempt, not set on any other kind of event, is what tells
      // Timeline's own render loop which <li> to make interactive: typing
      // on a task attempt is how AttemptTranscriptOverlay opens
      // (bwsalmon/agents#446).
      attempt: a,
      // Same reasoning as transitions above: a "running" badge only
      // belongs to an attempt that hasn't finished yet, never a past one.
      current: !a.finishedAt,
      render: () => (
        <>
          <div className="timeline-title">Attempt #{a.number} · {outcomeLabel(a.outcome)}</div>
          <div className="timeline-meta">
            started {new Date(a.startedAt).toLocaleString()}
            {a.finishedAt && <> · finished {new Date(a.finishedAt).toLocaleString()}</>}
          </div>
          {a.detail && <div className="timeline-detail">{a.detail}</div>}
        </>
      ),
    });
  });

  (t.comments || []).forEach((c, i) => {
    // onBehalfOf is set when grain relayed somebody else's words -- a
    // question from a dispatched run reads as grain speaking for an
    // agent, not as grain's own.
    const who = c.onBehalfOf ? `${c.author} on behalf of ${c.onBehalfOf}` : c.author;
    events.push({
      key: `comment-${i}`,
      at: c.createdAt ? new Date(c.createdAt) : null,
      badge: "comment",
      render: () => (
        <>
          <div className="timeline-meta">{who} · {c.authorKind}</div>
          <div className="timeline-comment-body">{c.body}</div>
        </>
      ),
    });
  });

  events.sort((a, b) => (a.at?.getTime() ?? 0) - (b.at?.getTime() ?? 0));
  return events;
}

function CapabilityToggles({ t, config, act }) {
  const capabilities = config?.capabilities || [];
  const selected = t.capabilities || [];

  const handleChange = (e) => {
    const next = e.target.value;
    const added = next.filter((id) => !selected.includes(id));
    const removed = selected.filter((id) => !next.includes(id));
    added.forEach((id) => act(() => api(`/api/tasks/${t.id}/capabilities`, {
      method: "POST",
      body: JSON.stringify({ id, attach: true }),
    }), t.id));
    removed.forEach((id) => act(() => api(`/api/tasks/${t.id}/capabilities`, {
      method: "POST",
      body: JSON.stringify({ id, attach: false }),
    }), t.id));
  };

  return (
    <fieldset>
      <legend>Capabilities</legend>
      <FormControl fullWidth size="small">
        <Select
          multiple
          displayEmpty
          inputProps={{ "aria-label": "Capabilities" }}
          value={selected}
          onChange={handleChange}
          renderValue={(sel) => (sel.length === 0 ? (
            <span className="hint">None</span>
          ) : (
            <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
              {sel.map((id) => {
                const c = capabilities.find((cap) => cap.id === id);
                return <Chip key={id} size="small" label={c ? c.name : id} />;
              })}
            </Box>
          ))}
        >
          {capabilities.map((c) => (
            <MenuItem key={c.id} value={c.id} title={c.description}>
              <Checkbox checked={selected.includes(c.id)} size="small" />
              <ListItemText primary={c.name} />
            </MenuItem>
          ))}
        </Select>
      </FormControl>
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
      <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.6, mb: dependsOn.length > 0 ? 1 : 0 }}>
        {dependsOn.map((id) => {
          const blocking = blockedBy.has(id);
          return (
            <Chip
              key={id}
              size="small"
              variant={blocking ? "outlined" : "filled"}
              color={blocking ? "warning" : "default"}
              label={`${id}${blocking ? " (open)" : ""}`}
              onClick={() => onOpenTask(id)}
              onDelete={() => act(() => api(`/api/tasks/${t.id}/depends-on`, {
                method: "POST",
                body: JSON.stringify({ id, attach: false }),
              }), t.id)}
              deleteIcon={<span title={`Remove dependency on ${id}`}>×</span>}
            />
          );
        })}
      </Box>
      <TaskPicker
        tasks={tasks || []}
        exclude={[t.id, ...dependsOn]}
        onPick={add}
        placeholder="Add a dependency…"
      />
    </fieldset>
  );
}

// Timeline is the task's whole history -- every state transition, every
// attempt's result, every comment -- as one time-ordered feed
// (bwsalmon/agents#453: "show the various actions ... as a timeline with
// times next to each transition"), rendered as a connected list of dots
// (reusing the existing badge-<state> dot styling) with the comment box
// fixed at the bottom, the same spot Comments used to keep it in under
// the conversation-only list this replaces.
function Timeline({ t, act, showError }) {
  const textareaRef = useRef(null);
  const events = timelineEvents(t);
  // openAttempt is the attempt (bwsalmon/agents#446's own Attempt shape,
  // off t.attempts) whose transcript is open, or null -- local to
  // Timeline, since nothing outside it needs to know.
  const [openAttempt, setOpenAttempt] = useState(null);

  const send = async () => {
    const body = textareaRef.current.value;
    if (!body.trim()) return;
    await act(() => api(`/api/tasks/${t.id}/comments`, { method: "POST", body: JSON.stringify({ body }) }), t.id);
    textareaRef.current.value = "";
  };

  return (
    <div className="timeline">
      <h3>Timeline</h3>
      {events.length > 0 && (
        <ul className="timeline-list">
          {events.map((e) => (
            <li
              className={e.attempt ? "timeline-item timeline-item-attempt" : "timeline-item"}
              key={e.key}
              {...(e.attempt && {
                tabIndex: 0,
                role: "button",
                title: "View this attempt's agent transcript",
                onClick: () => setOpenAttempt(e.attempt),
                onKeyDown: (ev) => {
                  if (ev.key === "Enter" || ev.key === " ") {
                    ev.preventDefault();
                    setOpenAttempt(e.attempt);
                  }
                },
              })}
            >
              <div className="timeline-marker">
                <span className={`badge badge-${e.badge}${e.badge === "running" && !e.current ? " badge-static" : ""}`} />
              </div>
              <div className="timeline-body">
                {e.at && <div className="timeline-when">{e.at.toLocaleString()}</div>}
                {e.render()}
              </div>
            </li>
          ))}
        </ul>
      )}
      {openAttempt && (
        <AttemptTranscriptOverlay
          taskId={t.id}
          attempt={openAttempt}
          onClose={() => setOpenAttempt(null)}
          showError={showError}
        />
      )}
      {/* Uncontrolled on purpose: a poll landing mid-reply re-renders
          this component with fresh props, but never touches the
          textarea's own DOM value, so an unsent draft survives it. */}
      <div className="comment-form">
        <TextField multiline rows={2} placeholder="Reply..." inputRef={textareaRef} fullWidth size="small" />
        <Button variant="outlined" onClick={send}>Comment</Button>
      </div>
    </div>
  );
}
