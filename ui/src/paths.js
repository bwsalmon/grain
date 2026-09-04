// paths.js maps App.jsx's own navigation state -- which of the four
// sidebar destinations is showing, whether a task's or a repo's own page
// is open, whether the settings or debug overlay is open -- onto real
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
// whatever uses it). Debug (Logs, Sandbox health, Top and the reboot
// control together) is a sidebar destination again now (bwsalmon/
// agents#640), with /debug as its own path -- a stale bookmark to the
// old /logs or /sandboxes still just falls back to the tasks view like
// any other unrecognized path. Metrics left that pane for a sidebar
// entry of its own (grain/task-173) and took /metrics with it, so the
// report an operator reads weekly is a link they can keep.

// "board" is the same tasks as "tasks", laid out as a Kanban board
// (TaskBoard.jsx, grain/task-287) -- a destination of its own rather
// than a mode of the tasks view, so /board is a link somebody can keep
// and a reload lands back on the board rather than on the flat list.
const VIEWS = ["tasks", "board", "repos", "schedules", "templates", "suites"];

// parsePath turns a URL path into the {view, taskId, repo, showReleases,
// scheduleId, templateId, suiteId, showSettings, showDebug, showMetrics}
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
export function parsePath(pathname) {
  const segments = pathname.split("/").filter(Boolean);
  if (segments[0] === "settings") {
    return { view: "tasks", showSettings: true };
  }
  if (segments[0] === "debug") {
    return { view: "tasks", showDebug: true };
  }
  if (segments[0] === "metrics") {
    return { view: "tasks", showMetrics: true };
  }
  if (segments[0] === "tasks" && segments[1]) {
    return { view: "tasks", taskId: segments[1] };
  }
  if (segments[0] === "repos" && segments[1] && segments[2]) {
    const repo = `${segments[1]}/${segments[2]}`;
    if (segments[3] === "releases") return { view: "repos", repo, showReleases: true };
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

// buildPath is parsePath's inverse: the path App's current state should
// be showing at any moment. App diffs this against window.location on
// every relevant state change to decide whether the address bar needs
// updating at all.
export function buildPath({
  view, taskId, repo, showReleases, scheduleId, templateId, suiteId, showSettings, showDebug, showMetrics,
}) {
  if (showSettings) return "/settings";
  if (showDebug) return "/debug";
  if (showMetrics) return "/metrics";
  if (taskId) return `/tasks/${taskId}`;
  // An open repo only means anything within the repos view -- App keeps
  // the two together, and the sidebar clears the repo on the way out of
  // that view, so this is belt and braces rather than a case that
  // arises. The same goes for each open item below: a schedule is only
  // showing while the schedules view is.
  if (view === "repos" && repo) return `/repos/${repo}${showReleases ? "/releases" : ""}`;
  if (view === "schedules" && scheduleId) return `/schedules/${scheduleId}`;
  if (view === "templates" && templateId) return `/templates/${templateId}`;
  if (view === "suites" && suiteId) return `/suites/${suiteId}`;
  if (view === "tasks") return "/";
  return `/${view}`;
}
