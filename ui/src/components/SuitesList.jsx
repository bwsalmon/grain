import { useState } from "react";
import { Button, Chip } from "@mui/material";
import { knownRepos } from "../state.js";
import SuiteOverlay from "./SuiteOverlay.jsx";
import SuiteRunOverlay from "./SuiteRunOverlay.jsx";
import { ListEmpty, ListHeader } from "./ListPrimitives.jsx";
import ItemGlyph from "./ItemGlyph.jsx";

// STATUS_LABELS/STATUS_COLORS render model.TaskSuiteRunStatus's own
// three values the way a human reads them -- Chip's own "color" prop
// vocabulary, TaskList's own state-chip precedent applied to a run
// instead of a task.
const STATUS_LABELS = {
  active: "Active",
  succeeded: "Succeeded",
  failed: "Failed",
};
const STATUS_COLORS = { active: "info", succeeded: "success", failed: "error" };

function describeMode(run) {
  return run.mode === "count"
    ? `run ${run.count}×`
    : `run until clean (max ${run.maxPasses})`;
}

// SuitesList is the suites page's main pane (bwsalmon/agents#642):
// the suite templates a human has saved, each runnable against any repo
// and branch, and -- what the issue itself asks for -- the status of
// every run, outstanding or finished, so a run started five minutes ago
// (or five days ago) is still visible without hunting through the task
// list for the tasks it filed. TemplatesList/SchedulesList's own
// "flat list, click a row to edit, a '+' button to add one" shape,
// with a second list (runs) beneath it since a suite's own status lives
// in what it has run, not in the suite itself.
//
// Which suite is open is App.jsx's state (openSuiteId), not this
// component's, so the URL can name it -- SchedulesList's own doc
// comment on why (grain/task-139). Starting a run stays local: a run is
// an action to fill in and dismiss, not something you open.
export default function SuitesList({
  suites,
  suiteRuns,
  templates = [],
  config,
  tasks,
  openSuiteId,
  onOpenSuite,
  onRefresh,
  onRefreshRuns,
  onRefreshTemplates,
  showError,
}) {
  const [showNew, setShowNew] = useState(false);
  const [running, setRunning] = useState(null); // suite id a "Run" click opened SuiteRunOverlay for, or true for the bare "+ Run" button
  const repoOptions = knownRepos(config, tasks);
  const editing = suites.find((s) => s.id === openSuiteId) || null;

  const suiteName = (id) => suites.find((s) => s.id === id)?.name || id;

  return (
    <main>
      <ListHeader
        title="Suites"
        icon={<ItemGlyph kind="suites" size={20} />}
        count={suites.length}
        action={
          <Button
            variant="contained"
            size="small"
            sx={{ ml: "auto" }}
            onClick={() => setShowNew(true)}
          >
            + New suite
          </Button>
        }
      />
      <ul className="template-list">
        {suites.map((s) => (
          <li
            className="template-row"
            key={s.id}
            onClick={() => onOpenSuite(s.id)}
          >
            <span className="template-name">{s.name}</span>
            <Chip size="small" label={describeMode(s)} />
            <Chip
              size="small"
              variant="outlined"
              label={`${s.items.length} template${s.items.length === 1 ? "" : "s"}`}
            />
            {s.requireApproval && (
              <Chip size="small" label="Requires approval" />
            )}
            <Button
              size="small"
              sx={{ ml: "auto" }}
              onClick={(e) => {
                e.stopPropagation();
                setRunning(s.id);
              }}
            >
              Run…
            </Button>
          </li>
        ))}
      </ul>
      {suites.length === 0 && (
        <ListEmpty>
          No suites yet -- combine one or more templates into a suite to qualify
          a branch before merging it, sweep for bugs, and the like.
        </ListEmpty>
      )}

      <ListHeader
        title="Runs"
        count={suiteRuns.length}
        style={{ marginTop: "1.5rem" }}
      />
      <ul className="template-list">
        {suiteRuns.map((r) => (
          <li className="template-row" key={r.id}>
            <span className="template-name">
              {r.suiteName || suiteName(r.suiteId)}
            </span>
            <Chip size="small" label={r.repo} />
            <Chip size="small" variant="outlined" label={r.base} />
            <Chip
              size="small"
              color={STATUS_COLORS[r.status]}
              label={STATUS_LABELS[r.status] || r.status}
            />
            {r.scheduleId && (
              <Chip size="small" variant="outlined" label="Scheduled" />
            )}
            <span className="template-title hint">
              pass {r.pass} · {describeMode(r)}
            </span>
            {r.error && (
              <span
                className="template-title hint"
                style={{ color: "var(--mui-palette-error-main, #c62828)" }}
              >
                {r.error}
              </span>
            )}
          </li>
        ))}
      </ul>
      {suiteRuns.length === 0 && <ListEmpty>No suite runs yet.</ListEmpty>}

      {showNew && (
        <SuiteOverlay
          templates={templates}
          repoOptions={repoOptions}
          config={config}
          onClose={() => setShowNew(false)}
          onSaved={onRefresh}
          onTemplatesChanged={onRefreshTemplates}
          showError={showError}
        />
      )}
      {editing && (
        <SuiteOverlay
          suite={editing}
          templates={templates}
          repoOptions={repoOptions}
          config={config}
          onClose={() => onOpenSuite(null)}
          onSaved={onRefresh}
          onTemplatesChanged={onRefreshTemplates}
          showError={showError}
        />
      )}
      {running && (
        <SuiteRunOverlay
          suites={suites}
          suiteId={running === true ? undefined : running}
          repoOptions={repoOptions}
          onClose={() => setRunning(null)}
          onStarted={onRefreshRuns}
          showError={showError}
        />
      )}
    </main>
  );
}
