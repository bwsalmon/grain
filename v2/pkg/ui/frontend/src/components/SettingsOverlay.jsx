import { useEffect, useState } from "react";
import { Alert, Button, Checkbox, FormControlLabel, Radio, RadioGroup, Stack, Tab, Tabs, TextField, Typography } from "@mui/material";
import api from "../api.js";
import AgentKeysSection from "./AgentKeysSection.jsx";
import CapabilitiesPanel from "./CapabilitiesPanel.jsx";
import Overlay from "./Overlay.jsx";
import SecretsPanel from "./SecretsPanel.jsx";
import UpgradePanel from "./UpgradePanel.jsx";
import { useThemeMode } from "../ThemeModeContext.jsx";

// bwsalmon/agents#456: Secrets and Upgrade used to be their own top-level
// sidebar overlays; they live here now as tabs alongside the rest of
// deployment settings, since all of it is the same kind of operator-only,
// deployment-wide configuration.
//
// Logs, Sandbox health and the reboot control used to join them here as
// a single Debug tab (bwsalmon/agents#623), but moved back out to their
// own sidebar entry/overlay, DebugOverlay.jsx (bwsalmon/agents#640): live
// diagnostics for a deployment gone wrong turned out to want quicker
// reach than a tab buried inside Settings, unlike the configuration the
// tabs below actually are.
//
// The catch-all "General" tab this pane started with grew past the point
// a single scroll of unrelated fields was still readable, so it is split
// by subject here instead: Agents, GitHub and Sandbox each get their own
// tab, and GCP project/service account -- config that exists only to
// satisfy the gcp-key/gemini-key capabilities -- moved into Capabilities,
// next to the read-only view of whether that config is actually enough.
// Each tab is its own <form>, submitting only the fields it owns, so
// saving one never has to know or care about the others' values.
const TABS = [
  { id: "general", label: "General" },
  { id: "agents", label: "Agents" },
  { id: "github", label: "GitHub" },
  { id: "sandbox", label: "Sandbox" },
  { id: "capabilities", label: "Capabilities" },
  { id: "secrets", label: "Secrets" },
  { id: "upgrade", label: "Upgrade" },
];

export default function SettingsOverlay({ onClose, showError }) {
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

  // save only puts a field in the request when it differs from what was
  // last loaded, so an operator changing one field on one tab never
  // overwrites a field that lives on another -- the same nil-means-
  // unchanged contract UpdateSettingsRequest's pointer fields already
  // give a PUT that leaves a key out entirely.
  const save = async (payload) => {
    try {
      await api("/api/settings", { method: "PUT", body: JSON.stringify(payload) });
      onClose();
    } catch (err) {
      // Same banner task creation's own validation errors surface
      // through.
      showError(err);
    }
  };

  const submitGeneral = (evt) => {
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

    const newestFirst = form.elements.newestFirst.checked;
    if (newestFirst !== !!settings.newestFirst) payload.newestFirst = newestFirst;

    const showClosedByDefault = form.elements.showClosedByDefault.checked;
    if (showClosedByDefault !== !!settings.showClosedByDefault) payload.showClosedByDefault = showClosedByDefault;

    const approvedByDefault = form.elements.approvedByDefault.checked;
    if (approvedByDefault !== !!settings.approvedByDefault) payload.approvedByDefault = approvedByDefault;

    const autoMergeByDefault = form.elements.autoMergeByDefault.checked;
    if (autoMergeByDefault !== !!settings.autoMergeByDefault) payload.autoMergeByDefault = autoMergeByDefault;

    return save(payload);
  };

  const submitAgents = (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {};

    const agentFramework = form.elements.agentFramework.value;
    if (agentFramework !== (settings.agentFramework || "antigravity")) payload.agentFramework = agentFramework;

    const geminiModel = form.elements.geminiModel.value.trim();
    if (geminiModel !== (settings.geminiModel || "")) payload.geminiModel = geminiModel;

    const maxAgentTurnsRaw = form.elements.maxAgentTurns.value.trim();
    if (maxAgentTurnsRaw !== "") {
      const maxAgentTurns = parseInt(maxAgentTurnsRaw, 10);
      if (maxAgentTurns !== (settings.maxAgentTurns || 0)) payload.maxAgentTurns = maxAgentTurns;
    }

    return save(payload);
  };

  const submitGithub = (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {};

    const githubHost = form.elements.githubHost.value.trim();
    if (githubHost !== (settings.githubHost || "")) payload.githubHost = githubHost;

    const githubInsecureHttp = form.elements.githubInsecureHttp.checked;
    if (githubInsecureHttp !== !!settings.githubInsecureHttp) payload.githubInsecureHttp = githubInsecureHttp;

    return save(payload);
  };

  const submitSandbox = (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {};

    // An empty box is a deliberate "go back to the default" (bwsalmon/agents#610),
    // not "leave it alone" -- unlike every other field on this pane, these two
    // pre-fill that default in faintly, so an operator who never touched them
    // and one who cleared them back to blank on purpose type the same thing here.
    const sandboxCpusRaw = form.elements.sandboxCpus.value.trim();
    const sandboxCpus = sandboxCpusRaw === "" ? 0 : parseInt(sandboxCpusRaw, 10);
    if (sandboxCpus !== (settings.sandboxCpus || 0)) payload.sandboxCpus = sandboxCpus;

    const sandboxMemoryMbRaw = form.elements.sandboxMemoryMb.value.trim();
    const sandboxMemoryMb = sandboxMemoryMbRaw === "" ? 0 : parseInt(sandboxMemoryMbRaw, 10);
    if (sandboxMemoryMb !== (settings.sandboxMemoryMb || 0)) payload.sandboxMemoryMb = sandboxMemoryMb;

    return save(payload);
  };

  const submitCapabilities = (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {};

    const gcpProject = form.elements.gcpProject.value.trim();
    if (gcpProject !== (settings.gcpProject || "")) payload.gcpProject = gcpProject;

    const gcpServiceAccountEmail = form.elements.gcpServiceAccountEmail.value.trim();
    if (gcpServiceAccountEmail !== (settings.gcpServiceAccountEmail || "")) payload.gcpServiceAccountEmail = gcpServiceAccountEmail;

    return save(payload);
  };

  if (settings === null) return null;

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Settings</Typography>
      {!settings.configured && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not configured yet -- nothing has been saved for this deployment. Poll interval and max concurrent
          (General), Gemini model (Agents) and GitHub host (GitHub) are required the first time.
        </Alert>
      )}
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
          <form onSubmit={submitGeneral}>
            <TextField name="pollInterval" label="Poll interval" helperText="Go duration, e.g. 30s" defaultValue={settings.pollInterval || ""} autoComplete="off" fullWidth margin="normal" />
            <TextField name="maxConcurrent" label="Max concurrent agents" helperText="maximum number of tasks dispatched at once" type="number" inputProps={{ min: 1, step: 1 }} defaultValue={String(settings.maxConcurrent || "")} fullWidth margin="normal" />
            <Typography variant="subtitle2" sx={{ mt: 2 }}>Backlog &amp; task defaults</Typography>
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
            <FormControlLabel
              control={<Checkbox name="approvedByDefault" defaultChecked={!!settings.approvedByDefault} />}
              label={(
                <>
                  Queue new tasks immediately by default
                  <span className="hint">
                    off (default): a new task's own "Queue immediately" checkbox starts unchecked, filing it as a
                    proposal needing approval. on: it starts checked instead, filing a task ready to dispatch at
                    once.
                  </span>
                </>
              )}
              sx={{ display: "flex", mt: 1 }}
            />
            <FormControlLabel
              control={<Checkbox name="autoMergeByDefault" defaultChecked={!!settings.autoMergeByDefault} />}
              label={(
                <>
                  Auto-merge new tasks by default
                  <span className="hint">
                    off (default): a new task's own "Auto-merge once checks pass" checkbox starts unchecked. on: it
                    starts checked instead.
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
        </>
      )}
      {tab === "agents" && (
        <form onSubmit={submitAgents}>
          <Typography variant="subtitle2">Agent frameworks</Typography>
          <RadioGroup row aria-label="Agent framework" name="agentFramework" defaultValue={settings.agentFramework || "antigravity"} sx={{ mb: 1 }}>
            <FormControlLabel value="antigravity" control={<Radio />} label="Antigravity" />
            <FormControlLabel value="claude" control={<Radio />} label="Claude" />
          </RadioGroup>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            Which agent drives a run by default &mdash; the Antigravity CLI (agy) or the Claude CLI, each run as a
            subprocess on the controller. A task can override it for its own dispatch (New task &rarr;
            Advanced options), so both frameworks want a credential below.
          </Typography>
          <AgentKeysSection settings={settings} showError={showError} />
          <TextField name="geminiModel" label="Gemini model" defaultValue={settings.geminiModel || ""} autoComplete="off" fullWidth margin="normal" />
          <TextField name="maxAgentTurns" label="Max agent turns" helperText="0 = the agent framework's own default" type="number" inputProps={{ min: 0, step: 1 }} defaultValue={String(settings.maxAgentTurns || 0)} fullWidth margin="normal" />

          <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
            <Button type="submit" variant="contained">Save</Button>
          </Stack>
        </form>
      )}
      {tab === "github" && (
        <form onSubmit={submitGithub}>
          <TextField name="githubHost" label="GitHub host" defaultValue={settings.githubHost || ""} autoComplete="off" fullWidth margin="normal" />
          <FormControlLabel
            control={<Checkbox name="githubInsecureHttp" defaultChecked={!!settings.githubInsecureHttp} />}
            label={<>Speak plain HTTP to GitHub host <span className="hint">mock servers only</span></>}
            sx={{ display: "flex", mt: 1 }}
          />

          <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
            <Button type="submit" variant="contained">Save</Button>
          </Stack>
        </form>
      )}
      {tab === "sandbox" && (
        <form onSubmit={submitSandbox}>
          <TextField
            name="sandboxCpus"
            label="Sandbox vCPUs"
            helperText="default vCPU count for a kontur-managed sandbox VM. Overridable per task."
            type="number"
            inputProps={{ min: 0, step: 1 }}
            defaultValue={settings.sandboxCpus ? String(settings.sandboxCpus) : ""}
            placeholder={settings.sandboxCpusDefault ? String(settings.sandboxCpusDefault) : undefined}
            fullWidth
            margin="normal"
          />
          <TextField
            name="sandboxMemoryMb"
            label="Sandbox memory (MiB)"
            helperText="default guest memory, in MiB, for a kontur-managed sandbox VM. Overridable per task."
            type="number"
            inputProps={{ min: 0, step: 1 }}
            defaultValue={settings.sandboxMemoryMb ? String(settings.sandboxMemoryMb) : ""}
            placeholder={settings.sandboxMemoryMbDefault ? String(settings.sandboxMemoryMbDefault) : undefined}
            fullWidth
            margin="normal"
          />

          <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
            <Button type="submit" variant="contained">Save</Button>
          </Stack>
        </form>
      )}
      {tab === "capabilities" && (
        <>
          <form onSubmit={submitCapabilities}>
            <Typography variant="subtitle2">GCP</Typography>
            <TextField name="gcpProject" label="GCP project" helperText="optional -- enables the gcp-key/gemini-key capabilities" defaultValue={settings.gcpProject || ""} autoComplete="off" fullWidth margin="normal" />
            <TextField name="gcpServiceAccountEmail" label="GCP service account email" helperText="optional" defaultValue={settings.gcpServiceAccountEmail || ""} autoComplete="off" fullWidth margin="normal" />

            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2, mb: 2 }}>
              <Button type="submit" variant="contained">Save</Button>
            </Stack>
          </form>
          <CapabilitiesPanel capabilities={settings.capabilities} />
        </>
      )}
      {tab === "secrets" && <SecretsPanel showError={showError} />}
      {tab === "upgrade" && <UpgradePanel showError={showError} />}
    </Overlay>
  );
}
