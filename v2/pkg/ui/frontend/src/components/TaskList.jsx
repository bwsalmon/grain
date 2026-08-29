import { STATE_LABELS, capabilityName } from "../state.js";

export default function TaskList({ tasks, stateFilter, config, onOpenTask }) {
  const visible = stateFilter === "all" ? tasks
    : stateFilter === "blocked" ? tasks.filter((t) => t.blocked)
    : tasks.filter((t) => t.state === stateFilter);

  return (
    <main>
      <ul className="task-list">
        {visible.map((t) => (
          <li key={t.id} onClick={() => onOpenTask(t.id)}>
            <span className="task-number">{t.id}</span>
            <span className="task-title">{t.title}</span>
            <span className="chips">
              {t.repo && <span className="chip">{t.repo}</span>}
              {(t.reads || []).map((repo) => (
                <span key={repo} className="chip chip-read" title="read-only">{repo} (read)</span>
              ))}
              {t.capabilities.map((id) => (
                <span key={id} className="chip">{capabilityName(config, id)}</span>
              ))}
            </span>
            <span className={`badge badge-${t.state}`}>{STATE_LABELS[t.state] || t.state}</span>
            {t.blocked && (
              <span className="badge badge-blocked" title={`Waiting on ${t.blockedBy.join(", ")}`}>Blocked</span>
            )}
          </li>
        ))}
      </ul>
      {visible.length === 0 && <p className="empty">No tasks in this state.</p>}
    </main>
  );
}
