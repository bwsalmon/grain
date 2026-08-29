import { useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

function parseSlots(value) {
  return value.split(",").map((slot) => slot.trim()).filter((slot) => slot !== "");
}

export default function SettingsOverlay({ config, onClose, showError }) {
  const [settings, setSettings] = useState(null);

  useEffect(() => {
    (async () => {
      try {
        setSettings(await api("/api/settings"));
      } catch (err) {
        showError(err);
      }
    })();
  }, [showError]);

  // submitSettings only puts a field in the request when it differs from
  // what was last loaded, so an operator changing one field never
  // overwrites the rest -- the same nil-means-unchanged contract
  // UpdateSettingsRequest's pointer fields already give a PUT that
  // leaves a key out entirely.
  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {};

    const pollInterval = form.elements.pollInterval.value.trim();
    if (pollInterval !== (settings.pollInterval || "")) payload.pollInterval = pollInterval;

    const slots = parseSlots(form.elements.slots.value);
    if (JSON.stringify(slots) !== JSON.stringify(settings.slots || [])) payload.slots = slots;

    const geminiModel = form.elements.geminiModel.value.trim();
    if (geminiModel !== (settings.geminiModel || "")) payload.geminiModel = geminiModel;

    const maxAgentTurnsRaw = form.elements.maxAgentTurns.value.trim();
    if (maxAgentTurnsRaw !== "") {
      const maxAgentTurns = parseInt(maxAgentTurnsRaw, 10);
      if (maxAgentTurns !== (settings.maxAgentTurns || 0)) payload.maxAgentTurns = maxAgentTurns;
    }

    const githubHost = form.elements.githubHost.value.trim();
    if (githubHost !== (settings.githubHost || "")) payload.githubHost = githubHost;

    const githubInsecureHttp = form.elements.githubInsecureHttp.checked;
    if (githubInsecureHttp !== !!settings.githubInsecureHttp) payload.githubInsecureHttp = githubInsecureHttp;

    const gcpProject = form.elements.gcpProject.value.trim();
    if (gcpProject !== (settings.gcpProject || "")) payload.gcpProject = gcpProject;

    const gcpServiceAccountEmail = form.elements.gcpServiceAccountEmail.value.trim();
    if (gcpServiceAccountEmail !== (settings.gcpServiceAccountEmail || "")) payload.gcpServiceAccountEmail = gcpServiceAccountEmail;

    try {
      await api("/api/settings", { method: "PUT", body: JSON.stringify(payload) });
      onClose();
    } catch (err) {
      // Same banner task creation's own validation errors surface
      // through.
      showError(err);
    }
  };

  // rebootHost is deliberately its own confirm/try, separate from
  // submit's settings form: it is not a settings field, and unlike a
  // failed settings save there is no "current" state to fall back on
  // showing afterward -- a successful call cuts this same connection
  // along with everything else on the machine.
  const rebootHost = async () => {
    if (!confirm("Reboot the host machine? Every task currently running is interrupted, and this UI will be unreachable until the machine comes back up.")) return;
    try {
      await api("/api/host/reboot", { method: "POST" });
    } catch (err) {
      showError(err);
    }
  };

  if (settings === null) return null;

  return (
    <Overlay onClose={onClose}>
      <h2>Settings</h2>
      {!settings.configured && (
        <p className="unconfigured-note">
          Not configured yet -- nothing has been saved for this deployment. Poll interval, slots, Gemini model
          and GitHub host are required the first time.
        </p>
      )}
      <form onSubmit={submit}>
        <label>Poll interval <span className="hint">Go duration, e.g. 30s</span>
          <input name="pollInterval" defaultValue={settings.pollInterval || ""} autoComplete="off" />
        </label>
        <label>Slots <span className="hint">comma-separated slot names</span>
          <input name="slots" defaultValue={(settings.slots || []).join(", ")} placeholder="a, b, c" autoComplete="off" />
        </label>
        <label>Gemini model
          <input name="geminiModel" defaultValue={settings.geminiModel || ""} autoComplete="off" />
        </label>
        <label>Max agent turns <span className="hint">0 = the agent framework's own default</span>
          <input name="maxAgentTurns" type="number" min="0" step="1" defaultValue={String(settings.maxAgentTurns || 0)} />
        </label>
        <label>GitHub host
          <input name="githubHost" defaultValue={settings.githubHost || ""} autoComplete="off" />
        </label>
        <label className="checkbox">
          <input type="checkbox" name="githubInsecureHttp" defaultChecked={!!settings.githubInsecureHttp} />
          Speak plain HTTP to GitHub host <span className="hint">mock servers only</span>
        </label>
        <label>GCP project <span className="hint">optional -- enables the gcp-key/gemini-key capabilities</span>
          <input name="gcpProject" defaultValue={settings.gcpProject || ""} autoComplete="off" />
        </label>
        <label>GCP service account email <span className="hint">optional</span>
          <input name="gcpServiceAccountEmail" defaultValue={settings.gcpServiceAccountEmail || ""} autoComplete="off" />
        </label>
        <div className="form-actions">
          <button type="submit" className="primary">Save</button>
        </div>
      </form>
      {config && config.rebootEnabled && (
        <fieldset>
          <legend>Danger zone</legend>
          <p className="hint">Reboots the machine grain itself is running on.</p>
          <button type="button" className="danger secondary" onClick={rebootHost}>Reboot host</button>
        </fieldset>
      )}
    </Overlay>
  );
}
