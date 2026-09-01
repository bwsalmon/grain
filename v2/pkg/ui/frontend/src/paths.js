// paths.js maps App.jsx's own navigation state -- which of the four
// sidebar destinations is showing, whether a task's detail overlay is
// open, whether the settings overlay is open -- onto real URL paths and
// back, so each is directly loadable/bookmarkable/shareable instead of
// reachable only by clicking through from "/" (bwsalmon/agents#548).
//
// Kept as two pure functions rather than pulling in a routing library:
// with only these three (mostly orthogonal) bits of state to encode,
// App's own effects -- mount, a state change, a popstate event -- can
// call straight into them without a router component owning the page
// tree the way the rest of App already does by hand.
//
// Logs and Sandbox health used to be sidebar destinations of their own
// (bwsalmon/agents#457, bwsalmon/agents#536) and so had entries here;
// they moved into Settings' own Debug tab (bwsalmon/agents#623), the
// same way Secrets and Upgrade already live there without ever having
// had a path of their own -- a stale bookmark to /logs or /sandboxes now
// just falls back to the tasks view like any other unrecognized path.

const VIEWS = ["tasks", "repos", "schedules", "templates"];

// parsePath turns a URL path into the {view, taskId, showSettings} App
// needs in order to restore on load or on a back/forward navigation.
// Anything it doesn't recognize -- an unknown segment, a stray trailing
// slash -- falls back to the default tasks view rather than erroring,
// the same as an unknown in-app route today just lands on "/".
export function parsePath(pathname) {
  const segments = pathname.split("/").filter(Boolean);
  if (segments[0] === "settings") {
    return { view: "tasks", showSettings: true };
  }
  if (segments[0] === "tasks" && segments[1]) {
    return { view: "tasks", taskId: segments[1] };
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
export function buildPath({ view, taskId, showSettings }) {
  if (showSettings) return "/settings";
  if (taskId) return `/tasks/${taskId}`;
  if (view === "tasks") return "/";
  return `/${view}`;
}
