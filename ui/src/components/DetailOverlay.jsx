import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  FormControl,
  FormControlLabel,
  Link,
  ListItemText,
  MenuItem,
  Select,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from "@mui/material";
import api from "../api.js";
import { useTimeZone } from "../TimeZoneContext.jsx";
import { formatDateTime } from "../time.js";
import fileToAttachment from "../attachments.js";
import {
  STATE_LABELS,
  capabilityRows,
  capabilityUnavailableHint,
  closablePullRequest,
  completionPhase,
  frameworkLabel,
  knownRepos,
  lastBaseForRepo,
  orphanedPullRequest,
  runActivity,
  stateLabel,
} from "../state.js";
import AttachmentLinks from "./AttachmentLinks.jsx";
import AttachmentPicker from "./AttachmentPicker.jsx";
import AttemptTranscriptOverlay from "./AttemptTranscriptOverlay.jsx";
import { GlyphLabel } from "./ItemGlyph.jsx";
import Markdown from "./Markdown.jsx";
import Overlay from "./Overlay.jsx";
import PromptOverlay from "./PromptOverlay.jsx";
import ReadOnlyReposField from "./ReadOnlyReposField.jsx";
import RepoField from "./RepoField.jsx";
import StateDot, { isLiveRunning } from "./StateDot.jsx";
import TaskPicker from "./TaskPicker.jsx";

// The panel splits like Plane's own issue peek: title, description and
// the conversation in a main column, everything about the task's current
// state and its declared shape (repo, capabilities, dependencies) in a
// narrow property column beside it. It fills the whole pane beside the
// sidebar rather than floating in the middle of it (grain/task-94, see
// Overlay's own `pane`): a task is mostly an agent's own prose, and a
// long answer wants the room.
export default function DetailOverlay({
  task: t,
  tasks,
  config,
  templates,
  onClose,
  onOpenTask,
  act,
  showError,
}) {
  const phase = completionPhase(t);
  const orphaned = orphanedPullRequest(t);
  const activity = runActivity(t);
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
            <EditTaskForm
              t={t}
              templates={templates}
              act={act}
              onDone={() => setEditing(false)}
            />
          ) : (
            <>
              <div className="detail-header">
                <Typography
                  variant="h6"
                  component="h2"
                  sx={{ m: 0, fontWeight: 600, fontSize: "1.15rem" }}
                >
                  {t.id} {t.title}
                </Typography>
                {/* The one way in to the whole prompt a task's agent was
                    handed (grain/task-175 moved it off every task row):
                    this is where someone works out why a task went the
                    way it did, and the title and description above are
                    only part of what its agent was actually told. */}
                <Button size="small" onClick={() => setShowPrompt(true)}>
                  Prompt
                </Button>
                <Button size="small" onClick={() => setEditing(true)}>
                  Edit
                </Button>
              </div>

              <div className="freshness">
                as of just now
                {t.pullRequest && (
                  <>
                    {" "}
                    ·{" "}
                    <Link
                      href={pullRequestUrl(t.pullRequest)}
                      target="_blank"
                      rel="noopener noreferrer"
                    >
                      {t.pullRequest}
                    </Link>
                  </>
                )}
                {t.generatedFrom && (
                  <>
                    {" "}
                    · generated from{" "}
                    <Link
                      href="#"
                      onClick={(e) => {
                        e.preventDefault();
                        onOpenTask(t.generatedFrom);
                      }}
                    >
                      {t.generatedFrom}
                    </Link>
                  </>
                )}
              </div>

              {/* The placeholder is grain's own words, not the task's,
                  so it stays a plain string -- only a real description
                  (often an agent's own markdown, for a task another run
                  proposed) goes through the renderer. */}
              {t.description ? (
                <Markdown className="description">{t.description}</Markdown>
              ) : (
                <div className="description">(no description)</div>
              )}
              <AttachmentLinks taskId={t.id} attachments={t.attachments} />
            </>
          )}

          {t.state === "failed" && (
            <Alert severity="error" sx={{ mb: 2 }}>
              <strong>
                {t.failedAttempts} consecutive failed attempt
                {t.failedAttempts === 1 ? "" : "s"}.
              </strong>
              {t.lastFailureReason && <> Last failure: {t.lastFailureReason}</>}
            </Alert>
          )}

          {/* A pull request nobody is going to merge, said where the
              person who closed the task is looking -- see state.js's
              orphanedPullRequest for why a closed task can be carrying
              one and why a merged one is not it. */}
          {orphaned && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              <strong>
                {orphaned} is still open, and grain has stopped watching it.
              </strong>{" "}
              Closing this task took it off the merge queue for good, so grain
              will not merge or update that pull request. Merge or close it on
              GitHub by hand, or reopen this task to put it back under grain's
              watch.
            </Alert>
          )}

          <PendingSecret t={t} act={act} showError={showError} />

          <Timeline t={t} act={act} showError={showError} />
        </div>

        <div className="detail-side">
          <div className="detail-state">
            <span
              className={`badge badge-${t.state}${isLiveRunning(t.state) ? " badge-mark" : ""}`}
            >
              <StateDot
                state={t.state}
                title={stateLabel(t)}
                repairing={t.repairing}
              />
              {stateLabel(t)}
            </span>
            {/* Both chips read as annotations beside the state dot, not a
                replacement for it -- a blocked task is still queued
                (docs/data-model.md), and a task whose pull request the
                merge queue gave up on is still "completed" as far as
                model.State goes. */}
            {phase && (
              <Chip
                size="small"
                color={phase.color}
                title={phase.title}
                label={phase.label}
              />
            )}
            {t.blocked && <Chip size="small" color="error" label="Blocked" />}
            {t.base && (
              <Chip
                size="small"
                variant="outlined"
                label={`target: ${t.base}`}
              />
            )}
          </div>
          {/* What the run is doing, under the badge that says only that
              it is running (state.js's runActivity) -- the run's own
              words through update_status, or grain's own account of the
              setup it is still doing before the agent's first turn. The
              same line the task list shows, kept here so the page
              somebody opens *because* the list said "running" does not
              answer with less than the list did. */}
          {activity && (
            <div className="detail-activity">
              {activity.bySetup && (
                <span className="task-activity-by">grain</span>
              )}
              {activity.note}
              {activity.age && (
                <span className="task-activity-age">{activity.age}</span>
              )}
            </div>
          )}

          <Actions t={t} config={config} act={act} />
          <Target
            t={t}
            tasks={tasks}
            config={config}
            act={act}
            showError={showError}
          />
          <ReadOnlyRepos
            t={t}
            tasks={tasks}
            config={config}
            act={act}
            showError={showError}
          />
          <Declared t={t} config={config} />
          <CapabilityToggles t={t} config={config} act={act} />
          <Dependencies t={t} tasks={tasks} act={act} onOpenTask={onOpenTask} />
        </div>
      </div>
      {showPrompt && (
        <PromptOverlay taskId={t.id} onClose={() => setShowPrompt(false)} />
      )}
    </Overlay>
  );
}

// EditTaskForm is the title/description editor DetailOverlay swaps its
// header for (bwsalmon/agents#523) -- those two fields are the only ones
// BuildPrompt actually hands a dispatched run, so an edit to either has
// to reach one already running, not just show up here; PATCH /api/tasks/
// {id} (ui.Client.UpdateTask) is what records that as an addendum
// comment (noteEdit) for orchestrator.addendaPoller to pick up. Every
// other UpdateTaskRequest field has its own editor on this same page --
// repo and base in Target, auto-merge in Actions, and reads,
// capabilities and dependencies in the three pickers below them -- so
// this form does not attempt to cover them too.
function EditTaskForm({ t, templates, act, onDone }) {
  const [title, setTitle] = useState(t.title);
  const [description, setDescription] = useState(t.description || "");
  // The review attached to this task (grain/task-284). Editable here and
  // not only on the new-task form because this is where the decision
  // usually gets made: a task is filed, and somewhere between queued and
  // finished somebody decides its change is one they want a second agent
  // over before it merges. An edit reaches any review not yet filed --
  // orchestrator.SyncReviews resolves this once the task's own work is
  // done rather than at creation -- which is why the picker locks once
  // there is a review task to point at.
  const [reviewTemplateId, setReviewTemplateId] = useState(
    t.reviewTemplateId || "",
  );
  const reviewFiled = Boolean(t.reviewTask);

  // Mirrors Timeline's own send(): act() already reports a failure to
  // the user (showError, wired in by App.jsx), so there is nothing left
  // for this form to do differently on one -- closing either way is the
  // same "fire it and stop showing the form" precedent send() already
  // set for a reply.
  const save = async () => {
    if (!title.trim()) return;
    await act(
      () =>
        api(`/api/tasks/${t.id}`, {
          method: "PATCH",
          body: JSON.stringify({ title, description, reviewTemplateId }),
        }),
      t.id,
    );
    onDone();
  };

  return (
    <Stack spacing={1.5} sx={{ mb: 2 }}>
      <TextField
        label="Title"
        value={title}
        onChange={(e) => setTitle(e.target.value)}
        autoFocus
        fullWidth
        size="small"
      />
      <TextField
        label="Description"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        multiline
        rows={5}
        fullWidth
        size="small"
      />
      <TextField
        select
        label="Review"
        value={reviewTemplateId}
        onChange={(e) => setReviewTemplateId(e.target.value)}
        // displayEmpty: "" is a real choice (no review), not a blank.
        SelectProps={{ displayEmpty: true }}
        disabled={reviewFiled || (templates || []).length === 0}
        helperText={
          reviewFiled
            ? `Task ${t.reviewTask} is already reviewing this one -- exactly one review is ever filed per task`
            : (templates || []).length === 0
              ? "no templates yet -- write one in Templates to be able to attach a review here"
              : "run a second agent over this task's own code before it merges, with instructions from a template"
        }
        fullWidth
        size="small"
      >
        <MenuItem value="">No review</MenuItem>
        {(templates || []).map((tmpl) => (
          <MenuItem key={tmpl.id} value={tmpl.id}>
            {tmpl.name}
          </MenuItem>
        ))}
      </TextField>
      <Stack direction="row" spacing={1} justifyContent="flex-end">
        <Button onClick={onDone}>Cancel</Button>
        <Button variant="contained" disabled={!title.trim()} onClick={save}>
          Save
        </Button>
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
//
// What is left here is what nothing on this page can edit yet: the
// sandbox shape, the framework, the prompt extension, the review and the
// interactive flag. Every row this used to open with -- repo, base,
// reads, auto-merge -- is now the control that sets it, above (Target,
// ReadOnlyRepos) or in Actions (the auto-merge checkbox), and a static
// row repeating one of them would be the same field on screen twice in a
// 260px column.
function Declared({ t, config }) {
  const rows = [];
  // bwsalmon/agents#534, grain/task-41: a per-task sandbox shape
  // override, shown only when set -- most tasks use the deployment
  // default and have none of the three fields, the same "0 means unset"
  // convention that keeps them out of the JSON response at all (Task's
  // own omitempty).
  if (t.sandboxCpus) rows.push(["Sandbox vCPUs", String(t.sandboxCpus)]);
  if (t.sandboxMemoryMb)
    rows.push(["Sandbox memory (MiB)", String(t.sandboxMemoryMb)]);
  if (t.sandboxDiskGb)
    rows.push(["Sandbox disk (GiB)", String(t.sandboxDiskGb)]);
  // Same treatment for a per-task agent framework: shown only when this
  // task overrides the deployment's own, which most do not.
  if (t.agentFramework)
    rows.push(["Agent framework", frameworkLabel(t.agentFramework)]);

  const framework = t.agentFramework || config.agentFramework || "antigravity";
  const modelStr =
    framework === "claude"
      ? config.claudeModel
      : framework === "codex"
        ? config.codexModel
        : config.geminiModel;
  if (modelStr) {
    rows.push(["Model", modelStr]);
  }

  // Same treatment again for a task that replaces the standing
  // instructions its deployment and repo would otherwise give its runs
  // (grain/task-114): shown only when it has one, since most tasks do
  // not and an empty row would suggest they had opted out of something.
  if (t.promptExtension) rows.push(["Prompt extension", t.promptExtension]);
  // The review attached to this task (grain/task-284), shown only when
  // there is one -- and saying whether it has been filed yet, since
  // "this will be reviewed once it is done" and "task 91 is reviewing it
  // right now" are the two different things this row means before and
  // after orchestrator.SyncReviews acts on it. The name is what somebody
  // picked it by; the bare id is the fallback for a template that has
  // since gone (ui.Task.reviewTemplateName).
  //
  // It is the one value in this column that names a template rather
  // than describing this task, so it carries the templates glyph
  // (ItemGlyph.jsx): "this came out of a template" said by the same
  // figure the Templates page and the nav rail use, in front of the
  // name it was picked by.
  if (t.reviewTemplateId) {
    const name = t.reviewTemplateName || t.reviewTemplateId;
    rows.push([
      "Review",
      <GlyphLabel kind="templates" key="review">
        {t.reviewTask ? `${name} (task ${t.reviewTask})` : name}
      </GlyphLabel>,
    ]);
  }
  // Most tasks are not interactive, so this row only shows up for the
  // ones that are -- the same "shown only when set" treatment the
  // sandbox override rows above already get.
  if (t.interactive) rows.push(["Mode", "Interactive"]);
  // Most tasks override none of the above, so this block is usually
  // empty now that the four fields every task has have moved to their
  // own editors -- an empty bordered box saying nothing is worse than no
  // box at all.
  if (rows.length === 0) return null;
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

// Target is the repo and base branch a task's runs check out
// (model.Task.Target and .Base), as the two fields that set them rather
// than as the two rows Declared used to open with: the same picker
// NewTaskOverlay files a task through (RepoField, "Other…" escape hatch
// and all) and a plain box for the branch, each PATCHing
// /api/tasks/{id} as it is settled.
//
// Unlike the read-only repos below, an edit here reaches nothing that is
// already running. The checkout is made once, from the repo and base as
// they stood when the run started (orchestrator.prepareCheckout), so a
// sandbox already up keeps the clone it was built with whatever this
// says afterwards. What an edit does reach is the task's *next* run --
// a retry, or a re-dispatch after a failure -- which is the case worth
// being able to fix without filing the task again: a task pointed at a
// base branch that has since been merged and deleted fails every attempt
// until somebody corrects it, and until now the only correction was a
// new task. The hint under the fields says that on a running task, the
// same way ReadOnlyRepos says the opposite where the opposite is true.
//
// Saved on commit rather than on change (RepoField's own onCommit, and
// blur or Enter on the branch box): every edit here is a request, and a
// text field that saved per keystroke would send one PATCH per letter of
// a branch name and have the server refuse all but the last.
function Target({ t, tasks, config, act, showError }) {
  const serverRepo = t.repo || "";
  const serverBase = t.base || "";
  // Held locally and re-seeded from the task the way ReadOnlyRepos holds
  // its own chips, and for the same reason: the box shows what is being
  // typed into it, which is not the task's base until it is committed.
  // Keyed on the value rather than on the object, since t is a fresh
  // object on every poll.
  const [base, setBase] = useState(serverBase);
  useEffect(() => {
    setBase(serverBase);
  }, [serverBase]);
  // repoSeed exists to undo a refused repo edit. RepoField keeps its own
  // uncontrolled <select>/<input> (defaultValue), so a rejected PATCH
  // would otherwise leave it displaying a repo the task does not have,
  // with nothing to correct it: the server value has not changed, so
  // re-rendering shows the same field in the same state. Bumping this
  // changes the key, which remounts the picker on the repo the task
  // actually has.
  const [repoSeed, setRepoSeed] = useState(0);
  // suggestFor is the repo this pane last changed the task to, and the
  // only thing the base suggestion below is offered for -- see it.
  const [suggestFor, setSuggestFor] = useState("");

  // act() refreshes the task once the edit lands, as every other editor
  // on this page does. The failure half is handled here rather than left
  // to act(), because a refused edit has to put the field back first:
  // both controls are already showing the value that was sent by the
  // time the server answers.
  const patch = async (body, undo) => {
    try {
      await api(`/api/tasks/${t.id}`, {
        method: "PATCH",
        body: JSON.stringify(body),
      });
    } catch (err) {
      undo();
      showError(err);
      return false;
    }
    await act(() => Promise.resolve(), t.id);
    return true;
  };

  const changeRepo = async (picked) => {
    const repo = picked.trim();
    // "" is what an emptied text box comes back as, and ui.Client.
    // UpdateTask refuses it outright ("a task with no target cannot be
    // dispatched") -- there is no way to un-target a task, so this is a
    // no-op rather than a request that could only ever fail. The picker
    // itself is `required` whenever the task has a repo, so its "—" is
    // not even offered there; a task filed with no repo at all keeps it,
    // as the honest display of a task that has none.
    if (repo === "" || repo === serverRepo) return;
    if (!(await patch({ repo }, () => setRepoSeed((n) => n + 1)))) return;
    setSuggestFor(repo);
  };

  const commitBase = () => {
    const next = base.trim();
    if (next === serverBase) return;
    setBase(next);
    setSuggestFor("");
    patch({ base: next }, () => setBase(serverBase));
  };

  // The suggestion NewTaskOverlay prefills "Base branch" with
  // (lastBaseForRepo, bwsalmon/agents#641), offered here instead of
  // applied: on a task that already has a base, silently rewriting it
  // because the repo changed would be an edit nobody made. Offered only
  // after a repo change made in this pane, because that is the moment
  // the base already on the task stops being evidence of anything -- a
  // standing "other tasks here branch off release/2" note on a task
  // nobody has touched would be second-guessing a branch somebody chose.
  //
  // Computed over the other tasks, never this one: this task is usually
  // the most recent against its own repo, so counting it would only ever
  // suggest back the base it already has.
  const others = (tasks || []).filter((other) => other.id !== t.id);
  const suggestion = suggestFor ? lastBaseForRepo(others, suggestFor) : "";
  // An empty suggestion is never offered, though lastBaseForRepo returns
  // one for a repo whose last task deliberately used the default branch:
  // "" is what an empty box already says, and an offer to clear a branch
  // somebody typed reads as a correction rather than a shortcut.
  const suggested = suggestion !== "" && suggestion !== base;

  const applySuggestion = () => {
    setBase(suggestion);
    setSuggestFor("");
    patch({ base: suggestion }, () => setBase(serverBase));
  };

  // config.targetRepos empty is an unrestricted deployment (ui.
  // targetAllowed), where there is nothing to be off. Said here because
  // this picker's "Other…" can name any repo at all and an edit does not
  // park the task the way filing one off the list does (ui.Client.
  // parkOffAllowlist runs on CreateTask alone), so nothing else on the
  // way would mention it -- the task would simply fail its next dispatch
  // with nothing to clone.
  const allowlist = config?.targetRepos || [];
  const offAllowlist =
    serverRepo !== "" &&
    allowlist.length > 0 &&
    !allowlist.includes(serverRepo);

  return (
    <fieldset>
      <legend>Target</legend>
      {/* Wrapped in a label the way NewTaskOverlay wraps the same field:
          RepoField renders a bare <select>/<input> with no caption of its
          own, and in a column of MUI fields that all carry one it would
          otherwise be the only control nothing names. */}
      <Box component="label" sx={{ display: "block" }}>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ display: "block", mb: 0.5 }}
        >
          Repo
        </Typography>
        <RepoField
          key={`${serverRepo}#${repoSeed}`}
          name="repo"
          options={knownRepos(config, tasks)}
          defaultValue={serverRepo}
          required={serverRepo !== ""}
          onCommit={changeRepo}
        />
      </Box>
      <TextField
        label="Base branch"
        placeholder="main"
        value={base}
        onChange={(e) => setBase(e.target.value)}
        // Enter commits by blurring rather than by saving directly, so
        // the two ways of leaving this box cannot both fire for one edit.
        onBlur={commitBase}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          e.target.blur();
        }}
        helperText={
          suggested ? (
            <>
              other tasks on {suggestFor} branch off {suggestion} --{" "}
              <Link component="button" type="button" onClick={applySuggestion}>
                use it
              </Link>
            </>
          ) : (
            "empty starts from the repo's default branch"
          )
        }
        autoComplete="off"
        fullWidth
        size="small"
        sx={{ mt: 1.5 }}
      />
      {offAllowlist && (
        <Alert severity="warning" sx={{ mt: 1, fontSize: "0.8rem" }}>
          {serverRepo} is not on this deployment&apos;s target repo list, so
          nothing here can clone it, push to it or open a pull request against
          it. An operator widens targetRepos under Settings.
        </Alert>
      )}
      {t.state === "running" && (
        <p className="hint">
          This run&apos;s sandbox was checked out when it started, so a change
          here reaches this task&apos;s next run rather than the one in flight.
        </p>
      )}
    </fieldset>
  );
}

// ReadOnlyRepos is the "Reads" row Declared used to render, turned into
// the editor every other thing on this page can be changed through:
// model.Task.Reads, the repos a run may clone but never push to, picked
// with the same field NewTaskOverlay files a task through
// (ReadOnlyReposField, grain/task-241) rather than being fixed forever
// at whatever the task was created with.
//
// An edit here reaches a run already in flight, but only partly, and the
// hint below says which part: the git proxy authorizes every fetch
// against the task's reads as they stand at that moment
// (model.Store.GitScope, gitproxy.ModelAuthorizer.Authorize), so adding
// a repo lets a live sandbox fetch it and removing one stops it at once
// -- but the checkout and the prompt naming those repos both happen once,
// when the run starts (orchestrator.prepareCheckout, BuildPrompt), so a
// repo added now is neither cloned into the sandbox already up nor
// mentioned to the agent working in it. Hence no attempt to block the
// edit on a running task, and no pretence that it is free either: the
// way to tell a live run about it is the comment box, the same as any
// other change of mind mid-run.
function ReadOnlyRepos({ t, tasks, config, act, showError }) {
  // reads is held locally as well as sent, because Reads has no
  // per-entry attach/detach endpoint the way capabilities and
  // dependencies do -- ui.UpdateTaskRequest.Reads replaces the whole set,
  // so each edit PATCHes the list as it now stands, and a second edit
  // made before the refresh lands has to be computed against the first.
  // Reading straight off t.reads instead would make "add a second repo"
  // send a list that had forgotten the first one.
  const [reads, setReads] = useState(() => t.reads || []);
  // Re-seeded when the *value* of the task's reads changes, not on every
  // poll: t.reads is a fresh array each time App re-reads the task, so
  // syncing on its identity would throw away an edit while the PATCH
  // carrying it was still in flight. Joined rather than compared by hand
  // because a dependency array compares by identity too.
  const serverReads = (t.reads || []).join("\n");
  useEffect(() => {
    setReads(serverReads === "" ? [] : serverReads.split("\n"));
  }, [serverReads]);

  // PendingSecret's shape rather than the one-liner CapabilityToggles
  // and Dependencies use: those send an attach/detach the server can
  // only apply or reject on its own, while this sends a whole set that
  // the chips above the box are already showing as picked. A rejected
  // PATCH (parseReads refuses anything that is not owner/name) therefore
  // has to put them back, or the picker goes on displaying a set the
  // task does not have.
  const change = async (next) => {
    const previous = reads;
    setReads(next);
    try {
      await api(`/api/tasks/${t.id}`, {
        method: "PATCH",
        body: JSON.stringify({ reads: next }),
      });
    } catch (err) {
      setReads(previous);
      showError(err);
      return;
    }
    await act(() => Promise.resolve(), t.id);
  };

  return (
    // The fieldset is here for the spacing its neighbours below get, but
    // without their legend: ReadOnlyReposField labels its own box
    // "Read-only repos", and a legend saying it again would put the same
    // words on screen twice in a 260px column.
    <fieldset>
      <ReadOnlyReposField
        options={knownRepos(config, tasks)}
        value={reads}
        onChange={change}
      />
      {t.state === "running" && (
        <p className="hint">
          This run&apos;s sandbox was checked out when it started, so a repo
          added now is not cloned into it and the agent is not told about it --
          comment if the run needs to know. Fetching is allowed or refused as
          this list stands, so a removal takes effect immediately.
        </p>
      )}
    </fieldset>
  );
}

// closePullRequest is deliberately local state that dies with the
// overlay, and starts false every time it is mounted: it is a choice
// about this close, made in the moment (ui.CloseOptions' own doc
// comment), and the one thing it must never become is a preference that
// outlives the task it was ticked on. Closing the panel and reopening it
// unticks it, which is exactly right.
function Actions({ t, config, act }) {
  const closable = closablePullRequest(t);
  const [closePullRequest, setClosePullRequest] = useState(false);
  // The flag is sent on every close, true or false, so that what was
  // asked for is on the wire either way -- and false whenever there is
  // no open pull request to act on, whatever the box was left at before
  // one merged underneath it.
  const close = () =>
    act(
      () =>
        api(`/api/tasks/${t.id}/close`, {
          method: "POST",
          body: JSON.stringify({
            close_pull_request: Boolean(closable) && closePullRequest,
          }),
        }),
      t.id,
    );
  return (
    <Stack className="actions" spacing={1}>
      {t.state === "proposed" && (
        <Button
          variant="contained"
          onClick={() =>
            act(
              () => api(`/api/tasks/${t.id}/approve`, { method: "POST" }),
              t.id,
            )
          }
        >
          Approve
        </Button>
      )}
      {/* Approve's own undo, offered on exactly the state it applies to:
          a queued task has been approved and has not started, so
          clearing that approval puts it back among the proposals rather
          than closing it (Client.WithdrawApproval, which refuses the
          states this button is never shown for). */}
      {t.state === "queued" && (
        <Button
          variant="outlined"
          onClick={() =>
            act(
              () =>
                api(`/api/tasks/${t.id}/withdraw-approval`, { method: "POST" }),
              t.id,
            )
          }
        >
          Withdraw approval
        </Button>
      )}
      {/* Putting a task aside, and picking it back up: offered on
          exactly the states ui.Client.Defer accepts, which are the two a
          task can be in before it has ever run. It is neither closing
          (this task is still meant to happen) nor withdrawing approval
          (the approval survives, so a task deferred off the queue comes
          back onto it) -- so it is its own button rather than a variant
          of either, sitting between them here the way it sits between
          them in the life of a task. */}
      {(t.state === "proposed" || t.state === "queued") && (
        <Button
          variant="outlined"
          title="Put this task aside for later. It stops waiting to run and drops out of the lists until you ask for it, and nothing about it is lost."
          onClick={() =>
            act(() => api(`/api/tasks/${t.id}/defer`, { method: "POST" }), t.id)
          }
        >
          Defer
        </Button>
      )}
      {t.state === "deferred" && (
        <Button
          variant="contained"
          title="Pick this task back up. It returns to the queue, or to the proposals, exactly as it was left."
          onClick={() =>
            act(
              () => api(`/api/tasks/${t.id}/undefer`, { method: "POST" }),
              t.id,
            )
          }
        >
          Undefer
        </Button>
      )}
      {/* Once a task's run has produced a pull request, submitting is
          what puts it on the merge queue for automatic conflict
          resolution and merging. Already-submitted tasks (autoMerge
          already true) have nothing left for this button to do. */}
      {t.pullRequest && !t.autoMerge && (
        <Button
          variant="contained"
          onClick={() =>
            act(
              () => api(`/api/tasks/${t.id}/submit`, { method: "POST" }),
              t.id,
            )
          }
        >
          Submit
        </Button>
      )}
      {/* The flag Submit sets, as the flag rather than as the button:
          ui.Client.Submit is `task.AutoMerge = true` and nothing else, so
          this box and that button are one control, not two -- ticking it
          is what Submit does, and Submit disappears the moment it is
          ticked because there is nothing left for it to do. Which leaves
          the box as the half Submit never was: the way to take a task
          back off the merge queue, and the way to say "merge this when
          it is done" before there is a pull request to submit at all.

          It sits here, among the actions, rather than in Target above:
          auto-merge decides nothing about how a task runs, only what
          happens to what it produced, and this is the column where that
          is decided (Submit, Close, "Close acme/widgets#42 too"). It is
          also where the autoMergeDegraded warning below has always been,
          and that warning keys off the flag itself, so it goes on
          applying whichever of the two set it. */}
      <FormControlLabel
        control={
          <Checkbox
            size="small"
            checked={!!t.autoMerge}
            // The flag to send is computed from the task, not read back
            // off the event's own checkbox: the box is controlled by
            // t.autoMerge, so by the time act() runs the request the DOM
            // has already snapped back to what the task still says.
            onChange={() =>
              act(
                () =>
                  api(`/api/tasks/${t.id}`, {
                    method: "PATCH",
                    body: JSON.stringify({ autoMerge: !t.autoMerge }),
                  }),
                t.id,
              )
            }
          />
        }
        label="Auto-merge once checks pass"
        title="The same flag Submit sets: grain puts this task's pull request on the merge queue and merges it once its checks pass. Untick to take it back off."
        sx={{ display: "flex", m: 0 }}
        slotProps={{ typography: { fontSize: "0.8rem" } }}
      />
      {/* The one field on this page an edit does reach mid-run, and the
          reason it is worth saying: auto-merge is read after the run,
          when its pull request is considered for the merge queue
          (orchestrator's own merge pass), not when the run starts -- the
          opposite of the repo and base above, whose checkout is already
          made by then. */}
      {t.state === "running" && (
        <p className="hint">
          Read after this run finishes, when its pull request is considered for
          the merge queue, so a change now still counts for the run in flight.
        </p>
      )}
      {/* Submit still sets autoMerge -- it just never resolves into an
          actual merge on a deployment whose GitHub credential can't read
          check runs (config.autoMergeDegraded, ui.Config.
          AutoMergeDegraded's own doc comment). Without this, clicking
          Submit here looked identical to clicking nothing at all
          (bwsalmon/agents#483). */}
      {t.autoMerge && config?.autoMergeDegraded && (
        <Alert severity="warning" sx={{ fontSize: "0.8rem" }}>
          Queued for auto-merge, but this deployment can't read pull request
          checks, so it will never merge automatically.
        </Alert>
      )}
      {t.state === "failed" && (
        <Button
          variant="contained"
          onClick={() =>
            act(() => api(`/api/tasks/${t.id}/retry`, { method: "POST" }), t.id)
          }
        >
          Retry
        </Button>
      )}
      {/* The choice grain makes nowhere else: closing this task's pull
          request along with it. Offered only where there is an open one
          to close (state.js's closablePullRequest), unticked every time,
          and read at the moment Close is clicked -- never stored, never
          a default. Closing a pull request on GitHub keeps the branch
          and every commit on it, and reopening the pull request brings
          it back whole, which is what makes it an offer worth making at
          all rather than a way to lose an agent's work. */}
      {t.state !== "closed" && closable && (
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={closePullRequest}
              onChange={(e) => setClosePullRequest(e.target.checked)}
            />
          }
          label={`Close ${closable} too`}
          title={`Closes ${closable} on GitHub without merging it. The branch and its commits are left untouched, and reopening the pull request restores it.`}
          sx={{ display: "flex", m: 0 }}
          slotProps={{ typography: { fontSize: "0.8rem" } }}
        />
      )}
      {t.state === "closed" ? (
        <Button
          variant="outlined"
          onClick={() =>
            act(
              () => api(`/api/tasks/${t.id}/reopen`, { method: "POST" }),
              t.id,
            )
          }
        >
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
            if (
              !confirm(
                cancelPrompt(closable && closePullRequest ? closable : null),
              )
            )
              return;
            close();
          }}
        >
          Cancel
        </Button>
      ) : (
        <Button variant="outlined" color="error" onClick={close}>
          Close
        </Button>
      )}
    </Stack>
  );
}

// cancelPrompt is the running task's confirmation, which has to name the
// pull request when the box beside it is ticked: cancelling a run is
// already destructive enough to confirm, and this is the one path where
// confirming it also shuts a pull request. A cancel that leaves the pull
// request alone reads exactly as it always did.
function cancelPrompt(ref) {
  const base =
    "Cancel this job? Its run will be abandoned: no pull request will be opened for it.";
  if (!ref) return base;
  return `Cancel this job, and close ${ref} on GitHub? Its run will be abandoned. The branch and its commits are kept, and reopening ${ref} restores it.`;
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
const PR_EVENT_LABELS = {
  opened: "PR opened",
  merged: "PR merged",
  closed: "PR closed without merging",
};

// outcome is undefined, not "", for a still-running attempt: the API's
// own Attempt.Outcome carries `omitempty`, so the wire form drops the
// key entirely rather than sending an empty string.
function outcomeLabel(outcome) {
  outcome = outcome || "";
  return (
    OUTCOME_LABELS[outcome] ||
    outcome.charAt(0).toUpperCase() + outcome.slice(1)
  );
}

// setupBreadcrumb is how far grain got in setting an attempt up before
// it stopped -- ui.Attempt.SetupNote, the last phrase grain stamped on
// the run while it was still building its sandbox, cloning its repo or
// minting its credentials, left standing because the run ended before
// its agent ever started.
//
// It is null for almost every attempt, and that is the ordinary case:
// grain clears its last phrase as it hands a run to its agent, so a note
// survives only on a run that never got that far -- a "setup-failed"
// one, or a "failed"/"cancelled" one whose checkout or capabilities
// broke first. Nothing an agent wrote through update_status is ever
// carried here (attemptFrom's own doc comment on why: an agent's last
// words beside a failed attempt read as that attempt's diagnosis and are
// not one).
//
// Only for a finished attempt. A live one is already narrated as
// something happening now, at the top of this same page (runActivity),
// and the wording below is deliberately past tense -- this is a
// breadcrumb, not a status.
//
// The verb follows the outcome: a run somebody cancelled, or one grain
// paused for want of agent budget, did not give up on anything.
function setupBreadcrumb(a) {
  if (!a.setupNote || !a.finishedAt) return null;
  const stopped = a.outcome === "cancelled" || a.outcome === "paused";
  return `${stopped ? "stopped while" : "gave up while"}: ${a.setupNote}`;
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
function timelineEvents(t, zone) {
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
  const shownTransitions =
    attempts.length > 0
      ? transitions.filter((tr) => tr.state !== "running")
      : transitions;
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
      render: () => (
        <div className="timeline-title">
          {STATE_LABELS[tr.state] || tr.state}
        </div>
      ),
    });
  });

  attempts.forEach((a) => {
    // The very first attempt *is* the task's one "running" node -- calling
    // it out as "Attempt #1" only makes sense once there's a second one to
    // tell it apart from (bwsalmon/agents#503).
    const title =
      a.number === 1
        ? outcomeLabel(a.outcome)
        : `Attempt #${a.number} · ${outcomeLabel(a.outcome)}`;
    const breadcrumb = setupBreadcrumb(a);
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
            started {formatDateTime(a.startedAt, zone)}
            {a.finishedAt && (
              <> · finished {formatDateTime(a.finishedAt, zone)}</>
            )}
          </div>
          {/* Marked as grain's own words, the same way the live phrase
              is on the task row (TaskList.jsx): every other sentence
              shown about an attempt is the agent's, and "gave up while"
              is grain talking about the setup it was doing, not the
              agent talking about its work. */}
          {breadcrumb && (
            <div
              className="timeline-setup-note"
              title="How far grain got in setting this attempt up before it stopped"
            >
              <span className="task-activity-by">grain</span>
              {breadcrumb}
            </div>
          )}
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
  const earliestAt =
    shownTransitions.length > 0 ? new Date(shownTransitions[0].at) : null;

  (t.pullRequestEvents || []).forEach((e, i) => {
    let at = e.at ? new Date(e.at) : null;
    if (e.kind === "opened" && at && earliestAt && at < earliestAt)
      at = earliestAt;
    events.push({
      key: `pr-event-${i}`,
      at,
      badge: `pr_${e.kind}`,
      render: () => (
        <>
          <div className="timeline-title">
            {PR_EVENT_LABELS[e.kind] || e.kind}
          </div>
          {t.pullRequest && (
            <div className="timeline-meta">
              <Link
                href={pullRequestUrl(t.pullRequest)}
                target="_blank"
                rel="noopener noreferrer"
              >
                {t.pullRequest}
              </Link>
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
    const who = c.onBehalfOf
      ? `${c.author} on behalf of ${c.onBehalfOf}`
      : c.author;
    events.push({
      key: `comment-${i}`,
      at: c.createdAt ? new Date(c.createdAt) : null,
      badge: "comment",
      render: () => (
        <>
          <div className="timeline-meta">
            {who} · {c.authorKind}
          </div>
          {c.body && (
            <Markdown className="timeline-comment-body">{c.body}</Markdown>
          )}
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
    added.forEach((id) =>
      act(
        () =>
          api(`/api/tasks/${t.id}/capabilities`, {
            method: "POST",
            body: JSON.stringify({ id, attach: true }),
          }),
        t.id,
      ),
    );
    removed.forEach((id) =>
      act(
        () =>
          api(`/api/tasks/${t.id}/capabilities`, {
            method: "POST",
            body: JSON.stringify({ id, attach: false }),
          }),
        t.id,
      ),
    );
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
          renderValue={(sel) =>
            sel.length === 0 ? (
              <span className="hint">None</span>
            ) : (
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                {sel.map((id) => {
                  const c = rows.find((cap) => cap.id === id);
                  return <Chip key={id} size="small" label={c ? c.name : id} />;
                })}
              </Box>
            )
          }
        >
          {rows.map((c) => {
            const unavailable = capabilityUnavailableHint(c);
            return (
              <MenuItem
                key={c.id}
                value={c.id}
                title={
                  unavailable
                    ? `${c.description}\n\n${unavailable}`
                    : c.description
                }
              >
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

// PendingSecret is what a run's request_secret call turns into for the
// person reading the task: a box for the one credential it asked for,
// beside the request itself, which is the last thing on the timeline
// below.
//
// It is not the reply box, and that is the whole point. A reply is a
// comment -- stored in the task's conversation, visible to everyone who
// opens it, and fed back into the next run's prompt -- so a credential
// typed there would be handed to the agent that asked for it and written
// into grain's state in plain text. What is typed here goes to PUT
// /api/tasks/{id}/secret, straight into the encrypted secret store, and
// into no comment. Nothing reads a stored value back out (see
// SecretField, and pkg/secrets' own doc comment), so this is one-way:
// the run gets the *use* of the credential on its next attempt, never
// the material.
//
// t.pendingSecret is nil for almost every task, and this renders nothing
// for those.
function PendingSecret({ t, act, showError }) {
  const [value, setValue] = useState("");
  const pending = t.pendingSecret;
  if (!pending) return null;

  // Unlike the reply box below, a rejected value stays in the box rather
  // than being cleared: this is usually something pasted out of a
  // password manager, and losing it to a 400 would mean going and
  // fetching it again. api throws on a non-2xx, so the early return is
  // the failure path -- act is only reached once the value is stored,
  // and is what re-reads the task (now queued again, with no secret
  // pending) and the list behind it.
  const submit = async () => {
    if (value.trim() === "") return;
    try {
      await api(`/api/tasks/${t.id}/secret`, {
        method: "PUT",
        body: JSON.stringify({ value }),
      });
    } catch (err) {
      showError(err);
      return;
    }
    setValue("");
    await act(() => Promise.resolve(), t.id);
  };

  return (
    <Alert severity="info" icon={false} sx={{ mb: 2 }}>
      <strong>This task&apos;s run asked for a secret.</strong>{" "}
      {pending.set
        ? `A value is already stored as ${pending.secret}/${pending.key}; setting one here replaces it.`
        : `Nothing is stored as ${pending.secret}/${pending.key} yet.`}{" "}
      The value goes straight into grain&apos;s secret store -- it is never
      shown to the agent, never added to this conversation, and cannot be read
      back out. Setting it puts the task back in the queue; replying below
      instead does the same without storing anything.
      <Stack direction="row" spacing={1} alignItems="flex-start" sx={{ mt: 1 }}>
        <TextField
          type="password"
          size="small"
          fullWidth
          autoComplete="off"
          label={pending.name}
          placeholder={
            pending.set ? "replace the stored value" : "paste a value to store"
          }
          helperText={`stored as ${pending.secret}/${pending.key} -- write-only, never shown or read back`}
          InputLabelProps={{ shrink: true }}
          inputProps={{ "aria-label": `Value for ${pending.name}` }}
          value={value}
          onChange={(evt) => setValue(evt.target.value)}
          onKeyDown={(evt) => {
            if (evt.key === "Enter") {
              evt.preventDefault();
              submit();
            }
          }}
        />
        <Button
          type="button"
          variant="contained"
          disabled={value.trim() === ""}
          onClick={submit}
          sx={{ mt: 0.5 }}
        >
          Set secret
        </Button>
      </Stack>
    </Alert>
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

  const add = (picked) =>
    act(
      () =>
        api(`/api/tasks/${t.id}/depends-on`, {
          method: "POST",
          body: JSON.stringify({ id: picked.id, attach: true }),
        }),
      t.id,
    );

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
            <Tooltip
              key={id}
              title={dependencyTooltip(id, dep, blocking)}
              placement="left"
            >
              <Chip
                variant={blocking ? "outlined" : "filled"}
                color={blocking ? "warning" : "default"}
                label={`${id}${dep ? ` ${dep.title}` : ""}${blocking ? " (open)" : ""}`}
                onClick={() => onOpenTask(id)}
                onDelete={() =>
                  act(
                    () =>
                      api(`/api/tasks/${t.id}/depends-on`, {
                        method: "POST",
                        body: JSON.stringify({ id, attach: false }),
                      }),
                    t.id,
                  )
                }
                deleteIcon={<span title={`Remove dependency on ${id}`}>×</span>}
                sx={{
                  width: "100%",
                  justifyContent: "space-between",
                  "& .MuiChip-label": {
                    flex: 1,
                    minWidth: 0,
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                  },
                }}
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
  const zone = useTimeZone();
  const events = timelineEvents(t, zone);
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
    const uploaded = await Promise.all(
      attachments.map((f) => fileToAttachment(f)),
    );
    await act(
      () =>
        api(`/api/tasks/${t.id}/comments`, {
          method: "POST",
          body: JSON.stringify({ body, attachments: uploaded }),
        }),
      t.id,
    );
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
              className={
                e.attempt
                  ? "timeline-item timeline-item-attempt"
                  : "timeline-item"
              }
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
                    e.badge === "running"
                      ? e.current
                        ? " badge-mark"
                        : " badge-static"
                      : ""
                  }`}
                >
                  <StateDot
                    state={e.badge}
                    live={e.current}
                    title={STATE_LABELS[e.badge] || e.badge}
                  />
                </span>
              </div>
              <div className="timeline-body">
                {e.at && (
                  <div className="timeline-when">
                    {formatDateTime(e.at, zone)}
                  </div>
                )}
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
        <Button variant="outlined" onClick={send}>
          Comment
        </Button>
      </div>
    </div>
  );
}
