import { useRef } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

export default function NewTaskOverlay({ config, onClose, onCreated, showError }) {
  const formRef = useRef(null);

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const capabilities = (config?.capabilities || [])
      .filter((c) => form.elements["cap-" + c.id] && form.elements["cap-" + c.id].checked)
      .map((c) => c.id);
    const dependsOn = (data.get("dependsOn") || "")
      .split(",").map((id) => id.trim()).filter((id) => id !== "");
    const reads = (data.get("reads") || "")
      .split(",").map((repo) => repo.trim()).filter((repo) => repo !== "");
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo") || "",
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      capabilities,
      dependsOn,
      reads,
      approved: form.elements.approved.checked,
    };
    try {
      await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
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
        <label>Depends on <span className="hint">task ids, comma-separated, optional</span>
          <input name="dependsOn" placeholder="12, 15" autoComplete="off" />
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
