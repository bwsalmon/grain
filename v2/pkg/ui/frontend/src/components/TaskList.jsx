import { STATE_LABELS, capabilityName } from "../state.js";

const FILTER_TITLES = { all: "All issues", blocked: "Blocked" };

// A stacked task -- the merge queue's own automatic fix for another
// task's pull request (bwsalmon/agents#378) -- is not new work of its
// own, so it is nested under the task named by generatedFrom instead of
// listed as a separate row, as long as that task also passes the
// current filter. One that doesn't (its parent fell out of the filtered
// view, or the parent it names is gone) falls back to a plain row of
// its own, so it is never silently dropped from the list.
function groupByStack(tasks, matches) {
  const topLevel = [];
  const orphans = [];
  for (const t of tasks) {
    if (!matches(t)) continue;
    if (t.stacked && t.generatedFrom) orphans.push(t);
    else topLevel.push(t);
  }
  const topLevelIds = new Set(topLevel.map((t) => t.id));
  const children = new Map();
  for (const c of orphans) {
    if (!topLevelIds.has(c.generatedFrom)) {
      topLevel.push(c);
      continue;
    }
    const list = children.get(c.generatedFrom) || [];
    list.push(c);
    children.set(c.generatedFrom, list);
  }
  return { topLevel, children };
}

export default function TaskList({ tasks, stateFilter, config, onOpenTask, selected, onToggleSelect, onSelectAll }) {
  const matches = (t) => stateFilter === "all" ? true
    : stateFilter === "blocked" ? t.blocked
    : t.state === stateFilter;

  const { topLevel, children } = groupByStack(tasks, matches);
  const visibleIds = topLevel.flatMap((t) => [t.id, ...(children.get(t.id) || []).map((c) => c.id)]);
  const allSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));

  const title = FILTER_TITLES[stateFilter] || STATE_LABELS[stateFilter] || stateFilter;

  return (
    <main>
      <div className="content-header">
        <h2>{title}</h2>
        <span className="count">{visibleIds.length}</span>
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
        {topLevel.map((t) => (
          <li key={t.id}>
            <TaskRow t={t} config={config} onOpenTask={onOpenTask} selected={selected} onToggleSelect={onToggleSelect} />
            {children.has(t.id) && (
              <ul className="task-sublist">
                {children.get(t.id).map((c) => (
                  <li key={c.id}>
                    <TaskRow t={c} config={config} onOpenTask={onOpenTask} selected={selected} onToggleSelect={onToggleSelect} />
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
      {topLevel.length === 0 && <p className="empty">No tasks in this state.</p>}
    </main>
  );
}

function TaskRow({ t, config, onOpenTask, selected, onToggleSelect }) {
  return (
    <div className="task-row" onClick={() => onOpenTask(t.id)}>
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
        {t.scheduled && <span className="chip chip-scheduled" title="filed automatically by a schedule">scheduled</span>}
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
    </div>
  );
}
