import { useEffect, useState } from "react";
import { Alert, Button, Checkbox, FormControlLabel, Radio, RadioGroup, Stack, Tab, Tabs, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import SecretsPanel from "./SecretsPanel.jsx";
import UpgradePanel from "./UpgradePanel.jsx";
import { useThemeMode } from "../ThemeModeContext.jsx";

// bwsalmon/agents#456: Secrets and Upgrade used to be their own top-level
// sidebar overlays; they live here now as tabs alongside the general
// deployment settings, since all three are the same kind of
// operator-only, deployment-wide configuration.
const TABS = [
  { id: "general", label: "General" },
  { id: "secrets", label: "Secrets" },
  { id: "upgrade", label: "Upgrade" },
];

export default function SettingsOverlay({ config, onClose, showError }) {
  const [tab, setTab] = useState("general");
  const [settings, setSettings] = useState(null);
  const { mode: themeMode, setMode: setThemeMode } = useThemeMode();

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

    const maxConcurrentRaw = form.elements.maxConcurrent.value.trim();
    if (maxConcurrentRaw !== "") {
      const maxConcurrent = parseInt(maxConcurrentRaw, 10);
      if (maxConcurrent !== (settings.maxConcurrent || 0)) payload.maxConcurrent = maxConcurrent;
    }

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

    const newestFirst = form.elements.newestFirst.checked;
    if (newestFirst !== !!settings.newestFirst) payload.newestFirst = newestFirst;

    const sandboxCpusRaw = form.elements.sandboxCpus.value.trim();
    if (sandboxCpusRaw !== "") {
      const sandboxCpus = parseInt(sandboxCpusRaw, 10);
      if (sandboxCpus !== (settings.sandboxCpus || 0)) payload.sandboxCpus = sandboxCpus;
    }

    const sandboxMemoryMbRaw = form.elements.sandboxMemoryMb.value.trim();
    if (sandboxMemoryMbRaw !== "") {
      const sandboxMemoryMb = parseInt(sandboxMemoryMbRaw, 10);
      if (sandboxMemoryMb !== (settings.sandboxMemoryMb || 0)) payload.sandboxMemoryMb = sandboxMemoryMb;
    }

    const showClosedByDefault = form.elements.showClosedByDefault.checked;
    if (showClosedByDefault !== !!settings.showClosedByDefault) payload.showClosedByDefault = showClosedByDefault;

    const agentFramework = form.elements.agentFramework.value;
    if (agentFramework !== (settings.agentFramework || "gemini")) payload.agentFramework = agentFramework;

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
  //
  // That cut connection is exactly what makes this button look broken
  // (bwsalmon/agents#581): the reboot itself starts before the daemon's
  // 200 response finishes its round trip back through this deployment's
  // load balancer/proxy hops, so the browser's fetch commonly rejects
  // with its own network-level failure -- a TypeError, per the Fetch
  // spec -- even though the reboot is under way. api()'s own throw for
  // a real non-2xx response is always a plain Error carrying the
  // server's message (api.js), never a TypeError, so that's the signal
  // this can key on to tell "the machine is going down, as asked" apart
  // from an actual failure (disabled, or the sudo command itself
  // erroring) worth showing the operator.
  const rebootHost = async () => {
    if (!confirm("Reboot the host machine? Every task currently running is interrupted, and this UI will be unreachable until the machine comes back up.")) return;
    try {
      await api("/api/host/reboot", { method: "POST" });
    } catch (err) {
      if (!(err instanceof TypeError)) showError(err);
    }
  };

  if (settings === null) return null;

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Settings</Typography>
      <Tabs value={tab} onChange={(_, value) => setTab(value)} sx={{ mb: 2 }}>
        {TABS.map((t) => (
          <Tab key={t.id} value={t.id} label={t.label} />
        ))}
      </Tabs>
      {tab === "general" && (
        <>
          <Typography variant="subtitle2">Appearance</Typography>
          <RadioGroup
            row
            aria-label="Appearance"
            value={themeMode}
            onChange={(evt) => setThemeMode(evt.target.value)}
            sx={{ mb: 2 }}
          >
            <FormControlLabel value="auto" control={<Radio />} label="Auto" />
            <FormControlLabel value="light" control={<Radio />} label="Light" />
            <FormControlLabel value="dark" control={<Radio />} label="Dark" />
          </RadioGroup>
          {!settings.configured && (
            <Alert severity="info" sx={{ mb: 2 }}>
              Not configured yet -- nothing has been saved for this deployment. Poll interval, max concurrent, Gemini
              model and GitHub host are required the first time.
            </Alert>
          )}
          <form onSubmit={submit}>
            <TextField name="pollInterval" label="Poll interval" helperText="Go duration, e.g. 30s" defaultValue={settings.pollInterval || ""} autoComplete="off" fullWidth margin="normal" />
            <TextField name="maxConcurrent" label="Max concurrent agents" helperText="maximum number of tasks dispatched at once" type="number" inputProps={{ min: 1, step: 1 }} defaultValue={String(settings.maxConcurrent || "")} fullWidth margin="normal" />
            <Typography variant="subtitle2" sx={{ mt: 2 }}>Agent framework</Typography>
            <RadioGroup row aria-label="Agent framework" name="agentFramework" defaultValue={settings.agentFramework || "gemini"} sx={{ mb: 1 }}>
              <FormControlLabel value="gemini" control={<Radio />} label="Gemini" />
              <FormControlLabel value="claude" control={<Radio />} label="Claude" />
            </RadioGroup>
            <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
              Which agent.Framework a run is driven by. Claude support is not yet wired into dispatch -- selecting it
              here does not change what a run actually does.
            </Typography>
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
            <FormControlLabel
              control={<Checkbox name="newestFirst" defaultChecked={!!settings.newestFirst} />}
              label={(
                <>
                  Work through the backlog newest-first
                  <span className="hint">
                    off (default): a new task is added to the top of the list but dispatched last, behind
                    everything already queued. on: it is dispatched next instead, ahead of everything queued.
                  </span>
                </>
              )}
              sx={{ display: "flex", mt: 1 }}
            />
            <TextField
              name="sandboxCpus"
              label="Sandbox vCPUs"
              helperText="default vCPU count for a kontur-managed sandbox VM; 0 or blank leaves kontur's own default in place. Overridable per task."
              type="number"
              inputProps={{ min: 0, step: 1 }}
              defaultValue={String(settings.sandboxCpus || 0)}
              fullWidth
              margin="normal"
            />
            <TextField
              name="sandboxMemoryMb"
              label="Sandbox memory (MiB)"
              helperText="default guest memory, in MiB, for a kontur-managed sandbox VM; 0 or blank leaves kontur's own default in place. Overridable per task."
              type="number"
              inputProps={{ min: 0, step: 1 }}
              defaultValue={String(settings.sandboxMemoryMb || 0)}
              fullWidth
              margin="normal"
            />
            <FormControlLabel
              control={<Checkbox name="showClosedByDefault" defaultChecked={!!settings.showClosedByDefault} />}
              label={(
                <>
                  Show closed tasks by default
                  <span className="hint">
                    off (default): a task list's own "Show closed tasks" checkbox starts unchecked, hiding closed
                    tasks until turned on. on: it starts checked instead, showing them from the start.
                  </span>
                </>
              )}
              sx={{ display: "flex", mt: 1 }}
            />

            <Typography variant="body2" color="text.secondary" sx={{ mt: 2 }}>
              Target repos are managed from the Repos pane now, not here.
            </Typography>

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
        </>
      )}
      {tab === "secrets" && <SecretsPanel showError={showError} />}
      {tab === "upgrade" && <UpgradePanel showError={showError} />}
    </Overlay>
  );
}
