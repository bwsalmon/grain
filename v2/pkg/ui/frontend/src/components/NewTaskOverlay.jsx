import { useRef, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import TaskPicker from "./TaskPicker.jsx";

export default function NewTaskOverlay({ tasks, config, onClose, onCreated, showError }) {
  const formRef = useRef(null);
  // dependsOn is picked tasks ({id, title}), not just ids -- keeping the
  // title lets the chips below the picker read as "task 12 Fix the
  // thing" instead of a bare number nobody can place.
  const [dependsOn, setDependsOn] = useState([]);

  const addDependency = (t) => {
    setDependsOn((prev) => (prev.some((p) => p.id === t.id) ? prev : [...prev, t]));
  };
  const removeDependency = (id) => {
    setDependsOn((prev) => prev.filter((p) => p.id !== id));
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const capabilities = (config?.capabilities || [])
      .filter((c) => form.elements["cap-" + c.id] && form.elements["cap-" + c.id].checked)
      .map((c) => c.id);
    const reads = (data.get("reads") || "")
      .split(",").map((repo) => repo.trim()).filter((repo) => repo !== "");
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo") || "",
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      capabilities,
      dependsOn: dependsOn.map((t) => t.id),
      reads,
      approved: form.elements.approved.checked,
    };
    try {
      await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      setDependsOn([]);
      onClose();
      await onCreated();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <h2>New task</h2>
      <form ref={formRef} onSubmit={submit}>
        <label>Title
          <input name="title" required autoComplete="off" />
        </label>
        <label>Description
          <textarea name="description" rows="5" />
        </label>
        <label>Target repo <span className="hint">owner/name, optional</span>
          <input name="repo" placeholder="owner/name" autoComplete="off" />
        </label>
        <label>Base branch <span className="hint">optional</span>
          <input name="base" placeholder="main" autoComplete="off" />
        </label>
        <label>Read-only repos <span className="hint">owner/name, comma-separated, optional</span>
          <input name="reads" placeholder="owner/shared-lib, owner/schema" autoComplete="off" />
        </label>
        <label className="checkbox">
          <input type="checkbox" name="autoMerge" />
          Auto-merge once checks pass
        </label>
        <fieldset>
          <legend>Capabilities</legend>
          {(config?.capabilities || []).map((c) => (
            <label key={c.id} className="checkbox" title={c.description}>
              <input type="checkbox" name={"cap-" + c.id} />
              {c.name}
            </label>
          ))}
        </fieldset>
        <label>Depends on <span className="hint">optional</span>
          {dependsOn.length > 0 && (
            <div className="chips dependency-chips">
              {dependsOn.map((t) => (
                <span key={t.id} className="chip dependency-chip">
                  <span>{t.id} {t.title}</span>
                  <button
                    type="button"
                    className="chip-remove"
                    title={`Remove dependency on ${t.id}`}
                    onClick={(e) => { e.stopPropagation(); removeDependency(t.id); }}
                  >
                    ×
                  </button>
                </span>
              ))}
            </div>
          )}
          <TaskPicker
            tasks={tasks || []}
            exclude={dependsOn.map((t) => t.id)}
            onPick={addDependency}
            placeholder="Search tasks to depend on…"
          />
        </label>
        <label className="checkbox">
          <input type="checkbox" name="approved" />
          Queue immediately (unchecked files it as a proposal, needing approval)
        </label>
        <div className="form-actions">
          <button type="submit" className="primary">Create task</button>
        </div>
      </form>
    </Overlay>
  );
}
