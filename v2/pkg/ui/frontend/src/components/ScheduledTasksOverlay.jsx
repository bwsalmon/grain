import { useCallback, useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// ScheduledTasksOverlay manages schedules (bwsalmon/agents#376): each row
// is a standing declaration -- "file this task every N" -- that graind's
// own schedule reconciler turns into a real task each time it comes due.
// SecretsOverlay's own shape fits here almost exactly: a list fetched on
// open, refreshed after every mutation, plus a form that only ever adds.
export default function ScheduledTasksOverlay({ onClose, showError }) {
  const [schedules, setSchedules] = useState(null);

  const refresh = useCallback(async () => {
    try {
      setSchedules(await api("/api/schedules"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const toggleEnabled = async (s) => {
    try {
      await api(`/api/schedules/${s.id}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !s.enabled }),
      });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async (s) => {
    if (!confirm(`Delete the schedule "${s.title}"? Tasks it already filed are not affected.`)) return;
    try {
      await api(`/api/schedules/${s.id}`, { method: "DELETE" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo"),
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      interval: data.get("interval"),
    };
    try {
      await api("/api/schedules", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  if (schedules === null) return null;

  return (
    <Overlay onClose={onClose}>
      <h2>Scheduled tasks</h2>
      <ul className="schedules-list">
        {schedules.map((s) => (
          <li className="schedule-row" key={s.id}>
            <div className="schedule-summary">
              <span className="schedule-title">{s.title}</span>
              <span className="chip">{s.repo}</span>
              <span className="chip">every {s.interval}</span>
              {!s.enabled && <span className="badge badge-blocked">Paused</span>}
            </div>
            <div className="schedule-meta hint">
              Next run {formatWhen(s.nextRunAt)}
              {s.lastRunAt && <> · last ran {formatWhen(s.lastRunAt)}</>}
            </div>
            <div className="form-actions">
              <button type="button" className="secondary" onClick={() => toggleEnabled(s)}>
                {s.enabled ? "Pause" : "Resume"}
              </button>
              <button type="button" className="danger secondary" onClick={() => remove(s)}>
                Delete
              </button>
            </div>
          </li>
        ))}
      </ul>
      {schedules.length === 0 && <p className="empty">No scheduled tasks.</p>}
      <form onSubmit={submit}>
        <label>Title
          <input name="title" required autoComplete="off" />
        </label>
        <label>Description
          <textarea name="description" rows="4" />
        </label>
        <label>Target repo <span className="hint">owner/name</span>
          <input name="repo" placeholder="owner/name" required autoComplete="off" />
        </label>
        <label>Base branch <span className="hint">optional</span>
          <input name="base" placeholder="main" autoComplete="off" />
        </label>
        <label>Interval <span className="hint">Go duration, e.g. 24h</span>
          <input name="interval" placeholder="24h" required autoComplete="off" />
        </label>
        <label className="checkbox">
          <input type="checkbox" name="autoMerge" />
          Auto-merge once checks pass
        </label>
        <div className="form-actions">
          <button type="submit" className="primary">Add schedule</button>
        </div>
      </form>
    </Overlay>
  );
}

function formatWhen(iso) {
  return new Date(iso).toLocaleString();
}
