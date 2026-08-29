import { STATE_LABELS, capabilityName } from "../state.js";

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

export default function TaskList({ tasks, stateFilter, config, onOpenTask }) {
  const matches = (t) => stateFilter === "all" ? true
    : stateFilter === "blocked" ? t.blocked
    : t.state === stateFilter;

  const { topLevel, children } = groupByStack(tasks, matches);

  return (
    <main>
      <ul className="task-list">
        {topLevel.map((t) => (
          <li key={t.id}>
            <TaskRow t={t} config={config} onOpenTask={onOpenTask} />
            {children.has(t.id) && (
              <ul className="task-sublist">
                {children.get(t.id).map((c) => (
                  <li key={c.id}>
                    <TaskRow t={c} config={config} onOpenTask={onOpenTask} />
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

function TaskRow({ t, config, onOpenTask }) {
  return (
    <div className="task-row" onClick={() => onOpenTask(t.id)}>
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
    </div>
  );
}
