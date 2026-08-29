export default function TopBar({ config, view, onSetView, onOpenSecrets, onOpenSettings, onOpenNewTask }) {
  const repoName = config ? (config.defaultTarget ? config.defaultTarget : `as ${config.actor}`) : "";
  return (
    <header className="topbar">
      <h1>grain</h1>
      <span className="repo-name">{repoName}</span>
      <nav className="view-tabs">
        <button className={view === "tasks" ? "active" : ""} onClick={() => onSetView("tasks")}>Tasks</button>
        <button className={view === "repos" ? "active" : ""} onClick={() => onSetView("repos")}>Repos</button>
      </nav>
      <div className="spacer" />
      <button className="secondary" onClick={onOpenSecrets}>Secrets</button>
      <button className="secondary" onClick={onOpenSettings}>Settings</button>
      <button className="primary" onClick={onOpenNewTask}>+ New task</button>
    </header>
  );
}
