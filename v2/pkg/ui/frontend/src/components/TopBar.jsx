export default function TopBar({ config, onOpenSecrets, onOpenSettings, onOpenNewTask }) {
  const repoName = config ? (config.defaultTarget ? config.defaultTarget : `as ${config.actor}`) : "";
  return (
    <header className="topbar">
      <h1>grain</h1>
      <span className="repo-name">{repoName}</span>
      <div className="spacer" />
      <button className="secondary" onClick={onOpenSecrets}>Secrets</button>
      <button className="secondary" onClick={onOpenSettings}>Settings</button>
      <button className="primary" onClick={onOpenNewTask}>+ New task</button>
    </header>
  );
}
