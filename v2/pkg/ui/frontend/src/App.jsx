import { useCallback, useEffect, useRef, useState } from "react";
import api from "./api.js";
import TopBar from "./components/TopBar.jsx";
import Filters from "./components/Filters.jsx";
import TaskList from "./components/TaskList.jsx";
import RepoList from "./components/RepoList.jsx";
import ErrorBanner from "./components/ErrorBanner.jsx";
import DetailOverlay from "./components/DetailOverlay.jsx";
import NewTaskOverlay from "./components/NewTaskOverlay.jsx";
import SettingsOverlay from "./components/SettingsOverlay.jsx";
import SecretsOverlay from "./components/SecretsOverlay.jsx";

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
  const [stateFilter, setStateFilter] = useState("all");
  // view switches the main pane between the flat task list and the repo
  // page; repoFilter is orthogonal to stateFilter and survives a trip
  // through the repo page and back, since "which repo" and "which
  // state" are two independent questions about the same task list.
  const [view, setView] = useState("tasks");
  const [repoFilter, setRepoFilter] = useState(null);
  const [error, setError] = useState(null);
  const [openTaskId, setOpenTaskId] = useState(null);
  const [detail, setDetail] = useState(null);
  const [showNewTask, setShowNewTask] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const [showSecrets, setShowSecrets] = useState(false);
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
    setTasks(await api("/api/tasks"));
  }, []);

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
    setView("tasks");
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

  useEffect(() => {
    (async () => {
      try {
        const cfg = await api("/api/config");
        setConfig(cfg);
        await refreshList();
      } catch (err) {
        showError(err);
      }
    })();
  }, [refreshList, showError]);

  useEffect(() => {
    async function poll() {
      if (polling.current || document.visibilityState === "hidden") return;
      polling.current = true;
      try {
        await refreshList();
        if (openTaskId !== null) {
          setDetail(await api(`/api/tasks/${openTaskId}`));
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
  }, [openTaskId, refreshList]);

  const scopedTasks = repoFilter ? tasks.filter((t) => t.repo === repoFilter) : tasks;

  return (
    <>
      <TopBar
        config={config}
        view={view}
        onSetView={setView}
        onOpenSecrets={() => setShowSecrets(true)}
        onOpenSettings={() => setShowSettings(true)}
        onOpenNewTask={() => setShowNewTask(true)}
      />
      {view === "repos" ? (
        <RepoList tasks={tasks} onOpenRepo={openRepo} />
      ) : (
        <>
          {repoFilter !== null && (
            <div className="repo-scope-bar">
              <span className="chip dependency-chip">
                Repo: {repoFilter}
                <button type="button" className="chip-remove" title="Clear repo filter" onClick={() => setRepoFilter(null)}>×</button>
              </span>
            </div>
          )}
          <Filters tasks={scopedTasks} stateFilter={stateFilter} onSetFilter={setStateFilter} />
          <TaskList tasks={scopedTasks} stateFilter={stateFilter} config={config} onOpenTask={openTask} />
        </>
      )}
      {error !== null && <ErrorBanner message={error} />}
      {openTaskId !== null && detail !== null && (
        <DetailOverlay task={detail} config={config} onClose={closeDetail} onOpenTask={openTask} act={act} />
      )}
      {showNewTask && (
        <NewTaskOverlay config={config} defaultRepo={repoFilter} onClose={() => setShowNewTask(false)} onCreated={refreshList} showError={showError} />
      )}
      {showSettings && <SettingsOverlay onClose={() => setShowSettings(false)} showError={showError} />}
      {showSecrets && <SecretsOverlay onClose={() => setShowSecrets(false)} showError={showError} />}
    </>
  );
}
