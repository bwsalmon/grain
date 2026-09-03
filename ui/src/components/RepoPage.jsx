import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Checkbox, Chip, FormControl, FormHelperText, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { STATE_LABELS, STATE_ORDER, capabilityName, capabilityRows, capabilityUnavailableHint, repoRows, unionCapabilities } from "../state.js";

// RepoPage is one repo's own page (grain/task-111), at /repos/{owner}/
// {name} -- the repo-side counterpart of a task's own /tasks/{id} page.
// Everything that is a property of one repo lives here: its task counts,
// the branches grain has been asked to create in it, the default
// capabilities every task filed against it starts with, the standing
// instructions every run against it is given, the way into its releases,
// and removing it from the deployment's allowlist.
//
// That is the whole point of it. Each of those used to be a button on
// the repo *list* row (bwsalmon/agents#459, #473, #474, #638), which
// left that list carrying five controls per row and folding forms out
// between rows -- so a list of repos read as a toolbar rather than as a
// list. The list is now one row per repo, name and counts, and clicking
// it lands here; a repo's controls have a page with room for them, and
// each one is a URL somebody can link to.
//
// The repo's task list is not rendered here: it is TaskList itself,
// scoped to this repo and passed in as children by App.jsx, so this page
// gets the search/sort/select/drag-reorder behavior the flat task list
// already has instead of a second, poorer list of its own. App keeps the
// selection and reorder wiring it already owns for that list rather than
// threading it through here.
export default function RepoPage({ repo, tasks, config, onBack, onNewTask, onOpenReleases, onRefreshConfig, showError, children }) {
  const [owner, name] = repo.split("/");
  const [branches, setBranches] = useState([]);
  // caps is what GET /api/repos/{owner}/{name}/capabilities last said --
  // all three sets: the repo's own, the deployment's, and the union a
  // task filed here would actually start with -- and capsSelection the
  // unsaved ticks. caps is null while that read is in flight, which the
  // form renders a loading line for rather than an empty picker that
  // would look like "this repo adds nothing".
  const [caps, setCaps] = useState(null);
  const [capsSelection, setCapsSelection] = useState([]);
  // prompt/promptText are that same shape again, for this repo's own
  // prompt extension (grain/task-114): what GET /api/repos/{owner}/
  // {name}/prompt-extension last said -- the repo's own text, the
  // deployment's, and the two as a run would actually be given them --
  // and the unsaved edit. prompt is null while that read is in flight,
  // since an empty box would otherwise look like "this repo says
  // nothing", which is a real and different state.
  const [prompt, setPrompt] = useState(null);
  const [promptText, setPromptText] = useState("");

  // The row this repo would have on the list page: its per-state counts,
  // whether it is on the allowlist (which is what makes Remove worth
  // offering), and whether it carries defaults of its own. A repo this
  // deployment knows nothing about -- a hand-typed URL, or one whose
  // last task and allowlist entry both went away while this page was
  // open -- has no row, and gets an empty one rather than crashing the
  // page: its forms still work, since none of them needs the repo to be
  // known first.
  const row = repoRows(config, tasks).find((r) => r.repo === repo)
    || { repo, total: 0, counts: {}, blocked: 0, configured: false, defaults: false };

  const loadBranches = useCallback(async () => {
    try {
      setBranches(await api(`/api/repos/${owner}/${name}/branches`));
    } catch (err) {
      showError(err);
    }
  }, [owner, name, showError]);

  const loadCapabilities = useCallback(async () => {
    try {
      const loaded = await api(`/api/repos/${owner}/${name}/capabilities`);
      setCaps(loaded);
      setCapsSelection(loaded.defaultCapabilities || []);
    } catch (err) {
      showError(err);
    }
  }, [owner, name, showError]);

  const loadPromptExtension = useCallback(async () => {
    try {
      const loaded = await api(`/api/repos/${owner}/${name}/prompt-extension`);
      setPrompt(loaded);
      setPromptText(loaded.promptExtension || "");
    } catch (err) {
      showError(err);
    }
  }, [owner, name, showError]);

  // All three reads happen on landing rather than behind a toggle the
  // way the list page's own fold-out forms did: this page is *about*
  // this repo, so there is nothing else the GETs could be competing
  // with.
  useEffect(() => { loadBranches(); }, [loadBranches]);
  useEffect(() => { loadCapabilities(); }, [loadCapabilities]);
  useEffect(() => { loadPromptExtension(); }, [loadPromptExtension]);

  // capsEffective is what a task filed against this repo would actually
  // start out holding -- the line the capabilities form ends on. The
  // union is recomputed from the unsaved ticks rather than read back from
  // caps.effectiveDefaultCapabilities, so it describes the set Save is
  // about to make real rather than the one the last response described.
  //
  // Filtered to what this build still offers, the same filter
  // ui.(*Client).defaultCapabilities applies before any grant is written
  // -- and so the same one behind RepoDefaults.
  // EffectiveDefaultCapabilities, the server's own answer to this
  // question. Neither of the two sets GET reports is filtered: both come
  // back exactly as stored, retired ids included, deliberately, so an id
  // chosen before a build retired it stays visible in the picker above
  // to be unticked. But a task filed here does not start out holding
  // one, so this line must not say it does.
  const capsEffective = unionCapabilities(caps?.deploymentDefaultCapabilities, capsSelection)
    .filter((id) => (config?.capabilities || []).some((c) => c.id === id));

  // Removing the repo from the allowlist leaves this page describing a
  // repo the deployment no longer allows -- and, if nothing else here
  // mentions it, one the list page no longer shows either -- so it goes
  // back to that list rather than staying put.
  const removeRepo = async () => {
    if (!confirm(`Remove ${repo} from target repos? Tasks that already target it are not affected, but new tasks won't be able to until it's added back.`)) return;
    try {
      await api(`/api/repos/${owner}/${name}`, { method: "DELETE" });
      await onRefreshConfig();
      onBack();
    } catch (err) {
      showError(err);
    }
  };

  // createBranch only ever records the request -- the branches reconciler
  // (pkg/orchestrator.SyncBranches) is what actually creates it on GitHub,
  // typically within one cycle, so a freshly submitted name reappears
  // below still "pending" until the next loadBranches picks up "created"
  // or, if GitHub refused it, an error.
  const createBranch = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const branchName = form.elements.branchName.value.trim();
    if (branchName === "") return;
    try {
      await api(`/api/repos/${owner}/${name}/branches`, { method: "POST", body: JSON.stringify({ name: branchName }) });
      form.reset();
      await loadBranches();
    } catch (err) {
      showError(err);
    }
  };

  // saveCapabilities replaces this repo's own set wholesale (PUT's whole
  // body is the new set, ui.SetRepoCapabilitiesRequest), then refreshes
  // the config the new-task form seeds its own picker from -- otherwise a
  // repo whose defaults just changed would keep filing tasks with the old
  // ones until the page was reloaded.
  const saveCapabilities = async (evt) => {
    evt.preventDefault();
    try {
      const updated = await api(`/api/repos/${owner}/${name}/capabilities`, {
        method: "PUT",
        body: JSON.stringify({ defaultCapabilities: capsSelection }),
      });
      setCaps(updated);
      setCapsSelection(updated.defaultCapabilities || []);
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  // savePromptExtension replaces this repo's own text wholesale (PUT's
  // whole body is the new text, ui.SetRepoPromptExtensionRequest), then
  // refreshes the config, for the same reason saveCapabilities does.
  // Nothing the new-task form seeds itself from changes here -- a repo's
  // own text is read at dispatch, not written onto the task -- but
  // config.reposWithPromptExtension is one of the three things that make
  // a repo appear on the list page at all (repoRows, state.js). Writing
  // the first standing instructions for a repo with no tasks and no
  // allowlist entry is what puts it on that list, and clearing its last
  // ones is what takes it off; without this the list keeps whichever
  // answer it was given at mount until the page is reloaded, and the
  // repo whose text is the only thing keeping it reachable is the one
  // that would go missing.
  const savePromptExtension = async (evt) => {
    evt.preventDefault();
    try {
      const updated = await api(`/api/repos/${owner}/${name}/prompt-extension`, {
        method: "PUT",
        body: JSON.stringify({ promptExtension: promptText.trim() }),
      });
      setPrompt(updated);
      setPromptText(updated.promptExtension || "");
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <div className="main-column">
      <div className="repo-page-header">
        <Button onClick={onBack} sx={{ ml: -0.9, alignSelf: "flex-start" }}>&larr; Repos</Button>
        <Stack direction="row" alignItems="center" spacing={1} flexWrap="wrap" useFlexGap>
          <Typography variant="h6" component="h2" sx={{ m: 0, fontSize: "1.15rem", fontWeight: 600 }}>{repo}</Typography>
          <span className="chips">
            {/* Each chip counts this repo's tasks in one state, not one
                task's own status, so "running" keeps the plain CSS spin
                (style.css's .badge-running) rather than StateDot's grain
                mark (bwsalmon/agents#586) -- there is no single task for
                the mark to represent. */}
            {STATE_ORDER.filter((s) => row.counts[s]).map((s) => (
              <Chip key={s} size="small" className={`badge badge-${s}`} label={`${STATE_LABELS[s]} ${row.counts[s]}`} />
            ))}
            {row.blocked > 0 && <Chip size="small" color="error" label={`Blocked ${row.blocked}`} />}
          </span>
          <Typography variant="caption" color="text.secondary" whiteSpace="nowrap">
            {row.total} task{row.total === 1 ? "" : "s"}
          </Typography>
          <Box sx={{ flex: 1 }} />
          <Button size="small" variant="contained" onClick={() => onNewTask(repo)}>New task</Button>
          <Button size="small" variant="outlined" onClick={() => onOpenReleases(repo)}>Releases</Button>
          {row.configured && (
            <Button size="small" variant="outlined" color="error" onClick={removeRepo}>Remove</Button>
          )}
        </Stack>

        {/* A repo with no tasks that is off the allowlist is known to
            this deployment only because it carries configuration of its
            own (repoRows' third source). Without saying so the page reads
            as being about a repo nothing here has anything to do with,
            and the two forms below are the only things that put it
            here. */}
        {row.defaults && !row.configured && row.total === 0 && (
          <Alert severity="info">
            No tasks, and not on this deployment&apos;s target repos -- {repo} is known here only because it
            carries configuration of its own -- default capabilities, standing instructions, or both --
            below.
          </Alert>
        )}

        <Box>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Branches</Typography>
          <Stack component="form" direction="row" spacing={1} alignItems="flex-start" onSubmit={createBranch}>
            <TextField
              name="branchName" label="New branch name" placeholder="feature/foo"
              helperText="Created from the repo's current default branch"
              autoComplete="off" required InputLabelProps={{ required: false }} size="small"
            />
            <Button type="submit" variant="outlined" size="small">Create branch</Button>
          </Stack>
          {branches.length > 0 && (
            <ul className="candidate-history" style={{ marginTop: "0.75rem" }}>
              {branches.map((b) => (
                <li key={`${b.name}-${b.createdAt}`}>
                  <strong>{b.name}</strong> -- {b.status}
                  {b.error && <span className="candidate-error"> ({b.error})</span>}
                </li>
              ))}
            </ul>
          )}
        </Box>

        <Box>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Default capabilities</Typography>
          {caps === null ? (
            <Typography variant="body2" color="text.secondary">Loading capabilities…</Typography>
          ) : (
            <Stack component="form" spacing={1} onSubmit={saveCapabilities}>
              <FormControl fullWidth size="small">
                <InputLabel id="repo-capabilities-label">Default capabilities</InputLabel>
                <Select
                  labelId="repo-capabilities-label"
                  label="Default capabilities"
                  multiple
                  value={capsSelection}
                  onChange={(e) => setCapsSelection(e.target.value)}
                  renderValue={(chosen) => (
                    <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                      {chosen.map((id) => (
                        <Chip key={id} size="small" label={capabilityName(config, id)} />
                      ))}
                    </Box>
                  )}
                >
                  {/* The same grantable-only listing Settings' own
                      default-capabilities picker offers, and for the same
                      reason: a default no task can be granted by hand
                      would fail at every filing, and PUT rejects one
                      anyway. It needs no filter of its own to say that,
                      unlike Settings' settings.capabilities:
                      config.capabilities *is* ui.OfferedCapabilities,
                      which is what "grantable" means, where that one
                      reports every provider grain ships and flags the
                      ungrantable ones. A capability the deployment
                      already defaults is still offered rather than
                      hidden -- ticking it here is how a repo keeps it if
                      the deployment-wide entry is later dropped -- and
                      says so in its own row, so nobody reads a blank box
                      as "not on here". An id this repo stored before the
                      build retired it gets a row too, purely so it can be
                      unticked (capabilityRows, state.js). */}
                  {capabilityRows(config?.capabilities, capsSelection).map((c) => (
                    <MenuItem key={c.id} value={c.id} title={c.description}>
                      <Checkbox checked={capsSelection.includes(c.id)} size="small" />
                      <ListItemText
                        primary={c.name}
                        secondary={c.retired
                          ? c.description
                          /* A default this deployment cannot honour is worth
                             more warning here than on one task, not less: it
                             fails every task filed against this repo, and the
                             person who set it is not the one who sees them
                             fail. */
                          : capabilityUnavailableHint(c)
                            || ((caps.deploymentDefaultCapabilities || []).includes(c.id)
                              ? "already a deployment default -- on here either way"
                              : null)}
                        secondaryTypographyProps={capabilityUnavailableHint(c)
                          ? { color: "warning.main" }
                          : undefined}
                      />
                    </MenuItem>
                  ))}
                </Select>
                <FormHelperText>
                  Added to this deployment&apos;s own defaults, never subtracted from them -- a repo can
                  only widen what a task filed against it starts with. Whoever files one can still untick
                  any of these on the new-task form, and tasks already filed keep what they were filed
                  with.
                </FormHelperText>
              </FormControl>
              <Typography variant="body2" color="text.secondary">
                A task filed against {repo} starts with:{" "}
                {capsEffective.length === 0
                  ? "nothing -- only what whoever files it ticks"
                  : capsEffective.map((id) => capabilityName(config, id)).join(", ")}
              </Typography>
              <Stack direction="row" justifyContent="flex-end">
                <Button type="submit" variant="contained" size="small">Save capabilities</Button>
              </Stack>
            </Stack>
          )}
        </Box>

        <Box>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>Prompt extension</Typography>
          {prompt === null ? (
            <Typography variant="body2" color="text.secondary">Loading prompt extension…</Typography>
          ) : (
            <Stack component="form" spacing={1} onSubmit={savePromptExtension}>
              <TextField
                name="repoPromptExtension"
                label={`Prompt extension for ${repo}`}
                helperText="Added to this deployment's own standing instructions for a run against this repo, never replacing them. A task can replace both for itself (New task -> Advanced options). Leave empty for a repo that adds nothing."
                value={promptText}
                onChange={(e) => setPromptText(e.target.value)}
                multiline
                minRows={4}
                autoComplete="off"
                fullWidth
                size="small"
              />
              {/* What the deployment already says, read-only and shown
                  whether or not this repo adds anything: text appended to
                  instructions nobody can see from here cannot be written
                  sensibly, and this is the layer somebody editing here is
                  appending to (ui.RepoDefaults.
                  DeploymentPromptExtension). */}
              <Typography variant="body2" color="text.secondary">
                Deployment-wide, set in Settings &rarr; Agents:{" "}
                {prompt.deploymentPromptExtension
                  ? <Box component="span" sx={{ whiteSpace: "pre-wrap" }}>{prompt.deploymentPromptExtension}</Box>
                  : "nothing"}
              </Typography>
              <Stack direction="row" justifyContent="flex-end">
                <Button type="submit" variant="contained" size="small">Save prompt extension</Button>
              </Stack>
            </Stack>
          )}
        </Box>
      </div>
      {children}
    </div>
  );
}
