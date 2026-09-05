import { useCallback, useEffect, useRef, useState } from "react";
import api from "./api.js";
import { buildPath, parsePath } from "./paths.js";
import { NO_NARROWING } from "./taskFilters.js";
import { TimeZoneProvider } from "./TimeZoneContext.jsx";
import Sidebar from "./components/Sidebar.jsx";
import TaskList from "./components/TaskList.jsx";
import TaskBoard from "./components/TaskBoard.jsx";
import RepoList from "./components/RepoList.jsx";
import RepoPage from "./components/RepoPage.jsx";
import SchedulesList from "./components/SchedulesList.jsx";
import TemplatesList from "./components/TemplatesList.jsx";
import SuitesList from "./components/SuitesList.jsx";
import BatchActionsBar from "./components/BatchActionsBar.jsx";
import ErrorBanner from "./components/ErrorBanner.jsx";
import DetailOverlay from "./components/DetailOverlay.jsx";
import NewTaskOverlay from "./components/NewTaskOverlay.jsx";
import SettingsOverlay from "./components/SettingsOverlay.jsx";
import SystemOverlay from "./components/SystemOverlay.jsx";
import MetricsOverlay from "./components/MetricsOverlay.jsx";
import RepoReleases from "./components/RepoReleases.jsx";
import LoadingScreen from "./components/LoadingScreen.jsx";
import BannerStrip from "./components/BannerStrip.jsx";
import ReconcilerDownBanner from "./components/ReconcilerDownBanner.jsx";
import AgentPauseBanner from "./components/AgentPauseBanner.jsx";
import HostSandboxesBanner from "./components/HostSandboxesBanner.jsx";

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
  // narrowing is how the task views are narrowed right now -- the search
  // text, the sort order and the attribute filters their shared toolbar
  // offers (taskFilters.js) -- lifted out of TaskList/TaskBoard and
  // seeded from the query string the page loaded with, which is also
  // where every change to it is written back (grain/task-317). A list
  // narrowed by several menus at once is a view worth linking to, so it
  // gets the same treatment as which sub-page is showing: loadable cold,
  // survives a reload, and the Back button undoes it.
  //
  // One narrowing, not one per view: the flat list, the board and the
  // list on a repo's own page all ask taskFilters.js's question, so
  // moving between them keeps it rather than silently dropping it.
  // (The repo page's own copy is already scoped to that repo, which is
  // why its Repo menu is hidden there -- a repo picked on the flat list
  // simply has nothing to narrow while that page is up, and is back in
  // force on the way out. The query goes on naming it there, which is
  // the price of not throwing it away: the URL still describes exactly
  // the state it reopens, since the same choice is inert again on the
  // way back in.)
  const [narrowing, setNarrowing] = useState(
    () =>
      parsePath(window.location.pathname, window.location.search).narrowing ||
      NO_NARROWING,
  );
  // narrow takes a patch rather than a whole narrowing, so a toolbar
  // control only has to name what it changed.
  const narrow = useCallback(
    (patch) => setNarrowing((prev) => ({ ...prev, ...patch })),
    [],
  );
  // view switches the main pane between the flat task list, the repos
  // pane, and the schedules page.
  //
  // view, openRepo and showSettings (below) all seed from the URL the
  // page loaded with rather than a fixed default, so a direct link to
  // /repos, /repos/acme/widgets or /settings lands on that sub-page
  // instead of always opening on the task list first (bwsalmon/agents
  // #548, grain/task-111).
  const [view, setView] = useState(
    () => parsePath(window.location.pathname).view,
  );
  // openRepo is which repo's own page is showing within the repos view
  // (null shows the repo list instead), and releasesOpen whether that
  // page's release pane is showing over it -- the repo page is the only
  // way into releases now that the list's own "Releases" button is gone.
  //
  // The repo page is also what scopes a task list to one repo, which the
  // task view's own repoFilter chip used to do: opening a repo lists its
  // tasks *on the repo's page*, so there is no second, filtered flavour
  // of the flat list to keep in step with it.
  const [openRepo, setOpenRepo] = useState(
    () => parsePath(window.location.pathname).repo || null,
  );
  const [releasesOpen, setReleasesOpen] = useState(
    () => parsePath(window.location.pathname).showReleases === true,
  );
  // openScheduleId/openTemplateId/openSuiteId are which schedule,
  // template or suite is showing as a pane over its own list -- the
  // openTaskId (below) of the other three lists. They live here rather
  // than inside SchedulesList/TemplatesList/SuitesList so that buildPath
  // can name the open item in the URL the way it already names an open
  // task or an open repo (grain/task-139): opening one is the same
  // gesture with the same result in all four lists (grain/task-94), so
  // it is the same kind of link too.
  //
  // Each seeds straight from the URL the page loaded with, and needs no
  // fetch of its own to do it -- unlike a task's detail, which is a
  // round trip and so a mount effect below. App already holds all three
  // lists in full, so restoring one of these panes is just finding the
  // row this id names once its list has landed.
  const [openScheduleId, setOpenScheduleId] = useState(
    () => parsePath(window.location.pathname).scheduleId || null,
  );
  const [openTemplateId, setOpenTemplateId] = useState(
    () => parsePath(window.location.pathname).templateId || null,
  );
  const [openSuiteId, setOpenSuiteId] = useState(
    () => parsePath(window.location.pathname).suiteId || null,
  );
  const [error, setError] = useState(null);
  const [openTaskId, setOpenTaskId] = useState(null);
  const [detail, setDetail] = useState(null);
  const [showNewTask, setShowNewTask] = useState(false);
  // newTaskRepo is the repo the "New task" button on a repo's own page
  // was opened from (bwsalmon/agents#474, grain/task-111); null means
  // "no override", so the overlay falls back to whichever repo page is
  // open, the same as the sidebar's own "+ New task" button does.
  const [newTaskRepo, setNewTaskRepo] = useState(null);
  const [showSettings, setShowSettings] = useState(
    () => parsePath(window.location.pathname).showSettings === true,
  );
  const [showSystem, setShowSystem] = useState(
    () => parsePath(window.location.pathname).showSystem === true,
  );
  const [showMetrics, setShowMetrics] = useState(
    () => parsePath(window.location.pathname).showMetrics === true,
  );
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

  // All three of these drop an open item the store no longer reports --
  // one deleted from its own pane, or a URL naming one that never
  // existed -- so the address bar never keeps naming a pane that isn't
  // there: refreshList's own pruning of `selected` above, applied to
  // each list's own open item. It hangs off the fetch rather than off an
  // effect watching the list because all three lists start out empty,
  // and an empty list nothing has fetched yet says nothing about
  // whether the item the URL names exists.
  const refreshSchedules = useCallback(async () => {
    const next = await api("/api/schedules");
    setSchedules(next);
    setOpenScheduleId((prev) =>
      prev === null || next.some((s) => s.id === prev) ? prev : null,
    );
  }, []);

  const refreshTemplates = useCallback(async () => {
    const next = await api("/api/templates");
    setTemplates(next);
    setOpenTemplateId((prev) =>
      prev === null || next.some((t) => t.id === prev) ? prev : null,
    );
  }, []);

  const refreshSuites = useCallback(async () => {
    const next = await api("/api/suites");
    setSuites(next);
    setOpenSuiteId((prev) =>
      prev === null || next.some((s) => s.id === prev) ? prev : null,
    );
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

  // refreshAfterCreate is what the new-task form is given instead of
  // refreshList alone: filing a task now also writes a setting this
  // response carries. Picking which end of the backlog it joins is
  // remembered as the deployment's default for the next one
  // (ui.CreateTaskRequest.AtFront, config.newestFirst), and the form
  // seeds its picker from that -- so without re-reading the config here,
  // the choice would only show up on the next full page load.
  const refreshAfterCreate = useCallback(async () => {
    await refreshList();
    await refreshConfig();
  }, [refreshList, refreshConfig]);

  const toggleSelect = useCallback((id) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const setSelection = useCallback((ids, checked) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  const openTask = useCallback(
    async (id) => {
      try {
        const d = await api(`/api/tasks/${id}`);
        setOpenTaskId(id);
        setDetail(d);
      } catch (err) {
        showError(err);
      }
    },
    [showError],
  );

  const closeDetail = useCallback(() => {
    setOpenTaskId(null);
    setDetail(null);
  }, []);

  // openTaskFromMetrics is the one link out of the Metrics pane: the
  // report's backlog names the oldest queued task, and clicking it
  // should land on that task. It closes the metrics pane on the way --
  // both fill the same area beside the sidebar, and MetricsOverlay is
  // mounted after DetailOverlay here, so leaving it open would put the
  // task the click asked for behind the pane it was clicked in.
  const openTaskFromMetrics = useCallback(
    (id) => {
      setShowMetrics(false);
      openTask(id);
    },
    [openTask],
  );

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

  // closeOpenItems clears whichever list's item pane is open -- only one
  // of the three can be, since each belongs to its own view. An open
  // pane is a modal dialog over the sidebar (Overlay.jsx), so today
  // nothing can navigate with one open and this never fires; it is here
  // so that "which item is open" can't outlive the view it means
  // anything in and quietly reopen on the way back to that view.
  const closeOpenItems = useCallback(() => {
    setOpenScheduleId(null);
    setOpenTemplateId(null);
    setOpenSuiteId(null);
  }, []);

  // setViewAndCloseOpen is Sidebar's onSetView: any nav click leaves an
  // open repo -- and an open schedule, template or suite -- behind, so a
  // later click on that destination lands back on its list rather than
  // on whichever item was last open there.
  const setViewAndCloseOpen = useCallback(
    (v) => {
      closeRepoPage();
      closeOpenItems();
      setView(v);
    },
    [closeRepoPage, closeOpenItems],
  );

  // act runs a mutation, then re-fetches the task (and the list behind
  // it) so the screen reflects what the store now reports -- never the
  // value the UI optimistically assumed it wrote.
  const act = useCallback(
    async (mutate, id) => {
      try {
        await mutate();
        await openTask(id);
        await refreshList();
      } catch (err) {
        showError(err);
      }
    },
    [openTask, refreshList, showError],
  );

  // actBatch is `act` (above) widened to many tasks at once: run one
  // mutation per id, in parallel, then refresh the list a single time
  // rather than once per task. Unlike `act` it never opens a detail
  // overlay -- a batch action has no one task to show. Failures are
  // collected rather than aborting the rest, since one bad id in a
  // multi-select should not stop the others from going through; it
  // reports whether every mutation landed so the caller can decide
  // whether the selection that drove it is still worth keeping.
  const actBatch = useCallback(
    async (ids, mutate) => {
      const results = await Promise.allSettled(ids.map((id) => mutate(id)));
      await refreshList();
      const failed = results.filter((r) => r.status === "rejected");
      if (failed.length > 0) {
        showError(
          new Error(
            `${failed.length} of ${ids.length} task(s) failed: ${failed[0].reason?.message || failed[0].reason}`,
          ),
        );
      }
      return failed.length === 0;
    },
    [refreshList, showError],
  );

  // Only clears the selection once a batch action fully succeeds -- on a
  // partial failure it stays as-is, so whoever is watching can see which
  // rows are still selected and retry rather than having to re-pick them.
  const runBatch = useCallback(
    (mutate) => {
      actBatch([...selected], mutate).then((ok) => {
        if (ok) clearSelection();
      });
    },
    [actBatch, selected, clearSelection],
  );

  // reorderTasks is TaskList's drag-and-drop drop handler (bwsalmon/
  // agents#476): afterId/beforeId are either the id of a task or null,
  // TaskList's own way of saying "no bound on this side" (dropped at the
  // very head or the very tail of the list), which the API expects as an
  // absent field rather than a literal null.
  const reorderTasks = useCallback(
    async (ids, afterId, beforeId) => {
      try {
        await api("/api/tasks/reorder", {
          method: "POST",
          body: JSON.stringify({
            ids,
            afterId: afterId || undefined,
            beforeId: beforeId || undefined,
          }),
        });
        await refreshList();
      } catch (err) {
        showError(err);
      }
    },
    [refreshList, showError],
  );

  useEffect(() => {
    (async () => {
      try {
        const cfg = await api("/api/config");
        setConfig(cfg);
        await Promise.all([
          refreshList(),
          refreshSchedules(),
          refreshTemplates(),
          refreshSuites(),
        ]);
      } catch (err) {
        showError(err);
      }
    })();
  }, [
    refreshList,
    refreshSchedules,
    refreshTemplates,
    refreshSuites,
    showError,
  ]);

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
        // config.agentPause is the second such field and the reason this
        // poll now also matters on the way back: a pause appears when the
        // agent's usage limit is met and disappears again when the
        // provider's window resets, with nothing on this end to notice
        // either (grain/task-132).
        await Promise.all([refreshList(), refreshConfig()]);
        if (openTaskId !== null) {
          setDetail(await api(`/api/tasks/${openTaskId}`));
        }
        if (view === "schedules") {
          // All three, not just schedules: ScheduleOverlay's own
          // "Template" and "Suite" pickers (opened from
          // SchedulesList.jsx) need up-to-date lists too, since either can
          // open at any time while this pane is on screen.
          await Promise.all([
            refreshSchedules(),
            refreshTemplates(),
            refreshSuites(),
          ]);
        } else if (view === "templates") {
          await refreshTemplates();
        } else if (view === "suites") {
          // Templates too: SuiteOverlay's own template picker needs an
          // up-to-date list, ScheduleOverlay's own reasoning applied here.
          await Promise.all([
            refreshSuites(),
            refreshTemplates(),
            refreshSuiteRuns(),
          ]);
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
  }, [
    openTaskId,
    refreshConfig,
    refreshList,
    refreshSchedules,
    refreshTemplates,
    refreshSuites,
    refreshSuiteRuns,
    view,
  ]);

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

  // Keeps the address bar in sync with view/openTaskId/openScheduleId/
  // showSettings -- everything paths.js's buildPath encodes -- so every
  // way of reaching a sub-page (a sidebar click, closing an overlay, the
  // popstate handler below) ends up shareable/bookmarkable, not just a
  // page loaded fresh from one. Skips the update entirely once the
  // computed path already matches the address bar, which is what stops
  // this from fighting the popstate handler: a back/forward navigation
  // updates window.location itself before this effect ever runs, so by
  // the time it does there is nothing left to push.
  const mountedRef = useRef(false);
  useEffect(() => {
    const path = buildPath({
      view,
      taskId: openTaskId,
      repo: openRepo,
      showReleases: releasesOpen,
      scheduleId: openScheduleId,
      templateId: openTemplateId,
      suiteId: openSuiteId,
      showSettings,
      showSystem,
      showMetrics,
      narrowing,
    });
    if (path !== window.location.pathname + window.location.search) {
      // Going somewhere pushes; narrowing where you already are
      // replaces. A narrowing is a change of question rather than of
      // page, and the search box writes one per keystroke -- pushing
      // those would fill the history with a step per character and make
      // Back mean "delete the last letter I typed" rather than "the
      // page I was on". The URL is still the current one either way,
      // which is the whole point: it can be copied, reloaded and sent.
      // The toolbar's own Clear button is how a narrowing is undone.
      //
      // The very first correction (e.g. an unrecognized path normalized
      // back to "/") replaces rather than pushes too, so a mistyped or
      // stale URL doesn't leave an extra "back" step to it in history.
      const samePage = path.split("?")[0] === window.location.pathname;
      if (mountedRef.current && !samePage) {
        window.history.pushState(null, "", path);
      } else {
        window.history.replaceState(null, "", path);
      }
    }
    mountedRef.current = true;
  }, [
    view,
    openTaskId,
    openRepo,
    releasesOpen,
    openScheduleId,
    openTemplateId,
    openSuiteId,
    showSettings,
    showSystem,
    showMetrics,
    narrowing,
  ]);

  // Mirrors the browser's own back/forward buttons onto the same state
  // buildPath/parsePath already govern everything else through.
  useEffect(() => {
    function onPopState() {
      const parsed = parsePath(
        window.location.pathname,
        window.location.search,
      );
      // A path that carries no narrowing puts the toolbar back to
      // asking nothing -- Back out of a narrowed list is how you undo
      // the narrowing, so it cannot be the one thing that survives.
      setNarrowing(parsed.narrowing || NO_NARROWING);
      setShowSettings(parsed.showSettings === true);
      setShowSystem(parsed.showSystem === true);
      setShowMetrics(parsed.showMetrics === true);
      setView(parsed.view);
      setOpenRepo(parsed.repo || null);
      setReleasesOpen(parsed.showReleases === true);
      setOpenScheduleId(parsed.scheduleId || null);
      setOpenTemplateId(parsed.templateId || null);
      setOpenSuiteId(parsed.suiteId || null);
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
  //
  // The narrowing is the flat list's own, shared rather than copied, so
  // /repos/acme/widgets?capability=gcp-key is as much a link as
  // /?capability=gcp-key is -- and a search typed on a repo's page is
  // still there on the way back out to the whole backlog.
  const taskListPane = (list, filter) => (
    <>
      <TaskList
        tasks={list}
        stateFilter={filter}
        config={config}
        narrowing={narrowing}
        onNarrow={narrow}
        onOpenTask={openTask}
        selected={selected}
        onToggleSelect={toggleSelect}
        onSelectAll={setSelection}
        onReorder={reorderTasks}
      />
      <BatchActionsBar
        count={selected.size}
        config={config}
        onRun={runBatch}
        onClear={clearSelection}
      />
    </>
  );

  // taskBoardPane is the same pair for the board (TaskBoard.jsx): the
  // columns, and the same batch-actions bar under them, so a card's
  // checkbox feeds the selection the list's rows do. Rendered both as
  // the board view itself and, scoped to one repo, as the body of that
  // repo's page when its switch is on "Board" (grain/task-321) -- the
  // same board either way, columns included, since the columns are one
  // layout for every board in this browser (board.js).
  //
  // It takes no state filter for the reason the board view never had
  // one: the columns *are* the board's answer to which states it is
  // about, and a board pre-filtered to one state would be a single
  // column. The narrowing it does take is the one every task view
  // shares (see taskListPane), so a search survives the switch between
  // a repo's list and a repo's board as it does the switch between
  // pages.
  const taskBoardPane = (list) => (
    <>
      <TaskBoard
        tasks={list}
        config={config}
        narrowing={narrowing}
        onNarrow={narrow}
        onOpenTask={openTask}
        selected={selected}
        onToggleSelect={toggleSelect}
        onSelectAll={setSelection}
        onReorder={reorderTasks}
      />
      <BatchActionsBar
        count={selected.size}
        config={config}
        onRun={runBatch}
        onClear={clearSelection}
      />
    </>
  );

  // Every timestamp under here is printed in the deployment's own zone
  // rather than the browser's (grain/task-368) -- config is loaded
  // before the first paint, so nothing renders in one zone and corrects
  // itself into another.
  return (
    <TimeZoneProvider zone={config?.timeZone}>
      <div className="app-shell">
        {config === null ? (
          <LoadingScreen />
        ) : (
          <>
            <Sidebar
              config={config}
              view={view}
              onSetView={setViewAndCloseOpen}
              tasks={tasks}
              schedules={schedules}
              templates={templates}
              suites={suites}
              stateFilter={stateFilter}
              onSetFilter={setStateFilter}
              showSettings={showSettings}
              showSystem={showSystem}
              showMetrics={showMetrics}
              onOpenSettings={() => setShowSettings(true)}
              onOpenSystem={() => setShowSystem(true)}
              onOpenMetrics={() => setShowMetrics(true)}
              onOpenNewTask={() => {
                setNewTaskRepo(null);
                setShowNewTask(true);
              }}
            />
            {view === "repos" && openRepo !== null && releasesOpen ? (
              <RepoReleases
                repo={openRepo}
                templates={templates}
                onBack={() => setReleasesOpen(false)}
                showError={showError}
              />
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
                {/* Both views over the same scoped tasks, and the page
                  picks -- see RepoPage's own doc comment for why the
                  choice lives there and the panes here. */}
                {(repoView) =>
                  repoView === "board"
                    ? taskBoardPane(tasks.filter((t) => t.repo === openRepo))
                    : taskListPane(
                        tasks.filter((t) => t.repo === openRepo),
                        "all",
                      )
                }
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
                openScheduleId={openScheduleId}
                onOpenSchedule={setOpenScheduleId}
                onRefresh={refreshSchedules}
                showError={showError}
              />
            ) : view === "templates" ? (
              <TemplatesList
                templates={templates}
                config={config}
                tasks={tasks}
                openTemplateId={openTemplateId}
                onOpenTemplate={setOpenTemplateId}
                onRefresh={refreshTemplates}
                showError={showError}
              />
            ) : view === "suites" ? (
              <SuitesList
                suites={suites}
                suiteRuns={suiteRuns}
                templates={templates}
                config={config}
                tasks={tasks}
                openSuiteId={openSuiteId}
                onOpenSuite={setOpenSuiteId}
                onRefresh={refreshSuites}
                onRefreshRuns={refreshSuiteRuns}
                onRefreshTemplates={refreshTemplates}
                showError={showError}
              />
            ) : view === "board" ? (
              /* Every task, rather than the sidebar's current state
               filter: see taskBoardPane above. */
              <div className="main-column">{taskBoardPane(tasks)}</div>
            ) : (
              <div className="main-column">
                {taskListPane(tasks, stateFilter)}
              </div>
            )}
          </>
        )}
        <BannerStrip>
          {/* Always shown when it is true, above whatever else is:
            host sandboxing is a fact about the deployment rather than
            an incident, so it outlasts the banners below it and must
            not be hidden by one of them (grain/task-15). */}
          {config?.hostSandboxes && <HostSandboxesBanner />}
          {/* One of these two at a time, though: a dead reconcile loop
            is the larger fact -- a deployment that is not dispatching
            at all has no use for being told why it is also not
            dispatching. */}
          {config?.reconcilerDown ? (
            <ReconcilerDownBanner />
          ) : config?.agentPause?.paused ? (
            <AgentPauseBanner
              pause={config.agentPause}
              onLifted={refreshConfig}
              showError={showError}
            />
          ) : null}
        </BannerStrip>
        {error !== null && <ErrorBanner message={error} />}
        {openTaskId !== null && detail !== null && (
          <DetailOverlay
            task={detail}
            tasks={tasks}
            config={config}
            templates={templates}
            onClose={closeDetail}
            onOpenTask={openTask}
            act={act}
            showError={showError}
          />
        )}
        {showNewTask && (
          <NewTaskOverlay
            tasks={tasks}
            config={config}
            templates={templates}
            defaultRepo={newTaskRepo !== null ? newTaskRepo : openRepo}
            onClose={() => setShowNewTask(false)}
            onCreated={refreshAfterCreate}
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
        {showSystem && (
          <SystemOverlay
            config={config}
            onClose={() => setShowSystem(false)}
            showError={showError}
          />
        )}
        {showMetrics && (
          <MetricsOverlay
            onClose={() => setShowMetrics(false)}
            onOpenTask={openTaskFromMetrics}
            showError={showError}
          />
        )}
      </div>
    </TimeZoneProvider>
  );
}
