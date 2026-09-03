import { useRef, useState } from "react";
import { Alert, Box, Button, Checkbox, Chip, FormControl, Link, ListItemText, MenuItem, Select, Stack, TextField, Tooltip, Typography } from "@mui/material";
import api from "../api.js";
import fileToAttachment from "../attachments.js";
import { STATE_LABELS, capabilityRows, capabilityUnavailableHint, completionPhase, frameworkLabel, orphanedPullRequest } from "../state.js";
import AttachmentLinks from "./AttachmentLinks.jsx";
import AttachmentPicker from "./AttachmentPicker.jsx";
import AttemptTranscriptOverlay from "./AttemptTranscriptOverlay.jsx";
import Markdown from "./Markdown.jsx";
import Overlay from "./Overlay.jsx";
import PromptOverlay from "./PromptOverlay.jsx";
import StateDot, { isLiveRunning } from "./StateDot.jsx";
import TaskPicker from "./TaskPicker.jsx";

// The panel splits like Plane's own issue peek: title, description and
// the conversation in a main column, everything about the task's current
// state and its declared shape (repo, capabilities, dependencies) in a
// narrow property column beside it. It fills the whole pane beside the
// sidebar rather than floating in the middle of it (grain/task-94, see
// Overlay's own `pane`): a task is mostly an agent's own prose, and a
// long answer wants the room.
export default function DetailOverlay({ task: t, tasks, config, onClose, onOpenTask, act, showError }) {
  const phase = completionPhase(t);
  const orphaned = orphanedPullRequest(t);
  // editing is local to DetailOverlay, not lifted to App.jsx, the same
  // as Timeline's own openAttempt -- nothing outside this overlay needs
  // to know a task's title and description are mid-edit, and closing
  // the overlay (which unmounts it) is itself "cancel" for free.
  const [editing, setEditing] = useState(false);
  // showPrompt is the same local treatment editing gets, for the same
  // reason: nothing outside this overlay needs to know the prompt pane
  // is open, and closing the task closes it too.
  const [showPrompt, setShowPrompt] = useState(false);
  return (
    <Overlay onClose={onClose} pane>
      <div className="detail-layout">
        <div className="detail-main">
          {editing ? (
            <EditTaskForm t={t} act={act} onDone={() => setEditing(false)} />
          ) : (
            <>
              <div className="detail-header">
                <Typography variant="h6" component="h2" sx={{ m: 0, fontWeight: 600, fontSize: "1.15rem" }}>{t.id} {t.title}</Typography>
                {/* The same prompt every task row offers a button for
                    (TaskList's own PromptButton), offered again here
                    because this is where someone works out why a task
                    went the way it did -- and the title and description
                    above are only part of what its agent was handed. */}
                <Button size="small" onClick={() => setShowPrompt(true)}>Prompt</Button>
                <Button size="small" onClick={() => setEditing(true)}>Edit</Button>
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

              {/* The placeholder is grain's own words, not the task's,
                  so it stays a plain string -- only a real description
                  (often an agent's own markdown, for a task another run
                  proposed) goes through the renderer. */}
              {t.description
                ? <Markdown className="description">{t.description}</Markdown>
                : <div className="description">(no description)</div>}
              <AttachmentLinks taskId={t.id} attachments={t.attachments} />
            </>
          )}

          {t.state === "failed" && (
            <Alert severity="error" sx={{ mb: 2 }}>
              <strong>{t.failedAttempts} consecutive failed attempt{t.failedAttempts === 1 ? "" : "s"}.</strong>
              {t.lastFailureReason && <> Last failure: {t.lastFailureReason}</>}
            </Alert>
          )}

          {/* A pull request nobody is going to merge, said where the
              person who closed the task is looking -- see state.js's
              orphanedPullRequest for why a closed task can be carrying
              one and why a merged one is not it. */}
          {orphaned && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              <strong>{orphaned} is still open, and grain has stopped watching it.</strong>{" "}
              Closing this task took it off the merge queue for good, so grain will not merge
              or update that pull request. Merge or close it on GitHub by hand, or reopen this
              task to put it back under grain's watch.
            </Alert>
          )}

          <Timeline t={t} act={act} showError={showError} />
        </div>

        <div className="detail-side">
          <div className="detail-state">
            <span className={`badge badge-${t.state}${isLiveRunning(t.state) ? " badge-mark" : ""}`}>
              <StateDot state={t.state} title={STATE_LABELS[t.state] || t.state} />
              {STATE_LABELS[t.state] || t.state}
            </span>
            {/* Both chips read as annotations beside the state dot, not a
                replacement for it -- a blocked task is still queued
                (docs/data-model.md), and a completed task's PR phase is
                still "completed" as far as model.State goes. */}
            {phase && <Chip size="small" color={phase.color} title={phase.title} label={phase.label} />}
            {t.blocked && <Chip size="small" color="error" label="Blocked" />}
          </div>

          <Actions t={t} config={config} act={act} />
          <Declared t={t} />
          <CapabilityToggles t={t} config={config} act={act} />
          <Dependencies t={t} tasks={tasks} act={act} onOpenTask={onOpenTask} />
        </div>
      </div>
      {showPrompt && <PromptOverlay taskId={t.id} onClose={() => setShowPrompt(false)} />}
    </Overlay>
  );
}

// EditTaskForm is the title/description editor DetailOverlay swaps its
// header for (bwsalmon/agents#523) -- those two fields are the only ones
// BuildPrompt actually hands a dispatched run, so an edit to either has
// to reach one already running, not just show up here; PATCH /api/tasks/
// {id} (ui.Client.UpdateTask) is what records that as an addendum
// comment (noteEdit) for orchestrator.addendaPoller to pick up. Every
// other UpdateTaskRequest field (repo, base, auto-merge, reads) already
// has its own editor on this same page (Declared has no editor yet, but
// CapabilityToggles and Dependencies cover the two that do), so this
// form does not attempt to cover them too.
function EditTaskForm({ t, act, onDone }) {
  const [title, setTitle] = useState(t.title);
  const [description, setDescription] = useState(t.description || "");

  // Mirrors Timeline's own send(): act() already reports a failure to
  // the user (showError, wired in by App.jsx), so there is nothing left
  // for this form to do differently on one -- closing either way is the
  // same "fire it and stop showing the form" precedent send() already
  // set for a reply.
  const save = async () => {
    if (!title.trim()) return;
    await act(() => api(`/api/tasks/${t.id}`, {
      method: "PATCH",
      body: JSON.stringify({ title, description }),
    }), t.id);
    onDone();
  };

  return (
    <Stack spacing={1.5} sx={{ mb: 2 }}>
      <TextField label="Title" value={title} onChange={(e) => setTitle(e.target.value)} autoFocus fullWidth size="small" />
      <TextField label="Description" value={description} onChange={(e) => setDescription(e.target.value)} multiline rows={5} fullWidth size="small" />
      <Stack direction="row" spacing={1} justifyContent="flex-end">
        <Button onClick={onDone}>Cancel</Button>
        <Button variant="contained" disabled={!title.trim()} onClick={save}>Save</Button>
      </Stack>
    </Stack>
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
  // bwsalmon/agents#534, grain/task-41: a per-task sandbox shape
  // override, shown only when set -- most tasks use the deployment
  // default and have none of the three fields, the same "0 means unset"
  // convention that keeps them out of the JSON response at all (Task's
  // own omitempty).
  if (t.sandboxCpus) rows.push(["Sandbox vCPUs", String(t.sandboxCpus)]);
  if (t.sandboxMemoryMb) rows.push(["Sandbox memory (MiB)", String(t.sandboxMemoryMb)]);
  if (t.sandboxDiskGb) rows.push(["Sandbox disk (GiB)", String(t.sandboxDiskGb)]);
  // Same treatment for a per-task agent framework: shown only when this
  // task overrides the deployment's own, which most do not.
  if (t.agentFramework) rows.push(["Agent framework", frameworkLabel(t.agentFramework)]);
  // Same treatment again for a task that replaces the standing
  // instructions its deployment and repo would otherwise give its runs
  // (grain/task-114): shown only when it has one, since most tasks do
  // not and an empty row would suggest they had opted out of something.
  if (t.promptExtension) rows.push(["Prompt extension", t.promptExtension]);
  // Most tasks are not interactive, so this row only shows up for the
  // ones that are -- the same "shown only when set" treatment the
  // sandbox override rows above already get. Configuration is always
  // also Interactive (model.Task.Configuration's own doc comment), so it
  // takes over the same row rather than adding a second one.
  if (t.configuration) rows.push(["Mode", "Configuration agent"]);
  else if (t.interactive) rows.push(["Mode", "Interactive"]);
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

function Actions({ t, config, act }) {
  return (
    <Stack className="actions" spacing={1}>
      {t.state === "proposed" && (
        <Button variant="contained" onClick={() => act(() => api(`/api/tasks/${t.id}/approve`, { method: "POST" }), t.id)}>
          Approve
        </Button>
      )}
      {/* Approve's own undo, offered on exactly the state it applies to:
          a queued task has been approved and has not started, so
          clearing that approval puts it back among the proposals rather
          than closing it (Client.WithdrawApproval, which refuses the
          states this button is never shown for). */}
      {t.state === "queued" && (
        <Button variant="outlined" onClick={() => act(() => api(`/api/tasks/${t.id}/withdraw-approval`, { method: "POST" }), t.id)}>
          Withdraw approval
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
      {/* Submit still sets autoMerge -- it just never resolves into an
          actual merge on a deployment whose GitHub credential can't read
          check runs (config.autoMergeDegraded, ui.Config.
          AutoMergeDegraded's own doc comment). Without this, clicking
          Submit here looked identical to clicking nothing at all
          (bwsalmon/agents#483). */}
      {t.autoMerge && config?.autoMergeDegraded && (
        <Alert severity="warning" sx={{ fontSize: "0.8rem" }}>
          Queued for auto-merge, but this deployment can't read pull request checks, so it will never merge automatically.
        </Alert>
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
// vocabulary (orchestrator.outcomeOf, orchestrator.run's "cancelled",
// orchestrator.runOne's "setup-failed", orchestrator.recoverRun's
// "orphaned"), plus the empty string a run still in flight (no finishedAt
// yet) comes back as -- the one case that isn't itself an outcome. An
// outcome this doesn't recognise falls back to the raw string,
// capitalised, and a "queued" badge, rather than disappearing, so a value
// added on the backend later still shows up here before this map catches
// up with it.
//
// The non-obvious ones are all runs that failed somewhere other than the
// agent's own work, which is exactly why each needs a word of its own:
// "setup-failed" is one whose sandbox could not be built (a VM that never
// booted, a token that could not be minted -- dispatch retries the task
// after its backoff), "orphaned" is one whose process died mid-run and
// which the next startup finished on its behalf (RecoverOrphanedRuns),
// "finish-failed" is one whose agent worked but whose result could not be
// turned into a pull request or a comment (orchestrator.noteFinishFailure
// -- the branch is pushed and nothing points at it), and "no_action" is
// one that ran fine and produced nothing to act on at all
// (orchestrator.ProcessResult). Every one of them takes the "failed"
// badge rather than the fallback's "queued", which would read as "hasn't
// run yet" for a run that did.
//
// "paused" (model.PausedOutcome) is the exception among those: it is a
// run grain stopped because the deployment's agent had no budget left in
// its provider's current window (orchestrator.Pause), so it takes the
// "closed" badge "cancelled" takes rather than a red one. Nothing about
// it failed, and the backend does not count it against the task's own
// failure streak either.
const OUTCOME_LABELS = {
  "": "Running",
  succeeded: "Succeeded",
  failed: "Failed",
  cancelled: "Cancelled",
  paused: "Paused (agent usage limit)",
  "setup-failed": "Setup failed",
  "finish-failed": "Finish failed",
  no_action: "No action",
  orphaned: "Orphaned",
};
const OUTCOME_BADGES = {
  "": "running",
  succeeded: "completed",
  failed: "failed",
  cancelled: "closed",
  paused: "closed",
  "setup-failed": "failed",
  "finish-failed": "failed",
  no_action: "failed",
  orphaned: "failed",
};

// PR_EVENT_LABELS covers ui.PullRequestEvent's own Kind vocabulary
// (bwsalmon/agents#493) -- "closed" specifically means closed *without*
// merging, since a merged PR is always "merged" (pullRequestEventsFrom's
// own doc comment), which the label spells out rather than leaving a
// reader to infer from "closed" alone what a task's own bare "Closed"
// state transition already uses that word for.
const PR_EVENT_LABELS = { opened: "PR opened", merged: "PR merged", closed: "PR closed without merging" };

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
// carry), every pull request event (bwsalmon/agents#493's
// t.pullRequestEvents -- opened, merged, or closed without merging), and
// every comment -- into one list, oldest first, each carrying the
// timestamp it happened at. There is no unified "history" on the wire:
// this just interleaves TaskDetail's own separate arrays by their own
// timestamps, client-side.
//
// t.transitions omits a state the record has no timestamp for -- most
// notably a past awaiting_reply period once its question has been
// answered (see model.Transitions' own doc comment on why that one is
// unrecoverable) -- so this can skip a state without that being a bug to
// chase.
function timelineEvents(t) {
  const events = [];
  const transitions = t.transitions || [];
  const attempts = t.attempts || [];
  // Every "running" period already gets its own, more detailed node from
  // t.attempts below (start/finish time, outcome, a transcript to click
  // into) -- so once attempts exist, the plain state transition into
  // "running" is a second node for the same moment (bwsalmon/agents#503:
  // "a running node ... followed by a running attempt #1 ... we only
  // need a single node"). Older tasks or edge cases with transitions but
  // no recorded attempts still show the bare transition, since it's the
  // only record of that running period they have.
  const shownTransitions = attempts.length > 0 ? transitions.filter((tr) => tr.state !== "running") : transitions;
  const lastTransitionIndex = shownTransitions.length - 1;

  shownTransitions.forEach((tr, i) => {
    events.push({
      key: `transition-${tr.state}-${i}`,
      at: new Date(tr.at),
      badge: tr.state,
      // Only the most recent transition can still be "now" -- a running
      // entry earlier in the list is over and done, so its dot should
      // read as static rather than keep spinning as if the task were
      // still running that far in the past.
      current: i === lastTransitionIndex,
      // "closed" is where a task's own timeline ends -- flagged here so
      // it can be pinned last below even if a synced pull request event
      // lands with a later timestamp (bwsalmon/agents#503).
      closedTransition: tr.state === "closed",
      render: () => <div className="timeline-title">{STATE_LABELS[tr.state] || tr.state}</div>,
    });
  });

  attempts.forEach((a) => {
    // The very first attempt *is* the task's one "running" node -- calling
    // it out as "Attempt #1" only makes sense once there's a second one to
    // tell it apart from (bwsalmon/agents#503).
    const title = a.number === 1 ? outcomeLabel(a.outcome) : `Attempt #${a.number} · ${outcomeLabel(a.outcome)}`;
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
          <div className="timeline-title">{title}</div>
          <div className="timeline-meta">
            started {new Date(a.startedAt).toLocaleString()}
            {a.finishedAt && <> · finished {new Date(a.finishedAt).toLocaleString()}</>}
          </div>
          {a.detail && <div className="timeline-detail">{a.detail}</div>}
        </>
      ),
    });
  });

  // A pull request grain re-linked (say, a task reopened against a PR that
  // had already been open for a while) keeps that PR's real, original
  // open time -- which can predate this lifecycle's own first transition,
  // sorting "PR opened" ahead of "Proposed"/"Queued" and everything else
  // rather than where it actually belongs (bwsalmon/agents#503: "PR
  // opened comes out of order at the beginning of the timeline"). Clamping
  // it to this lifecycle's own start keeps it from floating above events
  // that actually happened first.
  const earliestAt = shownTransitions.length > 0 ? new Date(shownTransitions[0].at) : null;

  (t.pullRequestEvents || []).forEach((e, i) => {
    let at = e.at ? new Date(e.at) : null;
    if (e.kind === "opened" && at && earliestAt && at < earliestAt) at = earliestAt;
    events.push({
      key: `pr-event-${i}`,
      at,
      badge: `pr_${e.kind}`,
      render: () => (
        <>
          <div className="timeline-title">{PR_EVENT_LABELS[e.kind] || e.kind}</div>
          {t.pullRequest && (
            <div className="timeline-meta">
              <Link href={pullRequestUrl(t.pullRequest)} target="_blank" rel="noopener noreferrer">{t.pullRequest}</Link>
            </div>
          )}
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
          {c.body && <Markdown className="timeline-comment-body">{c.body}</Markdown>}
          <AttachmentLinks taskId={t.id} attachments={c.attachments} />
        </>
      ),
    });
  });

  events.sort((a, b) => (a.at?.getTime() ?? 0) - (b.at?.getTime() ?? 0));

  // Closing can outrun the sync that notices its pull request followed
  // suit -- a "merged"/"closed" pull request event synced in afterward
  // would otherwise land after "Closed" by timestamp but read strangely
  // once "Closed" is already the story's end (bwsalmon/agents#503: "PR
  // closing is coming after the closed state, let's make closed the last
  // thing in the timeline").
  const closedIndex = events.findIndex((e) => e.closedTransition);
  if (closedIndex !== -1 && closedIndex !== events.length - 1) {
    events.push(events.splice(closedIndex, 1)[0]);
  }

  return events;
}

function CapabilityToggles({ t, config, act }) {
  const capabilities = config?.capabilities || [];
  const selected = t.capabilities || [];
  // A task can hold a grant the picker no longer offers -- a capability
  // renamed or dropped since it was attached ("scratch-repo", now
  // github-sandbox, bwsalmon/agents#612). capabilityRows (state.js)
  // gives it a row of its own purely to be turned off; without one it is
  // a chip nothing can untick, and a grant no provider is registered for
  // fails every run of the task holding it. SetCapability (pkg/ui,
  // client.go) is the other half -- it validates on attach only, so the
  // detach this row sends is accepted.
  const rows = capabilityRows(capabilities, selected);

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
                const c = rows.find((cap) => cap.id === id);
                return <Chip key={id} size="small" label={c ? c.name : id} />;
              })}
            </Box>
          ))}
        >
          {rows.map((c) => {
            const unavailable = capabilityUnavailableHint(c);
            return (
              <MenuItem key={c.id} value={c.id} title={unavailable ? `${c.description}\n\n${unavailable}` : c.description}>
                <Checkbox checked={selected.includes(c.id)} size="small" />
                <ListItemText
                  primary={c.name}
                  secondary={unavailable || null}
                  secondaryTypographyProps={{ color: "warning.main" }}
                />
              </MenuItem>
            );
          })}
        </Select>
      </FormControl>
    </fieldset>
  );
}

// What a chip cannot fit -- the depended-on task's state, and the tail
// of a long title -- goes here instead, so hovering a dependency tells
// you what it is without opening it. dep is missing
// when the depended-on task is not in the loaded list, and then its id
// is genuinely all this overlay knows about it.
function dependencyTooltip(id, dep, blocking) {
  if (!dep) return blocking ? `${id} — still open` : id;
  const state = STATE_LABELS[dep.state] || dep.state;
  return `${id} ${dep.title}${state ? ` — ${state}` : ""}${blocking ? " (blocking this task)" : ""}`;
}

// Dependencies is the "definition" and "signal" this whole feature is
// about, together: what a task has declared it depends on (chips,
// removable), which of those are still open (the "blocked" styling on a
// chip), and a way to add another -- attach/detach through /depends-on.
//
// One full-width chip per line, carrying the depended-on task's title:
// a wrapped row of id-only chips came out as bubbles a couple of
// characters wide, which named a dependency without telling you
// anything about it.
function Dependencies({ t, tasks, act, onOpenTask }) {
  const dependsOn = t.dependsOn || [];
  const blockedBy = new Set(t.blockedBy || []);
  const byId = new Map((tasks || []).map((other) => [other.id, other]));

  const add = (picked) => act(() => api(`/api/tasks/${t.id}/depends-on`, {
    method: "POST",
    body: JSON.stringify({ id: picked.id, attach: true }),
  }), t.id);

  return (
    <fieldset>
      <legend>Depends on</legend>
      {dependsOn.length === 0 && <p className="hint">No dependencies.</p>}
      <Stack spacing={0.6} sx={{ mb: dependsOn.length > 0 ? 1 : 0 }}>
        {dependsOn.map((id) => {
          const blocking = blockedBy.has(id);
          // A dependency the loaded list does not carry still gets its
          // chip, id only: dependsOn records ids, and hiding the row for
          // want of a title would hide the dependency itself.
          const dep = byId.get(id);
          return (
            <Tooltip key={id} title={dependencyTooltip(id, dep, blocking)} placement="left">
              <Chip
                variant={blocking ? "outlined" : "filled"}
                color={blocking ? "warning" : "default"}
                label={`${id}${dep ? ` ${dep.title}` : ""}${blocking ? " (open)" : ""}`}
                onClick={() => onOpenTask(id)}
                onDelete={() => act(() => api(`/api/tasks/${t.id}/depends-on`, {
                  method: "POST",
                  body: JSON.stringify({ id, attach: false }),
                }), t.id)}
                deleteIcon={<span title={`Remove dependency on ${id}`}>×</span>}
                sx={{ width: "100%", justifyContent: "space-between", "& .MuiChip-label": { flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis" } }}
              />
            </Tooltip>
          );
        })}
      </Stack>
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
  // attachments is File objects picked for the reply not yet sent --
  // AttachmentPicker's own doc comment on why reading them is deferred.
  const [attachments, setAttachments] = useState([]);

  const send = async () => {
    const body = textareaRef.current.value;
    if (!body.trim() && attachments.length === 0) return;
    const uploaded = await Promise.all(attachments.map((f) => fileToAttachment(f)));
    await act(() => api(`/api/tasks/${t.id}/comments`, {
      method: "POST",
      body: JSON.stringify({ body, attachments: uploaded }),
    }), t.id);
    textareaRef.current.value = "";
    setAttachments([]);
  };

  return (
    <div className="timeline">
      {/* bwsalmon/agents#539: an interactive task's Timeline *is* its
          chat, so it reads as one instead of as a history of a task
          nobody is meant to be watching live. */}
      <h3>{t.interactive ? "Chat" : "Timeline"}</h3>
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
                <span
                  className={`badge badge-${e.badge}${
                    e.badge === "running" ? (e.current ? " badge-mark" : " badge-static") : ""
                  }`}
                >
                  <StateDot state={e.badge} live={e.current} title={STATE_LABELS[e.badge] || e.badge} />
                </span>
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
        <TextField
          multiline
          rows={2}
          placeholder={t.interactive ? "Message..." : "Reply..."}
          inputRef={textareaRef}
          autoFocus={t.interactive}
          fullWidth
          size="small"
        />
        <AttachmentPicker files={attachments} onChange={setAttachments} />
        <Button variant="outlined" onClick={send}>Comment</Button>
      </div>
    </div>
  );
}
