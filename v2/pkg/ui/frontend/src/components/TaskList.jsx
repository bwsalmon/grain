import { STATE_LABELS, capabilityName } from "../state.js";

const FILTER_TITLES = { all: "All issues", blocked: "Blocked" };

export default function TaskList({ tasks, stateFilter, config, onOpenTask, selected, onToggleSelect, onSelectAll }) {
  const visible = stateFilter === "all" ? tasks
    : stateFilter === "blocked" ? tasks.filter((t) => t.blocked)
    : tasks.filter((t) => t.state === stateFilter);
  const visibleIds = visible.map((t) => t.id);
  const allSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));

  const title = FILTER_TITLES[stateFilter] || STATE_LABELS[stateFilter] || stateFilter;

  return (
    <main>
      <div className="content-header">
        <h2>{title}</h2>
        <span className="count">{visible.length}</span>
      </div>
      {visibleIds.length > 0 && (
        <label className="select-all">
          <input
            type="checkbox"
            checked={allSelected}
            onChange={(e) => onSelectAll(visibleIds, e.target.checked)}
          />
          Select all
        </label>
      )}
      <ul className="task-list">
        {visible.map((t) => (
          <li key={t.id} onClick={() => onOpenTask(t.id)}>
            <input
              type="checkbox"
              className="task-select"
              checked={selected.has(t.id)}
              onClick={(e) => e.stopPropagation()}
              onChange={() => onToggleSelect(t.id)}
            />
            <span className={`badge badge-${t.state}`} title={STATE_LABELS[t.state] || t.state} />
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
