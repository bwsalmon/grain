import { useEffect, useState } from "react";
import { Alert, Box, Button, Checkbox, Chip, FormControlLabel, Stack, Tab, Tabs, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import SecretsPanel from "./SecretsPanel.jsx";
import UpgradePanel from "./UpgradePanel.jsx";

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
  const [targetRepos, setTargetRepos] = useState([]);
  const [newRepo, setNewRepo] = useState("");

  useEffect(() => {
    (async () => {
      try {
        const s = await api("/api/settings");
        setSettings(s);
        setTargetRepos(s.targetRepos || []);
      } catch (err) {
        showError(err);
      }
    })();
  }, [showError]);

  // addTargetRepo/removeTargetRepo only touch local state -- like every
  // other field here, the list is only sent to the server when Save is
  // pressed, so adding or removing an entry can be undone by closing the
  // overlay without saving.
  const addTargetRepo = () => {
    const repo = newRepo.trim();
    if (repo === "" || targetRepos.includes(repo)) {
      setNewRepo("");
      return;
    }
    setTargetRepos([...targetRepos, repo]);
    setNewRepo("");
  };

  const removeTargetRepo = (repo) => {
    setTargetRepos(targetRepos.filter((r) => r !== repo));
  };

  const newRepoKeyDown = (evt) => {
    if (evt.key !== "Enter") return;
    evt.preventDefault();
    addTargetRepo();
  };

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
      <Tabs value={tab} onChange={(_, value) => setTab(value)} sx={{ mb: 2 }}>
        {TABS.map((t) => (
          <Tab key={t.id} value={t.id} label={t.label} />
        ))}
      </Tabs>
      {tab === "general" && (
        <>
          {!settings.configured && (
            <Alert severity="info" sx={{ mb: 2 }}>
              Not configured yet -- nothing has been saved for this deployment. Poll interval, max concurrent, Gemini
              model and GitHub host are required the first time.
            </Alert>
          )}
          <form onSubmit={submit}>
            <TextField name="pollInterval" label="Poll interval" helperText="Go duration, e.g. 30s" defaultValue={settings.pollInterval || ""} autoComplete="off" fullWidth margin="normal" />
            <TextField name="maxConcurrent" label="Max concurrent agents" helperText="maximum number of tasks dispatched at once" type="number" inputProps={{ min: 1, step: 1 }} defaultValue={String(settings.maxConcurrent || "")} fullWidth margin="normal" />
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

            <Typography variant="body2" sx={{ mt: 2, fontWeight: 500 }}>
              Target repos <span className="hint">owner/name; empty allows any</span>
            </Typography>
            <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.6, mt: 1 }}>
              {targetRepos.map((repo) => (
                <Chip
                  key={repo}
                  size="small"
                  label={repo}
                  onDelete={() => removeTargetRepo(repo)}
                  deleteIcon={<span title={`remove ${repo}`}>×</span>}
                />
              ))}
            </Box>
            {targetRepos.length === 0 && (
              <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>No repos added -- any repo is allowed.</Typography>
            )}
            <Stack direction="row" spacing={1} sx={{ mt: 1 }}>
              <TextField
                name="newTargetRepo"
                value={newRepo}
                onChange={(evt) => setNewRepo(evt.target.value)}
                onKeyDown={newRepoKeyDown}
                placeholder="owner/repo"
                autoComplete="off"
                size="small"
                fullWidth
              />
              <Button variant="outlined" onClick={addTargetRepo}>Add</Button>
            </Stack>

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
