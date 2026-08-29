import { STATE_LABELS, STATE_ORDER, reposFromTasks } from "../state.js";

// Sidebar replaces TopBar and Filters with the one nav Plane builds every
// view around: a fixed rail with the workspace identity up top, a state
// list styled like Plane's own status groups (a dot standing in for the
// state's badge color, a count on the right), and the deployment-level
// actions (secrets, settings) pinned to the bottom.
export default function Sidebar({ config, tasks, view, onSetView, stateFilter, onSetFilter, onOpenSecrets, onOpenSchedules, onOpenSettings, onOpenReleases, onOpenUpgrade, onOpenNewTask }) {
  const repoName = config ? (config.defaultTarget ? config.defaultTarget : `as ${config.actor}`) : "";

  const counts = {};
  let blocked = 0;
  for (const t of tasks) {
    counts[t.state] = (counts[t.state] || 0) + 1;
    if (t.blocked) blocked += 1;
  }
  const repoCount = reposFromTasks(tasks).length;

  // Picking a state filter always lands on the task list -- there is
  // nothing for it to scope on the repo page.
  const selectState = (id) => {
    onSetFilter(id);
    onSetView("tasks");
  };

  const NavItem = ({ id, label, dotClass, count }) => (
    <button className={view === "tasks" && stateFilter === id ? "active" : ""} onClick={() => selectState(id)}>
      <span className={`dot ${dotClass}`} />
      <span className="label">{label}</span>
      <span className="count">{count}</span>
    </button>
  );

  return (
    <aside className="sidebar">
      <div className="sidebar-brand">
        <span className="brand-mark" />
        <h1>grain</h1>
      </div>
      {repoName && <div className="sidebar-target" title={repoName}>{repoName}</div>}

      <button className="primary new-task" onClick={onOpenNewTask}>+ New task</button>

      <nav className="sidebar-nav">
        <button className={view === "repos" ? "active" : ""} onClick={() => onSetView("repos")}>
          <span className="dot dot-all" />
          <span className="label">Repos</span>
          <span className="count">{repoCount}</span>
        </button>
        <NavItem id="all" label="All issues" dotClass="dot-all" count={tasks.length} />
        {STATE_ORDER.filter((s) => counts[s]).map((s) => (
          <NavItem key={s} id={s} label={STATE_LABELS[s]} dotClass={`dot-${s}`} count={counts[s]} />
        ))}
        {/* Blocked is not a state (docs/data-model.md) so it gets its own
            nav entry alongside the state ones rather than a slot in
            STATE_ORDER -- a task stays under its own state filter too,
            this is just a faster way to find what dispatch is currently
            skipping over. */}
        {blocked > 0 && <NavItem id="blocked" label="Blocked" dotClass="dot-blocked" count={blocked} />}
      </nav>

      <div className="sidebar-footer">
        <button onClick={onOpenReleases}>Releases</button>
        <button onClick={onOpenSecrets}>Secrets</button>
        <button onClick={onOpenSchedules}>Scheduled tasks</button>
        <button onClick={onOpenSettings}>Settings</button>
        <button onClick={onOpenUpgrade}>Upgrade</button>
      </div>
    </aside>
  );
}
