import { STATE_LABELS, STATE_ORDER } from "../state.js";

export default function Filters({ tasks, stateFilter, onSetFilter }) {
  const counts = {};
  let blocked = 0;
  for (const t of tasks) {
    counts[t.state] = (counts[t.state] || 0) + 1;
    if (t.blocked) blocked += 1;
  }

  const Button = ({ id, label }) => (
    <button className={stateFilter === id ? "active" : ""} onClick={() => onSetFilter(id)}>
      {label}
    </button>
  );

  return (
    <nav className="filters">
      <Button id="all" label={`All (${tasks.length})`} />
      {STATE_ORDER.filter((s) => counts[s]).map((s) => (
        <Button key={s} id={s} label={`${STATE_LABELS[s]} (${counts[s]})`} />
      ))}
      {/* Blocked is not a state (docs/data-model.md) so it gets its own
          filter alongside the state ones rather than a slot in
          STATE_ORDER -- a task stays under its own state filter too,
          this is just a faster way to find what dispatch is currently
          skipping over. */}
      {blocked > 0 && <Button id="blocked" label={`Blocked (${blocked})`} />}
    </nav>
  );
}
