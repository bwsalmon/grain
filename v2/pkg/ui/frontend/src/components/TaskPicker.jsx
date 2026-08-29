import { useEffect, useRef, useState } from "react";

// TaskPicker is a search box that resolves free text to a task: type to
// filter by id or title, click (or arrow down + Enter) to pick one. It
// replaces the raw "type a task id and hope you copied it right" inputs
// that used to sit wherever a task needed to reference another one.
//
// It holds no selection state of its own -- onPick fires once per pick
// and the field clears, so a caller wanting a persistent choice (a chip,
// a form field) keeps that in its own state and renders it alongside.
// That keeps a single-pick "attach immediately" use (DetailOverlay's
// dependency add) and a multi-pick "build a list, submit once" use
// (NewTaskOverlay's dependsOn) working off the same component.
export default function TaskPicker({ tasks, exclude = [], onPick, placeholder = "Search tasks…", autoFocus = false }) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const containerRef = useRef(null);

  const excludeSet = new Set(exclude);
  const q = query.trim().toLowerCase();
  const matches = q === "" ? [] : tasks
    .filter((t) => !excludeSet.has(t.id))
    .filter((t) => t.id.toLowerCase().includes(q) || t.title.toLowerCase().includes(q))
    .slice(0, 8);

  useEffect(() => {
    function onDocClick(e) {
      if (containerRef.current && !containerRef.current.contains(e.target)) setOpen(false);
    }
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, []);

  useEffect(() => { setHighlight(0); }, [query]);

  const pick = (t) => {
    onPick(t);
    setQuery("");
    setOpen(false);
  };

  const onKeyDown = (e) => {
    if (e.key === "Escape") { setOpen(false); return; }
    if (matches.length === 0) return;
    if (e.key === "ArrowDown") { e.preventDefault(); setHighlight((h) => Math.min(h + 1, matches.length - 1)); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setHighlight((h) => Math.max(h - 1, 0)); }
    else if (e.key === "Enter") { e.preventDefault(); pick(matches[highlight]); }
  };

  return (
    <div className="task-picker" ref={containerRef}>
      <input
        type="text"
        placeholder={placeholder}
        value={query}
        autoFocus={autoFocus}
        autoComplete="off"
        onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
        onFocus={() => setOpen(true)}
        onKeyDown={onKeyDown}
      />
      {open && q !== "" && (
        <ul className="task-picker-results">
          {matches.length === 0 && <li className="task-picker-empty">No matching tasks</li>}
          {matches.map((t, i) => (
            <li
              key={t.id}
              className={i === highlight ? "active" : ""}
              onClick={() => pick(t)}
              onMouseEnter={() => setHighlight(i)}
            >
              <span className="task-picker-id">{t.id}</span>
              <span className="task-picker-title">{t.title}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
