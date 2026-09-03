import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Checkbox, Chip, FormControl, FormControlLabel, FormHelperText, InputLabel, ListItemText, MenuItem, Radio, RadioGroup, Select, Stack, Tab, Tabs, TextField, Typography } from "@mui/material";
import api from "../api.js";
import AgentKeysSection, { AGENT_KEY_SECRETS } from "./AgentKeysSection.jsx";
import CapabilitiesPanel from "./CapabilitiesPanel.jsx";
import Overlay from "./Overlay.jsx";
import SecretsPanel from "./SecretsPanel.jsx";
import UpgradePanel from "./UpgradePanel.jsx";
import { useThemeMode } from "../ThemeModeContext.jsx";
import { capabilityRows } from "../state.js";

// bwsalmon/agents#456: Secrets and Upgrade used to be their own top-level
// sidebar overlays; they live here now as tabs alongside the rest of
// deployment settings, since all of it is the same kind of operator-only,
// deployment-wide configuration.
//
// grain/task-115: and it is a full-height pane beside the sidebar now
// (Overlay's own `pane`), not a centered dialog. Six tabs of
// configuration is a destination an operator navigates around, not a
// single form to fill in and dismiss, so it gets the same shape opening
// a task or a schedule does -- with the title and the tab strip as the
// pane's fixed header, so switching tabs never means scrolling back up
// to find them.
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
//
// Secrets had a tab of its own here until grain/task-110, listing every
// secret in the store with a secret/key/value form under it. A secret is
// only ever meaningful to whatever resolves it, though, and that tab said
// nothing about which name belonged to which framework or capability --
// so each one now sits with what uses it: the agent credentials on
// Agents (AgentKeysSection), every capability's own beside it on
// Capabilities (CapabilitiesPanel), and whatever neither of those claims
// under "Other secrets" at the foot of that same tab (SecretsPanel).
const TABS = [
  { id: "general", label: "General" },
  { id: "agents", label: "Agents" },
  { id: "github", label: "GitHub" },
  { id: "sandbox", label: "Sandbox" },
  { id: "capabilities", label: "Capabilities" },
  { id: "upgrade", label: "Upgrade" },
];

// Saving a setting here changes what the deployment does straight away:
// the daemon re-reads its stored configuration once per reconcile tick
// and applies whatever changed (cmd/grain/daemon.go's liveConfig). The
// handful that genuinely cannot be swapped under a live deployment come
// back from the API as settings.restartRequired, and the ones changed but
// not yet running as settings.pendingRestart -- both keyed by the same
// JSON field name the form's own inputs are named after, so annotating a
// field is a lookup rather than a second list to keep in step.
//
// SETTING_LABELS only supplies human wording for the banner that names
// them; a key with no entry falls back to the key itself, which is the
// right failure mode for a UI running against a newer daemon that has
// added a restart-only setting this build has never heard of.
const SETTING_LABELS = {
  githubHost: "GitHub host",
  githubInsecureHttp: "Speak plain HTTP to GitHub host",
};

const settingLabel = (key) => SETTING_LABELS[key] || key;

export default function SettingsOverlay({ onClose, showError }) {
  const [tab, setTab] = useState("general");
  const [settings, setSettings] = useState(null);
  // defaultCapabilities is the one field on this pane that is not a
  // plain form input: it is a multi-select of capability ids (the set
  // every new task is filed holding, model.Config.DefaultCapabilities),
  // so it needs state of its own rather than being read off the form at
  // submit. Seeded from the loaded settings and from every response that
  // carries the field back, so saving another tab does not strand it.
  const [defaultCapabilities, setDefaultCapabilities] = useState([]);
  const { mode: themeMode, setMode: setThemeMode } = useThemeMode();

  useEffect(() => {
    (async () => {
      try {
        const loaded = await api("/api/settings");
        setSettings(loaded);
        setDefaultCapabilities(loaded.defaultCapabilities || []);
      } catch (err) {
        showError(err);
      }
    })();
  }, [showError]);

  // Re-reads settings after something on the pane changed what they
  // would say without going through save() -- setting or clearing a
  // capability's own secret (CapabilitiesPanel), which moves that
  // capability's readiness and the "set" chip on the field itself.
  //
  // Deliberately does not re-seed defaultCapabilities the initial load
  // does: a secret written on this tab has nothing to say about a
  // capability selection ticked here but not yet saved, and re-seeding
  // would quietly throw it away.
  const reloadSettings = useCallback(async () => {
    try {
      setSettings(await api("/api/settings"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  // save only puts a field in the request when it differs from what was
  // last loaded, so an operator changing one field on one tab never
  // overwrites a field that lives on another -- the same nil-means-
  // unchanged contract UpdateSettingsRequest's pointer fields already
  // give a PUT that leaves a key out entirely.
  //
  // A successful save closes the whole overlay: Save is the button that
  // finishes with this pane, so it saves and leaves rather than saving
  // and sitting there with nothing left to say. It briefly did the
  // opposite, once the pane grew a tab per subject, so that a second
  // tab could be saved without a reopen -- but a tab only owns the
  // fields it submits, so anything typed on another tab and left
  // unsaved was never going into that PUT anyway, and staying open made
  // a saved pane and an unsaved one look exactly alike.
  //
  // A failed save keeps it open, error banner and typed values intact:
  // closing there would throw away the very edit that has to be fixed.
  //
  // The response is still folded into the loaded settings before
  // closing, so the state the pane leaves behind matches what was
  // stored -- and it is merged rather than assigned outright, since a
  // test's mocked response is typically a bare {}, which a plain
  // assignment would otherwise wipe every other field's value with.
  //
  // One thing closing does cost: saving a restart-only field no longer
  // leaves the "saved, but not applied yet" banner on screen, since the
  // pane carrying it is gone by then. The field said "takes effect when
  // the daemon restarts" while it was being edited, and the banner is
  // still there on the next open, so the fact is told before the save
  // rather than only after it.
  const save = async (payload) => {
    try {
      const updated = await api("/api/settings", { method: "PUT", body: JSON.stringify(payload) });
      setSettings((prev) => ({ ...prev, ...updated }));
      if ("defaultCapabilities" in updated) setDefaultCapabilities(updated.defaultCapabilities || []);
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

    // Trimmed here and again on the way in (ui.UpdateSettings), so
    // clearing the box back to blank sends "" -- a deliberate "this
    // deployment has no name" -- rather than being mistaken for
    // unchanged the way a blank sandbox-shape box would be.
    const environmentName = form.elements.environmentName.value.trim();
    if (environmentName !== (settings.environmentName || "")) payload.environmentName = environmentName;

    const pollInterval = form.elements.pollInterval.value.trim();
    if (pollInterval !== (settings.pollInterval || "")) payload.pollInterval = pollInterval;

    const maxWorkersRaw = form.elements.maxWorkers.value.trim();
    if (maxWorkersRaw !== "") {
      const maxWorkers = parseInt(maxWorkersRaw, 10);
      if (maxWorkers !== (settings.maxWorkers || 0)) payload.maxWorkers = maxWorkers;
    }

    const maxMergersRaw = form.elements.maxMergers.value.trim();
    if (maxMergersRaw !== "") {
      const maxMergers = parseInt(maxMergersRaw, 10);
      if (maxMergers !== (settings.maxMergers || 0)) payload.maxMergers = maxMergers;
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

    const claudeModel = form.elements.claudeModel.value.trim();
    if (claudeModel !== (settings.claudeModel || "")) payload.claudeModel = claudeModel;

    const maxAgentTurnsRaw = form.elements.maxAgentTurns.value.trim();
    if (maxAgentTurnsRaw !== "") {
      const maxAgentTurns = parseInt(maxAgentTurnsRaw, 10);
      if (maxAgentTurns !== (settings.maxAgentTurns || 0)) payload.maxAgentTurns = maxAgentTurns;
    }

    // Trimmed here and again on the way in (ui.UpdateSettings), so
    // clearing the box back to blank sends "" -- a deliberate "tell runs
    // nothing extra" -- rather than reading as unchanged.
    const promptExtension = form.elements.promptExtension.value.trim();
    if (promptExtension !== (settings.promptExtension || "")) payload.promptExtension = promptExtension;

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
    // not "leave it alone": unlike every other field on this pane, an unset one
    // of these reads as blank rather than as its stored value, so an operator
    // who never touched it and one who cleared it back to blank on purpose type
    // the same thing here. (vCPUs and memory show the real default faintly, as a
    // placeholder; disk has no such number to show -- see its own field below.)
    const sandboxCpusRaw = form.elements.sandboxCpus.value.trim();
    const sandboxCpus = sandboxCpusRaw === "" ? 0 : parseInt(sandboxCpusRaw, 10);
    if (sandboxCpus !== (settings.sandboxCpus || 0)) payload.sandboxCpus = sandboxCpus;

    const sandboxMemoryMbRaw = form.elements.sandboxMemoryMb.value.trim();
    const sandboxMemoryMb = sandboxMemoryMbRaw === "" ? 0 : parseInt(sandboxMemoryMbRaw, 10);
    if (sandboxMemoryMb !== (settings.sandboxMemoryMb || 0)) payload.sandboxMemoryMb = sandboxMemoryMb;

    const sandboxDiskGbRaw = form.elements.sandboxDiskGb.value.trim();
    const sandboxDiskGb = sandboxDiskGbRaw === "" ? 0 : parseInt(sandboxDiskGbRaw, 10);
    if (sandboxDiskGb !== (settings.sandboxDiskGb || 0)) payload.sandboxDiskGb = sandboxDiskGb;

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

    // Compared as a set, not by identity: an operator who ticked one box
    // and unticked another has changed it, and one who reordered nothing
    // has not. A present list replaces the whole set server-side
    // (ui.UpdateSettingsRequest.DefaultCapabilities), empty included, so
    // sending it only when it differs keeps this tab from rewriting a
    // set another client changed underneath it.
    const stored = settings.defaultCapabilities || [];
    const same = stored.length === defaultCapabilities.length
      && stored.every((id) => defaultCapabilities.includes(id));
    if (!same) payload.defaultCapabilities = defaultCapabilities;

    return save(payload);
  };

  if (settings === null) return null;

  const restartRequired = new Set(settings.restartRequired || []);
  const pendingRestart = new Set(settings.pendingRestart || []);

  // The Capabilities tab's own picker rows. settings.capabilities is
  // every capability grain ships a provider for, so an ungrantable one
  // is filtered out here before capabilityRows appends anything: what is
  // left is what UpdateSettings validates the set against, plus a row
  // for each already-stored id this build no longer offers at all.
  const capabilityChoices = capabilityRows(
    (settings.capabilities || []).filter((c) => c.grantable !== false),
    defaultCapabilities,
  );

  // Every secret a control on this pane already owns: the two agent
  // credentials on the Agents tab, and each capability's own, set beside
  // it in CapabilitiesPanel. "Other secrets" is the remainder -- what is
  // in the store with nothing here claiming it -- so this list is what
  // keeps a secret from being offered twice, in two places, with two
  // different explanations of what it is for.
  const claimedSecrets = [
    ...AGENT_KEY_SECRETS,
    ...(settings.capabilities || []).flatMap((c) => (c.secrets || []).map((s) => s.secret)),
  ];

  // restartHint annotates one field: "this one needs a restart" always,
  // and "you have already changed it, and it isn't running yet" once the
  // stored value and the running daemon's have actually diverged. base is
  // whatever helper text the field had of its own, kept in front so the
  // annotation reads as an addition rather than a replacement.
  const restartHint = (field, base) => {
    if (!restartRequired.has(field)) return base;
    if (pendingRestart.has(field)) {
      return (
        <>
          {base ? `${base} ` : ""}
          <Box component="span" sx={{ color: "warning.main" }}>
            Changed, but not applied &mdash; the daemon is still running with its previous value. Restart it to
            apply.
          </Box>
        </>
      );
    }
    return `${base ? `${base} ` : ""}Takes effect when the daemon restarts, unlike every other setting here.`;
  };

  // The same annotation as a badge on the control itself, so the field
  // carries it whether or not anyone reads the helper text: "needs
  // restart" from the moment the pane opens, turning into a warning-
  // coloured "restart to apply" once the stored value has actually moved
  // away from what the daemon is running.
  const restartChip = (field) => {
    if (!restartRequired.has(field)) return null;
    const changed = pendingRestart.has(field);
    return (
      <Chip
        size="small"
        variant={changed ? "filled" : "outlined"}
        color={changed ? "warning" : "default"}
        label={changed ? "restart to apply" : "needs restart"}
        sx={{ ml: 1 }}
      />
    );
  };

  // The same fact at the top of the pane, so it is visible from whichever
  // tab is open rather than only from the one the field lives on.
  const pending = (settings.pendingRestart || []).map(settingLabel);

  // The pane's fixed chrome: the title and the tab strip stay put while
  // whichever tab is open scrolls under them (grain/task-115). The strip
  // scrolls sideways rather than wrapping, so a narrow window shortens
  // it instead of pushing the tallest tab's fields further down.
  const header = (
    <>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Settings</Typography>
      <Tabs value={tab} onChange={(_, value) => setTab(value)} variant="scrollable" scrollButtons="auto">
        {TABS.map((t) => (
          <Tab key={t.id} value={t.id} label={t.label} />
        ))}
      </Tabs>
    </>
  );

  return (
    <Overlay onClose={onClose} pane header={header}>
      {/* Capped like any other pane's form: these are single-column
          fields, and a "Poll interval" box the width of a widescreen
          reads no better here than it does anywhere else. */}
      <div className="pane-form">
        {pending.length > 0 && (
          <Alert severity="warning" sx={{ mb: 2 }}>
            Saved, but not applied yet: {pending.join(", ")}. Everything else here takes effect within a poll
            interval; these only take effect when the daemon restarts.
          </Alert>
        )}
        {!settings.configured && (
          <Alert severity="info" sx={{ mb: 2 }}>
            Not configured yet -- nothing has been saved for this deployment. Poll interval and max concurrent
            (General), Gemini model and Claude model (Agents) and GitHub host (GitHub) are required the first time.
          </Alert>
        )}
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
              <Typography variant="subtitle2" sx={{ mt: 1 }}>Deployment</Typography>
              <TextField
                name="environmentName"
                label="Environment name"
                helperText="Shown beside the grain mark and in the browser tab, e.g. staging. Leave empty for an unnamed deployment."
                defaultValue={settings.environmentName || ""}
                inputProps={{ maxLength: 32 }}
                autoComplete="off"
                fullWidth
                margin="normal"
              />
              <TextField name="pollInterval" label="Poll interval" helperText="Go duration, e.g. 30s" defaultValue={settings.pollInterval || ""} autoComplete="off" fullWidth margin="normal" />
              <TextField name="maxWorkers" label="Max worker agents" helperText="maximum number of ordinary tasks dispatched at once" type="number" inputProps={{ min: 1, step: 1 }} defaultValue={String(settings.maxWorkers || "")} fullWidth margin="normal" />
              <TextField name="maxMergers" label="Max merge agents" helperText="extra agents only the merge queue may dispatch, to repair a pull request that will not land -- on top of the workers above, and free to use a spare worker slot too. 0 makes them wait for one like anything else" type="number" inputProps={{ min: 0, step: 1 }} defaultValue={String(settings.maxMergers ?? "")} fullWidth margin="normal" />
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
                      on (default): a new task's own "Queue immediately" checkbox starts checked, filing a task ready
                      to dispatch at once. off: it starts unchecked instead, filing it as a proposal needing
                      approval.
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
                      on (default): a new task's own "Auto-merge once checks pass" checkbox starts checked. off: it
                      starts unchecked instead.
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
            <TextField name="claudeModel" label="Claude model" defaultValue={settings.claudeModel || ""} autoComplete="off" fullWidth margin="normal" />
            <TextField name="maxAgentTurns" label="Max agent turns" helperText="0 = uncapped; runs are bounded by wall-clock runtime instead" type="number" inputProps={{ min: 0, step: 1 }} defaultValue={String(settings.maxAgentTurns || 0)} fullWidth margin="normal" />

            {/* The deployment-wide layer of model/prompt_extension.go's
                three. It lives on this tab rather than General because it
                is about what the agent is told, next to which agent and
                which model -- and, like those, it reaches the next run
                dispatched rather than needing a restart. */}
            <Typography variant="subtitle2" sx={{ mt: 3 }}>Prompt extension</Typography>
            <TextField
              name="promptExtension"
              label="Standing instructions for every run"
              helperText="Added to the end of every run's prompt on this deployment -- house style, a test command, a convention grain has no way to know. A repo can add its own on the Repos pane, and a task can replace both (New task -> Advanced options). Leave empty to add nothing."
              defaultValue={settings.promptExtension || ""}
              multiline
              minRows={4}
              autoComplete="off"
              fullWidth
              margin="normal"
            />

            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
              <Button type="submit" variant="contained">Save</Button>
            </Stack>
          </form>
        )}
        {tab === "github" && (
          <form onSubmit={submitGithub}>
            <TextField
              name="githubHost"
              label="GitHub host"
              helperText={restartHint("githubHost", "")}
              defaultValue={settings.githubHost || ""}
              autoComplete="off"
              fullWidth
              margin="normal"
              InputProps={{ endAdornment: restartChip("githubHost") }}
            />
            <FormControlLabel
              control={<Checkbox name="githubInsecureHttp" defaultChecked={!!settings.githubInsecureHttp} />}
              label={(
                <>
                  Speak plain HTTP to GitHub host <span className="hint">mock servers only</span>
                  {restartChip("githubInsecureHttp")}
                  {restartRequired.has("githubInsecureHttp") && (
                    <span className="hint">{restartHint("githubInsecureHttp", "")}</span>
                  )}
                </>
              )}
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
            {/*
              Disk shows a faint placeholder default like the two above now.
              It used to have none, because an empty box meant "as large as
              the guest image behind the VM" -- a property of the image this
              deployment built rather than a number the API could report.
              grain names all three sizes itself and passes all three on
              every create, so sandboxDiskGbDefault is a real number here.
            */}
            <TextField
              name="sandboxDiskGb"
              label="Sandbox disk (GiB)"
              helperText="default root disk size, in GiB, for a kontur-managed sandbox VM. Overridable per task."
              type="number"
              inputProps={{ min: 0, step: 1 }}
              defaultValue={settings.sandboxDiskGb ? String(settings.sandboxDiskGb) : ""}
              placeholder={settings.sandboxDiskGbDefault ? String(settings.sandboxDiskGbDefault) : undefined}
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
              {/* The credential those two capabilities mint *through* is
                  not a setting and is not here: it is a secret, set on the
                  gcp-key row further down this tab (grain/task-110). */}
              <TextField name="gcpServiceAccountEmail" label="GCP service account email" helperText="optional -- the minter's own key is set with the gcp-key capability below" defaultValue={settings.gcpServiceAccountEmail || ""} autoComplete="off" fullWidth margin="normal" />

              <Typography variant="subtitle2" sx={{ mt: 2 }}>New tasks</Typography>
              {/* Only grantable capabilities are offered: the set is
                  validated against the same picker listing a task's own
                  capabilities are, and one no task could be granted by hand
                  would be a default that failed at every filing. A stored
                  id this build has retired gets a row anyway, purely so it
                  can be unticked -- capabilityRows (state.js) has why a
                  pane without one cannot be saved at all. */}
              <FormControl fullWidth margin="normal" size="small">
                <InputLabel id="settings-default-capabilities-label">Default capabilities</InputLabel>
                <Select
                  labelId="settings-default-capabilities-label"
                  label="Default capabilities"
                  multiple
                  value={defaultCapabilities}
                  onChange={(e) => setDefaultCapabilities(e.target.value)}
                  renderValue={(selected) => (
                    <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                      {selected.map((id) => {
                        const cap = capabilityChoices.find((c) => c.id === id);
                        return <Chip key={id} size="small" label={cap ? cap.name || cap.id : id} />;
                      })}
                    </Box>
                  )}
                >
                  {capabilityChoices.map((c) => (
                    <MenuItem key={c.id} value={c.id} title={c.description}>
                      <Checkbox checked={defaultCapabilities.includes(c.id)} size="small" />
                      <ListItemText primary={c.name || c.id} secondary={c.retired ? c.description : null} />
                    </MenuItem>
                  ))}
                </Select>
                <FormHelperText>
                  attached to every new task as it is filed, whichever repo it targets -- whoever files one can
                  untick any of these on the new-task form, and any of them can be detached from a task
                  afterwards. Tasks already filed keep what they were filed with. An individual repo can add
                  more of its own, on the repos page.
                </FormHelperText>
              </FormControl>

              <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2, mb: 2 }}>
                <Button type="submit" variant="contained">Save</Button>
              </Stack>
            </form>
            <CapabilitiesPanel
              capabilities={settings.capabilities}
              showError={showError}
              onSecretsChanged={reloadSettings}
            />
            <SecretsPanel showError={showError} claimed={claimedSecrets} />
          </>
        )}
        {tab === "upgrade" && <UpgradePanel showError={showError} />}
      </div>
    </Overlay>
  );
}
