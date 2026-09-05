// paths.js maps App.jsx's own navigation state -- which of the four
// sidebar destinations is showing, whether a task's or a repo's own page
// is open, whether the settings or system overlay is open -- onto real
// URL paths and back, so each is directly loadable/bookmarkable/
// shareable instead of reachable only by clicking through from "/"
// (bwsalmon/agents#548).
//
// Kept as two pure functions rather than pulling in a routing library:
// with only these few (mostly orthogonal) bits of state to encode,
// App's own effects -- mount, a state change, a popstate event -- can
// call straight into them without a router component owning the page
// tree the way the rest of App already does by hand.
//
// Logs and Sandbox health used to be sidebar destinations of their own
// (bwsalmon/agents#457, bwsalmon/agents#536), then moved into Settings'
// own Debug tab (bwsalmon/agents#623) and so lost their own paths, the
// same way Upgrade lives there without ever having had a path of its
// own (and Secrets did too, until grain/task-110 gave each secret to
// whatever uses it). System (Logs, Sandbox health, Top and the reboot
// control together) is a sidebar destination again now (bwsalmon/
// agents#640), with /system as its own path -- a stale bookmark to the
// old /logs, /sandboxes or /debug still just falls back to the tasks
// view like any other unrecognized path. Metrics left that pane for a sidebar
// entry of its own (grain/task-173) and took /metrics with it, so the
// report an operator reads weekly is a link they can keep.

// How a task view is narrowed -- the search box, the sort order and the
// attribute filters its toolbar offers (taskFilters.js) -- rides in the
// query string of the paths that show one (grain/task-317). Those
// refinements were local component state until a list could be narrowed
// by half a dozen menus at once; at that point "the failed gcp-key
// tasks on acme/widgets" stops being a gesture somebody repeats and
// becomes a view worth bookmarking, linking to a colleague, and getting
// back to with the browser's own Back button -- which is exactly what
// every other piece of navigable state here already gets.
//
// It goes in the query rather than in the path because it is not a
// different page: /?repo=acme/widgets is the same task list as /, asked
// a narrower question, and the path stays what the view *is*.
import { FILTERS, NO_NARROWING, SORTS } from "./taskFilters.js";

// "board" is the same tasks as "tasks", laid out as a Kanban board
// (TaskBoard.jsx, grain/task-287) -- a destination of its own rather
// than a mode of the tasks view, so /board is a link somebody can keep
// and a reload lands back on the board rather than on the flat list.
const VIEWS = ["tasks", "board", "repos", "schedules", "templates", "suites"];

// parsePath turns a URL path into the {view, taskId, repo, showReleases,
// scheduleId, templateId, suiteId, showSettings, showSystem, showMetrics}
// App needs in order to restore on load or on a back/forward
// navigation. Anything it
// doesn't recognize -- an unknown segment, a stray trailing slash --
// falls back to the default tasks view rather than erroring, the same as
// an unknown in-app route today just lands on "/".
//
// A repo's own page (RepoPage, grain/task-111) takes two segments
// rather than one, since a repo is named "owner/name" -- /repos/acme/
// widgets -- and its releases pane hangs off that as a third. An
// "owner" with no "name" is not a repo, so /repos/acme lands on the
// repo list the same way any other unrecognized path lands on "/".
//
// A schedule, a template and a suite each take one segment under their
// own list -- /schedules/sched-1 -- the way /tasks/42 already does
// (grain/task-139). Opening one of the four fills the same pane beside
// the sidebar (grain/task-94), so all four are the same kind of link:
// loadable cold, survives a reload, and back closes the pane rather
// than the page.
//
// `search` is window.location.search, and is where a narrowing comes
// from: parsePath reports one -- as `narrowing`, absent when the query
// asks for none -- only on the paths that actually show a task view,
// the same paths buildPath writes one onto. A query on any other path
// (a hand-edited /schedules?repo=acme/widgets, or the /settings the
// address bar showed while an overlay covered a narrowed list) is
// ignored rather than half-restored, which is the treatment /settings
// already gives the view it was opened over.
//
// A narrowing naming something that has since gone -- a repo that is no
// longer a target, a capability that was removed -- is parsed as
// written and left to the view to resolve: filterViews already reads a
// choice that is not among the options it can offer as "any"
// (taskFilters.js), so a stale link opens the list it names rather than
// an empty one or an error.
export function parsePath(pathname, search = "") {
  const parsed = parseSegments(pathname);
  const narrowing = showsTaskView(parsed) ? parseNarrowing(search) : null;
  return narrowing ? { ...parsed, narrowing } : parsed;
}

function parseSegments(pathname) {
  const segments = pathname.split("/").filter(Boolean);
  if (segments[0] === "settings") {
    return { view: "tasks", showSettings: true };
  }
  if (segments[0] === "system") {
    return { view: "tasks", showSystem: true };
  }
  if (segments[0] === "metrics") {
    return { view: "tasks", showMetrics: true };
  }
  if (segments[0] === "tasks" && segments[1]) {
    return { view: "tasks", taskId: segments[1] };
  }
  if (segments[0] === "repos" && segments[1] && segments[2]) {
    const repo = `${segments[1]}/${segments[2]}`;
    if (segments[3] === "releases")
      return { view: "repos", repo, showReleases: true };
    return { view: "repos", repo };
  }
  if (segments[0] === "schedules" && segments[1]) {
    return { view: "schedules", scheduleId: segments[1] };
  }
  if (segments[0] === "templates" && segments[1]) {
    return { view: "templates", templateId: segments[1] };
  }
  if (segments[0] === "suites" && segments[1]) {
    return { view: "suites", suiteId: segments[1] };
  }
  if (VIEWS.includes(segments[0])) {
    return { view: segments[0] };
  }
  return { view: "tasks" };
}

// showsTaskView is whether a parsed path has a task view on screen --
// the flat list, the board, or the list on a repo's own page -- and so
// whether a narrowing means anything on it. An overlay's path
// (/settings, /system, /metrics), an open task and the releases pane all
// say no: each of those covers the list rather than narrowing it, and
// none of them survives in the URL as "the thing behind the overlay"
// today either.
function showsTaskView(parsed) {
  if (parsed.showSettings || parsed.showSystem || parsed.showMetrics)
    return false;
  if (parsed.taskId) return false;
  if (parsed.view === "repos") return !!parsed.repo && !parsed.showReleases;
  return parsed.view === "tasks" || parsed.view === "board";
}

// parseNarrowing reads the query into the {search, sortBy, filters}
// shape the task views take (taskFilters.js's NO_NARROWING), or null
// when the query narrows nothing -- so a plain "/" parses to exactly
// what it did before there was a query to read.
//
// Every value is taken on trust except the sort, which names one of a
// fixed set of orders and falls back to the backlog's own when it names
// something this build has never heard of. A filter value cannot be
// checked here at all -- whether "acme/widgets" is a repo any task
// carries is a question about the tasks, which paths.js has never seen.
function parseNarrowing(search) {
  const params = new URLSearchParams(search || "");
  const q = params.get("q") || "";
  const sort = params.get("sort") || "";
  const sortBy = SORTS[sort] ? sort : NO_NARROWING.sortBy;
  const filters = {};
  for (const f of FILTERS) {
    const value = params.get(f.id);
    if (value) filters[f.id] = value;
  }
  if (
    q === "" &&
    sortBy === NO_NARROWING.sortBy &&
    Object.keys(filters).length === 0
  )
    return null;
  return { search: q, sortBy, filters };
}

// narrowingQuery is parseNarrowing's inverse: "?q=ci&repo=acme/widgets"
// for what is actually narrowed, "" for what isn't -- so an un-narrowed
// list still lives at "/" rather than at "/?".
//
// Parameters are written in a fixed order (the search, the sort, then
// FILTERS' own order) so the same narrowing always produces the same
// URL, and the "/" in a repo or a base branch is left as itself rather
// than percent-encoded: it is legal in a query string, and this is a
// link somebody reads before they send it.
function narrowingQuery(narrowing) {
  if (!narrowing) return "";
  const params = new URLSearchParams();
  if (narrowing.search) params.set("q", narrowing.search);
  if (narrowing.sortBy && narrowing.sortBy !== NO_NARROWING.sortBy)
    params.set("sort", narrowing.sortBy);
  const filters = narrowing.filters || {};
  for (const f of FILTERS) {
    if (filters[f.id]) params.set(f.id, filters[f.id]);
  }
  const query = params.toString().replace(/%2F/g, "/");
  return query ? `?${query}` : "";
}

// buildPath is parsePath's inverse: the path App's current state should
// be showing at any moment. App diffs this against window.location on
// every relevant state change to decide whether the address bar needs
// updating at all.
//
// The narrowing rides along on the paths showsTaskView admits and is
// dropped from the rest, so opening a task or the settings pane over a
// narrowed list leaves the query behind for as long as that pane is up
// and closing it puts the query back -- App holds the narrowing itself
// throughout, the same way it goes on holding the view behind
// /settings.
export function buildPath({
  view,
  taskId,
  repo,
  showReleases,
  scheduleId,
  templateId,
  suiteId,
  showSettings,
  showSystem,
  showMetrics,
  narrowing,
}) {
  const query = narrowingQuery(narrowing);
  if (showSettings) return "/settings";
  if (showSystem) return "/system";
  if (showMetrics) return "/metrics";
  if (taskId) return `/tasks/${taskId}`;
  // An open repo only means anything within the repos view -- App keeps
  // the two together, and the sidebar clears the repo on the way out of
  // that view, so this is belt and braces rather than a case that
  // arises. The same goes for each open item below: a schedule is only
  // showing while the schedules view is.
  if (view === "repos" && repo)
    return `/repos/${repo}${showReleases ? "/releases" : query}`;
  if (view === "schedules" && scheduleId) return `/schedules/${scheduleId}`;
  if (view === "templates" && templateId) return `/templates/${templateId}`;
  if (view === "suites" && suiteId) return `/suites/${suiteId}`;
  if (view === "tasks") return `/${query}`;
  if (view === "board") return `/board${query}`;
  return `/${view}`;
}
