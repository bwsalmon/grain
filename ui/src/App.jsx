import { useCallback, useEffect, useRef, useState } from "react";
import api from "./api.js";
import { buildPath, parsePath } from "./paths.js";
import Sidebar from "./components/Sidebar.jsx";
import TaskList from "./components/TaskList.jsx";
import RepoList from "./components/RepoList.jsx";
import RepoPage from "./components/RepoPage.jsx";
import SchedulesList from "./components/SchedulesList.jsx";
import TemplatesList from "./components/TemplatesList.jsx";
import SuitesList from "./components/SuitesList.jsx";
import BatchActionsBar from "./components/BatchActionsBar.jsx";
import ErrorBanner from "./components/ErrorBanner.jsx";
import DetailOverlay from "./components/DetailOverlay.jsx";
import NewTaskOverlay from "./components/NewTaskOverlay.jsx";
import ConfigurationAgentButton from "./components/ConfigurationAgentButton.jsx";
import SettingsOverlay from "./components/SettingsOverlay.jsx";
import DebugOverlay from "./components/DebugOverlay.jsx";
import RepoReleases from "./components/RepoReleases.jsx";
import LoadingScreen from "./components/LoadingScreen.jsx";
import ReconcilerDownBanner from "./components/ReconcilerDownBanner.jsx";

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
  const [templates, setTemplates] = useState([]);
  const [suites, setSuites] = useState([]);
  const [suiteRuns, setSuiteRuns] = useState([]);
  const [stateFilter, setStateFilter] = useState("all");
  // view switches the main pane between the flat task list, the repos
  // pane, and the schedules page.
  //
  // view, openRepo and showSettings (below) all seed from the URL the
  // page loaded with rather than a fixed default, so a direct link to
  // /repos, /repos/acme/widgets or /settings lands on that sub-page
  // instead of always opening on the task list first (bwsalmon/agents
  // #548, grain/task-111).
  const [view, setView] = useState(() => parsePath(window.location.pathname).view);
  // openRepo is which repo's own page is showing within the repos view
  // (null shows the repo list instead), and releasesOpen whether that
  // page's release pane is showing over it -- the repo page is the only
  // way into releases now that the list's own "Releases" button is gone.
  //
  // The repo page is also what scopes a task list to one repo, which the
  // task view's own repoFilter chip used to do: opening a repo lists its
  // tasks *on the repo's page*, so there is no second, filtered flavour
  // of the flat list to keep in step with it.
  const [openRepo, setOpenRepo] = useState(() => parsePath(window.location.pathname).repo || null);
  const [releasesOpen, setReleasesOpen] = useState(() => parsePath(window.location.pathname).showReleases === true);
  const [error, setError] = useState(null);
  const [openTaskId, setOpenTaskId] = useState(null);
  const [detail, setDetail] = useState(null);
  const [showNewTask, setShowNewTask] = useState(false);
  // newTaskRepo is the repo the "New task" button on a repo's own page
  // was opened from (bwsalmon/agents#474, grain/task-111); null means
  // "no override", so the overlay falls back to whichever repo page is
  // open, the same as the sidebar's own "+ New task" button does.
  const [newTaskRepo, setNewTaskRepo] = useState(null);
  const [showSettings, setShowSettings] = useState(() => parsePath(window.location.pathname).showSettings === true);
  const [showDebug, setShowDebug] = useState(() => parsePath(window.location.pathname).showDebug === true);
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

  const refreshTemplates = useCallback(async () => {
    setTemplates(await api("/api/templates"));
  }, []);

  const refreshSuites = useCallback(async () => {
    setSuites(await api("/api/suites"));
  }, []);

  const refreshSuiteRuns = useCallback(async () => {
    setSuiteRuns(await api("/api/suite-runs"));
  }, []);

  // refreshConfig re-fetches /api/config -- needed after adding a target
  // repo (RepoList's own "Add") or removing one (RepoPage's own
  // "Remove", bwsalmon/agents#473) since config is otherwise only ever fetched
  // once, at mount, and the repos pane's own list (repoRows) reads
  // config.targetRepos to decide which repos to show and which ones it
  // can offer to remove. It reads config.repoDefaultCapabilities for the
  // same reason, which is why saving a repo's own defaults refreshes
  // this too: a repo listed only because it carries a set stops being
  // listed the moment that set is emptied. A repo's standing
  // instructions are the third thing that can put it on that list
  // (config.reposWithPromptExtension), so saving those refreshes it as
  // well.
  //
  // And every save on the Settings pane (SettingsOverlay's onSaved),
  // since half of what that pane writes is read back out of this
  // response: the target repo allowlist, the deployment-wide default
  // capabilities and task defaults the new-task form starts from, and
  // the deployment's own prompt extension.
  const refreshConfig = useCallback(async () => {
    setConfig(await api("/api/config"));
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

  // openTaskFromDebug is the one link out of the Debug overlay: the
  // metrics panel names the oldest queued task, and clicking it should
  // land on that task. It closes the debug overlay on the way -- both
  // are dialogs, and DebugOverlay is mounted after DetailOverlay here,
  // so leaving it open would put the task the click asked for behind
  // the pane it was clicked in.
  const openTaskFromDebug = useCallback((id) => {
    setShowDebug(false);
    openTask(id);
  }, [openTask]);

  // openRepoPage is the repo list's row click: show that repo's own page
  // (RepoPage, grain/task-111), which is where its tasks and everything
  // else about it live.
  const openRepoPage = useCallback((repo) => {
    setOpenRepo(repo);
    setReleasesOpen(false);
    setView("repos");
  }, []);

  const closeRepoPage = useCallback(() => {
    setOpenRepo(null);
    setReleasesOpen(false);
  }, []);

  // openNewTaskForRepo is the repo page's own "New task" button: file a
  // task against that specific repo. It takes the repo explicitly rather
  // than reading openRepo, since the overlay outlives a navigation away
  // from the page it was opened on.
  const openNewTaskForRepo = useCallback((repo) => {
    setNewTaskRepo(repo);
    setShowNewTask(true);
  }, []);

  // setViewAndCloseRepo is Sidebar's onSetView: any nav click leaves an
  // open repo behind, so a later click on "Repos" lands back on the repo
  // list rather than on whichever repo was last open.
  const setViewAndCloseRepo = useCallback((v) => {
    closeRepoPage();
    setView(v);
  }, [closeRepoPage]);

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

  // reorderTasks is TaskList's drag-and-drop drop handler (bwsalmon/
  // agents#476): afterId/beforeId are either the id of a task or null,
  // TaskList's own way of saying "no bound on this side" (dropped at the
  // very head or the very tail of the list), which the API expects as an
  // absent field rather than a literal null.
  const reorderTasks = useCallback(async (ids, afterId, beforeId) => {
    try {
      await api("/api/tasks/reorder", {
        method: "POST",
        body: JSON.stringify({ ids, afterId: afterId || undefined, beforeId: beforeId || undefined }),
      });
      await refreshList();
    } catch (err) {
      showError(err);
    }
  }, [refreshList, showError]);

  useEffect(() => {
    (async () => {
      try {
        const cfg = await api("/api/config");
        setConfig(cfg);
        await Promise.all([refreshList(), refreshSchedules(), refreshTemplates(), refreshSuites()]);
      } catch (err) {
        showError(err);
      }
    })();
  }, [refreshList, refreshSchedules, refreshTemplates, refreshSuites, showError]);

  // The deployment's name in the browser tab (grain/task-69), beside
  // the sidebar badge Sidebar.jsx renders it as. A tab strip is where a
  // staging window and a production window actually get confused, and it
  // is the one piece of chrome the app cannot draw into -- so the name
  // goes first, since a narrow tab truncates its title from the end and
  // "grain — sta..." would say nothing this is for.
  useEffect(() => {
    const name = config?.environmentName;
    document.title = name ? `${name} — grain` : "grain";
  }, [config?.environmentName]);

  useEffect(() => {
    async function poll() {
      if (polling.current || document.visibilityState === "hidden") return;
      polling.current = true;
      try {
        // refreshConfig here (not just at mount) is what keeps
        // config.reconcilerDown -- and the banner it drives, below --
        // current: it is the one /api/config field that can flip from
        // false to true mid-session, on a process this same UI is still
        // otherwise happily serving (bwsalmon/agents#576).
        await Promise.all([refreshList(), refreshConfig()]);
        if (openTaskId !== null) {
          setDetail(await api(`/api/tasks/${openTaskId}`));
        }
        if (view === "schedules") {
          // All three, not just schedules: ScheduleOverlay's own
          // "Template" and "Task suite" pickers (opened from
          // SchedulesList.jsx) need up-to-date lists too, since either can
          // open at any time while this pane is on screen.
          await Promise.all([refreshSchedules(), refreshTemplates(), refreshSuites()]);
        } else if (view === "templates") {
          await refreshTemplates();
        } else if (view === "suites") {
          // Templates too: SuiteOverlay's own template picker needs an
          // up-to-date list, ScheduleOverlay's own reasoning applied here.
          await Promise.all([refreshSuites(), refreshTemplates(), refreshSuiteRuns()]);
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
  }, [openTaskId, refreshConfig, refreshList, refreshSchedules, refreshTemplates, refreshSuites, refreshSuiteRuns, view]);

  // Opens whatever task /tasks/:id in the URL the page loaded with names
  // -- the view/showSettings equivalent of this runs synchronously in
  // useState's own initializer above, but a task's detail needs an
  // /api/tasks/:id round trip first, which only a mount effect can do.
  // Runs once: openTask is a stable callback (its only dependency,
  // showError, never changes identity either), so this never re-fires
  // just because some other state the effect doesn't list changed.
  useEffect(() => {
    const { taskId } = parsePath(window.location.pathname);
    if (taskId) openTask(taskId);
  }, [openTask]);

  // Keeps the address bar in sync with view/openTaskId/showSettings --
  // the three things paths.js's buildPath encodes -- so every way of
  // reaching a sub-page (a sidebar click, closing an overlay, the
  // popstate handler below) ends up shareable/bookmarkable, not just a
  // page loaded fresh from one. Skips the update entirely once the
  // computed path already matches the address bar, which is what stops
  // this from fighting the popstate handler: a back/forward navigation
  // updates window.location itself before this effect ever runs, so by
  // the time it does there is nothing left to push.
  const mountedRef = useRef(false);
  useEffect(() => {
    const path = buildPath({ view, taskId: openTaskId, repo: openRepo, showReleases: releasesOpen, showSettings, showDebug });
    if (path !== window.location.pathname) {
      // The very first correction (e.g. an unrecognized path normalized
      // back to "/") replaces rather than pushes, so a mistyped or
      // stale URL doesn't leave an extra "back" step to it in history.
      if (mountedRef.current) {
        window.history.pushState(null, "", path);
      } else {
        window.history.replaceState(null, "", path);
      }
    }
    mountedRef.current = true;
  }, [view, openTaskId, openRepo, releasesOpen, showSettings, showDebug]);

  // Mirrors the browser's own back/forward buttons onto the same state
  // buildPath/parsePath already govern everything else through.
  useEffect(() => {
    function onPopState() {
      const parsed = parsePath(window.location.pathname);
      setShowSettings(parsed.showSettings === true);
      setShowDebug(parsed.showDebug === true);
      setView(parsed.view);
      setOpenRepo(parsed.repo || null);
      setReleasesOpen(parsed.showReleases === true);
      if (parsed.taskId) {
        openTask(parsed.taskId);
      } else {
        closeDetail();
      }
    }
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, [openTask, closeDetail]);

  // taskListPane is the flat task list plus the batch-actions bar under
  // it -- rendered both as the tasks view itself and, scoped to one repo
  // and unfiltered, as the body of that repo's page. The same list
  // either way, search/sort/select/drag-reorder included, rather than a
  // poorer second list of tasks on the repo page (RepoPage's own doc
  // comment). A repo page always shows all of the repo's tasks: the
  // sidebar's state filter is a question about the flat list, and
  // picking one there navigates back to it (Sidebar's selectState).
  const taskListPane = (list, filter) => (
    <>
      <TaskList
        tasks={list}
        stateFilter={filter}
        config={config}
        onOpenTask={openTask}
        selected={selected}
        onToggleSelect={toggleSelect}
        onSelectAll={setSelection}
        onReorder={reorderTasks}
      />
      <BatchActionsBar count={selected.size} config={config} onRun={runBatch} onClear={clearSelection} />
    </>
  );

  return (
    <div className="app-shell">
      {config === null ? (
        <LoadingScreen />
      ) : (
        <>
          <Sidebar
            config={config}
            view={view}
            onSetView={setViewAndCloseRepo}
            tasks={tasks}
            schedules={schedules}
            templates={templates}
            suites={suites}
            stateFilter={stateFilter}
            onSetFilter={setStateFilter}
            onOpenSettings={() => setShowSettings(true)}
            onOpenDebug={() => setShowDebug(true)}
            onOpenNewTask={() => { setNewTaskRepo(null); setShowNewTask(true); }}
          />
          {view === "repos" && openRepo !== null && releasesOpen ? (
            <RepoReleases repo={openRepo} templates={templates} onBack={() => setReleasesOpen(false)} showError={showError} />
          ) : view === "repos" && openRepo !== null ? (
            <RepoPage
              repo={openRepo}
              tasks={tasks}
              config={config}
              onBack={closeRepoPage}
              onNewTask={openNewTaskForRepo}
              onOpenReleases={() => setReleasesOpen(true)}
              onRefreshConfig={refreshConfig}
              showError={showError}
            >
              {taskListPane(tasks.filter((t) => t.repo === openRepo), "all")}
            </RepoPage>
          ) : view === "repos" ? (
            <RepoList
              tasks={tasks}
              config={config}
              onOpenRepo={openRepoPage}
              onRefreshConfig={refreshConfig}
              showError={showError}
            />
          ) : view === "schedules" ? (
            <SchedulesList
              schedules={schedules}
              templates={templates}
              suites={suites}
              config={config}
              tasks={tasks}
              onRefresh={refreshSchedules}
              showError={showError}
            />
          ) : view === "templates" ? (
            <TemplatesList templates={templates} config={config} onRefresh={refreshTemplates} showError={showError} />
          ) : view === "suites" ? (
            <SuitesList
              suites={suites}
              suiteRuns={suiteRuns}
              templates={templates}
              config={config}
              tasks={tasks}
              onRefresh={refreshSuites}
              onRefreshRuns={refreshSuiteRuns}
              onRefreshTemplates={refreshTemplates}
              showError={showError}
            />
          ) : (
            <div className="main-column">
              {taskListPane(tasks, stateFilter)}
            </div>
          )}
        </>
      )}
      {config !== null && (
        <ConfigurationAgentButton defaultRepo={openRepo} onOpenTask={openTask} showError={showError} />
      )}
      {config?.reconcilerDown && <ReconcilerDownBanner />}
      {error !== null && <ErrorBanner message={error} />}
      {openTaskId !== null && detail !== null && (
        <DetailOverlay task={detail} tasks={tasks} config={config} onClose={closeDetail} onOpenTask={openTask} act={act} showError={showError} />
      )}
      {showNewTask && (
        <NewTaskOverlay
          tasks={tasks}
          config={config}
          defaultRepo={newTaskRepo !== null ? newTaskRepo : openRepo}
          onClose={() => setShowNewTask(false)}
          onCreated={refreshList}
          onOpenTask={openTask}
          showError={showError}
        />
      )}
      {showSettings && (
        <SettingsOverlay
          onClose={() => setShowSettings(false)}
          onSaved={refreshConfig}
          showError={showError}
        />
      )}
      {showDebug && <DebugOverlay config={config} onClose={() => setShowDebug(false)} onOpenTask={openTaskFromDebug} showError={showError} />}
    </div>
  );
}
