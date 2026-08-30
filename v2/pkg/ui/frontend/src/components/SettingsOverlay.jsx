import { useEffect, useState } from "react";
import { Alert, Button, Checkbox, FormControlLabel, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

function parseCommaList(value) {
  return value.split(",").map((item) => item.trim()).filter((item) => item !== "");
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

    const slots = parseCommaList(form.elements.slots.value);
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

    const targetRepos = parseCommaList(form.elements.targetRepos.value);
    if (JSON.stringify(targetRepos) !== JSON.stringify(settings.targetRepos || [])) payload.targetRepos = targetRepos;

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
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Settings</Typography>
      {!settings.configured && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not configured yet -- nothing has been saved for this deployment. Poll interval, slots, Gemini model
          and GitHub host are required the first time.
        </Alert>
      )}
      <form onSubmit={submit}>
        <TextField name="pollInterval" label="Poll interval" helperText="Go duration, e.g. 30s" defaultValue={settings.pollInterval || ""} autoComplete="off" fullWidth margin="normal" />
        <TextField name="slots" label="Slots" helperText="comma-separated slot names" defaultValue={(settings.slots || []).join(", ")} placeholder="a, b, c" autoComplete="off" fullWidth margin="normal" />
        <TextField name="geminiModel" label="Gemini model" defaultValue={settings.geminiModel || ""} autoComplete="off" fullWidth margin="normal" />
        <TextField name="maxAgentTurns" label="Max agent turns" helperText="0 = the agent framework's own default" type="number" inputProps={{ min: 0, step: 1 }} defaultValue={String(settings.maxAgentTurns || 0)} fullWidth margin="normal" />
        <TextField name="githubHost" label="GitHub host" defaultValue={settings.githubHost || ""} autoComplete="off" fullWidth margin="normal" />
        <FormControlLabel
          control={<Checkbox name="githubInsecureHttp" defaultChecked={!!settings.githubInsecureHttp} />}
          label={<>Speak plain HTTP to GitHub host <span className="hint">mock servers only</span></>}
          sx={{ display: "flex", mt: 1 }}
        />
        <TextField name="gcpProject" label="GCP project" helperText="optional -- enables the gcp-key/gemini-key capabilities" defaultValue={settings.gcpProject || ""} autoComplete="off" fullWidth margin="normal" />
        <TextField name="gcpServiceAccountEmail" label="GCP service account email" helperText="optional" defaultValue={settings.gcpServiceAccountEmail || ""} autoComplete="off" fullWidth margin="normal" />
        <TextField name="targetRepos" label="Target repos" helperText="comma-separated owner/name; empty allows any" defaultValue={(settings.targetRepos || []).join(", ")} placeholder="owner/repo, owner/other" autoComplete="off" fullWidth margin="normal" />
        <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
          <Button type="submit" variant="contained">Save</Button>
        </Stack>
      </form>
      {config && config.rebootEnabled && (
        <fieldset>
          <legend>Danger zone</legend>
          <p className="hint">Reboots the machine grain itself is running on.</p>
          <Button variant="outlined" color="error" onClick={rebootHost}>Reboot host</Button>
        </fieldset>
      )}
    </Overlay>
  );
}
