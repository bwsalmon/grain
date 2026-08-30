import { useCallback, useEffect, useRef, useState } from "react";
import { Chip } from "@mui/material";
import api from "./api.js";
import Sidebar from "./components/Sidebar.jsx";
import TaskList from "./components/TaskList.jsx";
import RepoList from "./components/RepoList.jsx";
import SchedulesList from "./components/SchedulesList.jsx";
import BatchActionsBar from "./components/BatchActionsBar.jsx";
import ErrorBanner from "./components/ErrorBanner.jsx";
import DetailOverlay from "./components/DetailOverlay.jsx";
import NewTaskOverlay from "./components/NewTaskOverlay.jsx";
import SettingsOverlay from "./components/SettingsOverlay.jsx";
import RepoReleases from "./components/RepoReleases.jsx";
import LogsPage from "./components/LogsPage.jsx";

// POLL_INTERVAL_MS is how long the UI can be out of date by.
//
// A task changes state when graind dispatches it, when a run finishes,
// and when a pull request merges -- none of which the browser is told
// about, so without this the screen only moves when somebody clicks. A
// few seconds is far below what anyone notices and the request is a
// list of a handful of rows against a store on the same machine.
const POLL_INTERVAL_MS = 3000;

export default function App() {
  const [config, setConfig] = useState(null);
  const [tasks, setTasks] = useState([]);
  const [schedules, setSchedules] = useState([]);
  const [stateFilter, setStateFilter] = useState("all");
  // view switches the main pane between the flat task list, the repo
  // page, and the schedules page; repoFilter is orthogonal to
  // stateFilter and survives a trip through the repo page and back,
  // since "which repo" and "which state" are two independent questions
  // about the same task list.
  const [view, setView] = useState("tasks");
  const [repoFilter, setRepoFilter] = useState(null);
  // releasesRepo is which repo's release pane is open within the repos
  // view (null shows the repo list instead) -- see RepoList's own
  // "Releases" button.
  const [releasesRepo, setReleasesRepo] = useState(null);
  const [error, setError] = useState(null);
  const [openTaskId, setOpenTaskId] = useState(null);
  const [detail, setDetail] = useState(null);
  const [showNewTask, setShowNewTask] = useState(false);
  // newTaskRepo is the repo the "+" on a repo page row was opened from
  // (bwsalmon/agents#474); null means "no override", so the overlay
  // falls back to repoFilter the same way it always has for the
  // sidebar's own "+ New task" button.
  const [newTaskRepo, setNewTaskRepo] = useState(null);
  const [showSettings, setShowSettings] = useState(false);
  const [selected, setSelected] = useState(() => new Set());
  const polling = useRef(false);

  const showError = useCallback((err) => {
    setError(String(err.message || err));
  }, []);

  useEffect(() => {
    if (error === null) return;
    const t = setTimeout(() => setError(null), 5000);
    return () => clearTimeout(t);
  }, [error]);

  const refreshList = useCallback(async () => {
    const next = await api("/api/tasks");
    setTasks(next);
    // Drop any selected id the store no longer reports, so a stale
    // selection never quietly outlives the task it named.
    const ids = new Set(next.map((t) => t.id));
    setSelected((prev) => {
      const kept = new Set([...prev].filter((id) => ids.has(id)));
      return kept.size === prev.size ? prev : kept;
    });
  }, []);

  const refreshSchedules = useCallback(async () => {
    setSchedules(await api("/api/schedules"));
  }, []);

  const toggleSelect = useCallback((id) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  }, []);

  const setSelection = useCallback((ids, checked) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (checked) next.add(id); else next.delete(id);
      }
      return next;
    });
  }, []);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  const openTask = useCallback(async (id) => {
    try {
      const d = await api(`/api/tasks/${id}`);
      setOpenTaskId(id);
      setDetail(d);
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  const closeDetail = useCallback(() => {
    setOpenTaskId(null);
    setDetail(null);
  }, []);

  // openRepo is the repo page's row click: scope the task list to that
  // repo and switch back to it, the same as clicking a repo chip
  // anywhere else would.
  const openRepo = useCallback((repo) => {
    setRepoFilter(repo);
    setReleasesRepo(null);
    setView("tasks");
  }, []);

  // openNewTaskForRepo is the repo page's own "+" button: file a task
  // against that specific repo without disturbing repoFilter, which is
  // "what the tasks view is scoped to" and has nothing to do with which
  // repo's row happened to be clicked here.
  const openNewTaskForRepo = useCallback((repo) => {
    setNewTaskRepo(repo);
    setShowNewTask(true);
  }, []);

  // setViewAndCloseReleases is Sidebar's onSetView: any nav click leaves
  // the repos view's release pane behind, so returning to "repos" later
  // should land back on the repo list rather than a stale release pane.
  const setViewAndCloseReleases = useCallback((v) => {
    setReleasesRepo(null);
    setView(v);
  }, []);

  // act runs a mutation, then re-fetches the task (and the list behind
  // it) so the screen reflects what the store now reports -- never the
  // value the UI optimistically assumed it wrote.
  const act = useCallback(async (mutate, id) => {
    try {
      await mutate();
      await openTask(id);
      await refreshList();
    } catch (err) {
      showError(err);
    }
  }, [openTask, refreshList, showError]);

  // actBatch is `act` (above) widened to many tasks at once: run one
  // mutation per id, in parallel, then refresh the list a single time
  // rather than once per task. Unlike `act` it never opens a detail
  // overlay -- a batch action has no one task to show. Failures are
  // collected rather than aborting the rest, since one bad id in a
  // multi-select should not stop the others from going through; it
  // reports whether every mutation landed so the caller can decide
  // whether the selection that drove it is still worth keeping.
  const actBatch = useCallback(async (ids, mutate) => {
    const results = await Promise.allSettled(ids.map((id) => mutate(id)));
    await refreshList();
    const failed = results.filter((r) => r.status === "rejected");
    if (failed.length > 0) {
      showError(new Error(`${failed.length} of ${ids.length} task(s) failed: ${failed[0].reason?.message || failed[0].reason}`));
    }
    return failed.length === 0;
  }, [refreshList, showError]);

  // Only clears the selection once a batch action fully succeeds -- on a
  // partial failure it stays as-is, so whoever is watching can see which
  // rows are still selected and retry rather than having to re-pick them.
  const runBatch = useCallback((mutate) => {
    actBatch([...selected], mutate).then((ok) => { if (ok) clearSelection(); });
  }, [actBatch, selected, clearSelection]);

  useEffect(() => {
    (async () => {
      try {
        const cfg = await api("/api/config");
        setConfig(cfg);
        await Promise.all([refreshList(), refreshSchedules()]);
      } catch (err) {
        showError(err);
      }
    })();
  }, [refreshList, refreshSchedules, showError]);

  useEffect(() => {
    async function poll() {
      if (polling.current || document.visibilityState === "hidden") return;
      polling.current = true;
      try {
        await refreshList();
        if (openTaskId !== null) {
          setDetail(await api(`/api/tasks/${openTaskId}`));
        }
        if (view === "schedules") {
          await refreshSchedules();
        }
      } catch (err) {
        // Deliberately quiet -- see app.js's own poll for why.
        console.warn("grain: poll failed", err);
      } finally {
        polling.current = false;
      }
    }
    const interval = setInterval(poll, POLL_INTERVAL_MS);
    const onVisible = () => {
      if (document.visibilityState === "visible") poll();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [openTaskId, refreshList, refreshSchedules, view]);

  const scopedTasks = repoFilter ? tasks.filter((t) => t.repo === repoFilter) : tasks;

  return (
    <div className="app-shell">
      <Sidebar
        config={config}
        view={view}
        onSetView={setViewAndCloseReleases}
        tasks={tasks}
        schedules={schedules}
        stateFilter={stateFilter}
        onSetFilter={setStateFilter}
        onOpenSettings={() => setShowSettings(true)}
        onOpenNewTask={() => { setNewTaskRepo(null); setShowNewTask(true); }}
      />
      {view === "repos" && releasesRepo !== null ? (
        <RepoReleases repo={releasesRepo} onBack={() => setReleasesRepo(null)} showError={showError} />
      ) : view === "repos" ? (
        <RepoList
          tasks={tasks}
          config={config}
          onOpenRepo={openRepo}
          onOpenReleases={setReleasesRepo}
          onOpenTask={openTask}
          onNewTask={openNewTaskForRepo}
        />
      ) : view === "schedules" ? (
        <SchedulesList schedules={schedules} config={config} tasks={tasks} onRefresh={refreshSchedules} showError={showError} />
      ) : view === "logs" ? (
        <LogsPage showError={showError} />
      ) : (
        <div className="main-column">
          {repoFilter !== null && (
            <div className="repo-scope-bar">
              <Chip
                label={`Repo: ${repoFilter}`}
                onDelete={() => setRepoFilter(null)}
                deleteIcon={<span title="Clear repo filter">×</span>}
              />
            </div>
          )}
          <TaskList
            tasks={scopedTasks}
            stateFilter={stateFilter}
            config={config}
            onOpenTask={openTask}
            selected={selected}
            onToggleSelect={toggleSelect}
            onSelectAll={setSelection}
          />
          <BatchActionsBar count={selected.size} config={config} onRun={runBatch} onClear={clearSelection} />
        </div>
      )}
      {error !== null && <ErrorBanner message={error} />}
      {openTaskId !== null && detail !== null && (
        <DetailOverlay task={detail} tasks={tasks} config={config} onClose={closeDetail} onOpenTask={openTask} act={act} showError={showError} />
      )}
      {showNewTask && (
        <NewTaskOverlay
          tasks={tasks}
          config={config}
          defaultRepo={newTaskRepo !== null ? newTaskRepo : repoFilter}
          onClose={() => setShowNewTask(false)}
          onCreated={refreshList}
          showError={showError}
        />
      )}
      {showSettings && <SettingsOverlay config={config} onClose={() => setShowSettings(false)} showError={showError} />}
    </div>
  );
}
