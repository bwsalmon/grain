import { useState } from "react";
import AddIcon from "@mui/icons-material/Add";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import ChevronRightIcon from "@mui/icons-material/ChevronRight";
import { Alert, Box, Button, Checkbox, Chip, FormControl, FormHelperText, IconButton, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { STATE_LABELS, STATE_ORDER, capabilityName, capabilityRows, repoRows, unionCapabilities } from "../state.js";
import { TaskRow } from "./TaskList.jsx";
import { ListEmpty, ListHeader, ListSearchField, ListToolbar } from "./ListPrimitives.jsx";

// RepoList is the repo page: one row per known repo -- every
// config.targetRepos entry, plus any repo tasks target that isn't one,
// plus any repo carrying default capabilities of its own (repoRows has
// why all three) -- each showing how many tasks sit in every state so a
// repo with something stuck (awaiting_reply, or a pile of blocked work)
// stands out before anyone opens it. Clicking a row is the entry point
// into the repo-centric task list -- onOpenRepo scopes App's own task
// view to it.
//
// The chevron (bwsalmon/agents#474) is a second way into the same
// tasks: it folds them out right here, for a quick look that doesn't
// leave the repo page, without replacing the deeper task-list view the
// row itself still opens. The New branch, Releases and "+" buttons are
// further entry points into the same row (hence stopPropagation on all
// three, so none of them also fires onOpenRepo or the chevron toggle):
// release management is a property of the repo (bwsalmon/agents#459),
// creating a branch outright is too (bwsalmon/agents#638, for whatever
// doesn't fit release management's own latest/rc/prod shape), and filing
// a task against it is the repo page's own shortcut for not retyping the
// repo you're already looking at, so all three live here rather than
// behind a sidebar button reachable from anywhere. New branch's own form
// folds out below the row the same way the chevron's tasks do, rather
// than a modal, since it only ever needs the one field.
//
// Adding and removing a target repo (bwsalmon/agents#473) lives here
// too, replacing the "Target repos" list Settings used to bury this
// behind: a repo is a thing this page is about, not a deployment knob.
// Remove only appears on a row that is actually in config.targetRepos
// (repoRows' own `configured`) -- a row that only exists because a task
// already targets it has nothing to remove, and removing a configured
// repo doesn't make the row disappear either as long as a task still
// targets it, so the two facts (targeted, configured) stay visibly
// independent rather than the button pretending removal always clears
// the row.
//
// "Add repo" reads backwards the first time (grain/task-45). An empty
// config.targetRepos means *unrestricted* everywhere it appears --
// CreateTask enforces nothing, `grain settings` prints "unrestricted",
// every row here is `configured: false` -- so adding the first repo does
// not widen anything, it narrows the deployment to exactly that repo,
// and the next task filed against any other repo parks off the allowlist
// instead of dispatching. Two things say so, one before the click and
// one at it: the note below the header stands on the page for as long
// as the allowlist is empty (and, once it holds one entry, says that
// too -- keyed on the list rather than on the transition, the way `grain
// repo add`'s own one-line note is), and addRepo confirms the first add
// naming the repos that would fall off. The confirmation is the one that
// can be specific, since only it knows which repo is being added; the
// standing note is what somebody reads while deciding to click at all.
//
// The header/toolbar/row shape below mirrors TaskList's own
// (bwsalmon/agents#561): a .content-header with title, count, and this
// page's own primary action (the add-repo form, in place of TaskList's
// filter title or TemplatesList/SchedulesList's "+ New X" button) above
// a .task-list-toolbar search box, then flat divider rows instead of
// this list's old card-per-repo look -- so the four list pages read as
// one design instead of four.
export default function RepoList({ tasks, config, onOpenRepo, onOpenReleases, onOpenTask, onNewTask, onRefreshConfig, showError }) {
  const [newRepo, setNewRepo] = useState("");
  const [search, setSearch] = useState("");
  const repos = repoRows(config, tasks);
  const [expanded, setExpanded] = useState(() => new Set());
  // branchRepo is the one repo, if any, whose "New branch" form and
  // recent-branches list is open -- a single slot rather than a Set the
  // way `expanded` is, since only one repo's own branches are ever being
  // read from the API at a time (branches holds that one repo's list).
  const [branchRepo, setBranchRepo] = useState(null);
  const [branches, setBranches] = useState([]);
  // capsRepo/caps/capsSelection are the same one-slot shape branchRepo/
  // branches above use, for the per-repo default capability set
  // (grain/task-24): which repo's form is open, what GET
  // /api/repos/{owner}/{name}/capabilities last said (all three sets --
  // the repo's own, the deployment's, and the union a task filed here
  // would actually start with), and the unsaved ticks. caps is null
  // while that read is in flight, which is what the form renders a
  // loading line for rather than an empty picker that would look like
  // "this repo adds nothing".
  const [capsRepo, setCapsRepo] = useState(null);
  const [caps, setCaps] = useState(null);
  const [capsSelection, setCapsSelection] = useState([]);

  const q = search.trim().toLowerCase();
  const visible = repos.filter((r) => q === "" || r.repo.toLowerCase().includes(q));
  const targetRepos = config?.targetRepos || [];

  // capsEffective is what a task filed against the repo whose
  // capabilities form is open would actually start out holding -- the
  // line the form ends on. The union is recomputed from the unsaved
  // ticks rather than read back from caps.effectiveDefaultCapabilities,
  // so it describes the set Save is about to make real rather than the
  // one the last response described.
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

  const toggleExpanded = (repo) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };

  // firstAddWarning is what the confirmation says before the first repo
  // is added to an empty (== unrestricted) allowlist, or null when this
  // add can only ever widen an allowlist that already restricts.
  //
  // The repos it names are the rows this page is already showing. With
  // targetRepos empty none of them is `configured`, so every one is here
  // because a task targets it or because it carries default capabilities
  // of its own -- repoRows' other two sources -- and either way the
  // allowlist this click creates leaves it off, so the next task filed
  // against it parks. Worth listing, since that is the cost of the click
  // and nothing else on the page states it.
  //
  // Compared as typed rather than as the server will store it
  // (AddTargetRepo runs the string through model.ParseRepo first), so an
  // oddly-spelled repo may list itself among the fallers; the sentence
  // is still true, and the alternative is a second parse here that could
  // drift from that one.
  const firstAddWarning = (repo) => {
    if (targetRepos.length > 0) return null;
    const falling = repos.map((r) => r.repo).filter((r) => r !== repo);
    let msg = `Add ${repo} as the only repo this deployment allows?\n\n`
      + "This deployment doesn't restrict target repos today -- an empty allowlist is what means "
      + `unrestricted -- so adding ${repo} narrows it to that one repo rather than widening anything.`;
    if (falling.length > 0) {
      msg += `\n\nOff the allowlist as of this click: ${falling.join(", ")}. `
        + "Tasks already filed against them are unaffected, but the next one is parked instead of "
        + "dispatched until the repo is added back here.";
    }
    return msg;
  };

  const addRepo = async (evt) => {
    evt.preventDefault();
    const repo = newRepo.trim();
    if (repo === "") return;
    const warning = firstAddWarning(repo);
    if (warning !== null && !confirm(warning)) return;
    try {
      await api("/api/repos", { method: "POST", body: JSON.stringify({ repo }) });
      setNewRepo("");
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  const removeRepo = async (evt, repo) => {
    evt.stopPropagation();
    if (!confirm(`Remove ${repo} from target repos? Tasks that already target it are not affected, but new tasks won't be able to until it's added back.`)) return;
    try {
      const [owner, name] = repo.split("/");
      await api(`/api/repos/${owner}/${name}`, { method: "DELETE" });
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  const loadBranches = async (repo) => {
    try {
      const [owner, name] = repo.split("/");
      setBranches(await api(`/api/repos/${owner}/${name}/branches`));
    } catch (err) {
      showError(err);
    }
  };

  const toggleBranchForm = (evt, repo) => {
    evt.stopPropagation();
    if (branchRepo === repo) {
      setBranchRepo(null);
      return;
    }
    setBranchRepo(repo);
    setBranches([]);
    loadBranches(repo);
  };

  const loadCapabilities = async (repo) => {
    try {
      const [owner, name] = repo.split("/");
      const loaded = await api(`/api/repos/${owner}/${name}/capabilities`);
      setCaps(loaded);
      setCapsSelection(loaded.defaultCapabilities || []);
    } catch (err) {
      // Closed again rather than left on "Loading capabilities…": the
      // form has nothing to edit if this read failed, and the banner
      // showError raises is where the reason belongs.
      setCapsRepo(null);
      showError(err);
    }
  };

  const toggleCapabilitiesForm = (evt, repo) => {
    evt.stopPropagation();
    if (capsRepo === repo) {
      setCapsRepo(null);
      return;
    }
    setCapsRepo(repo);
    setCaps(null);
    setCapsSelection([]);
    loadCapabilities(repo);
  };

  // saveCapabilities replaces this repo's own set wholesale (PUT's whole
  // body is the new set, ui.SetRepoCapabilitiesRequest), then refreshes
  // the config the new-task form seeds its own picker from -- otherwise a
  // repo whose defaults just changed would keep filing tasks with the old
  // ones until the page was reloaded.
  const saveCapabilities = async (evt, repo) => {
    evt.preventDefault();
    try {
      const [owner, name] = repo.split("/");
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

  // createBranch only ever records the request -- the branches reconciler
  // (pkg/orchestrator.SyncBranches) is what actually creates it on GitHub,
  // typically within one cycle, so a freshly submitted name reappears
  // below still "pending" until the next loadBranches picks up "created"
  // or, if GitHub refused it, an error.
  const createBranch = async (evt, repo) => {
    evt.preventDefault();
    const form = evt.target;
    const name = form.elements.branchName.value.trim();
    if (name === "") return;
    try {
      const [owner, repoName] = repo.split("/");
      await api(`/api/repos/${owner}/${repoName}/branches`, { method: "POST", body: JSON.stringify({ name }) });
      form.reset();
      await loadBranches(repo);
    } catch (err) {
      showError(err);
    }
  };

  return (
    <main>
      <ListHeader
        title="Repos"
        count={visible.length}
        style={{ alignItems: "center" }}
        action={(
          <Stack component="form" direction="row" spacing={1} onSubmit={addRepo} sx={{ ml: "auto" }}>
            <TextField
              value={newRepo}
              onChange={(evt) => setNewRepo(evt.target.value)}
              placeholder="owner/name"
              size="small"
              autoComplete="off"
            />
            <Button type="submit" variant="outlined" size="small">Add repo</Button>
          </Stack>
        )}
      />
      {/* The standing half of grain/task-45's answer, keyed on the
          allowlist as it stands rather than on any transition: empty is
          the state in which "Add repo" is about to restrict rather than
          widen, and one entry is the state that leaves. Neither is an
          error or a warning -- both are working deployments -- so both
          are severity="info". */}
      {targetRepos.length === 0 && (
        <Alert severity="info" sx={{ mx: "1.75rem", mt: 2 }}>
          This deployment allows a task to target any repo: an empty allowlist is what means unrestricted.
          Adding the first repo here restricts it to that one repo rather than widening anything, and a task
          filed against any other repo is parked instead of dispatched until that repo is added too.
        </Alert>
      )}
      {targetRepos.length === 1 && (
        <Alert severity="info" sx={{ mx: "1.75rem", mt: 2 }}>
          This deployment allows only {targetRepos[0]}. A task filed against any other repo is parked instead
          of dispatched until that repo is added here; removing this one allows any repo again.
        </Alert>
      )}
      {repos.length > 0 && (
        <ListToolbar>
          <ListSearchField placeholder="Search repos…" value={search} onChange={setSearch} />
        </ListToolbar>
      )}
      <ul className="repo-list">
        {visible.map((r) => {
          const isOpen = expanded.has(r.repo);
          // A row with no tasks, off the allowlist, is on this page only
          // because the repo carries default capabilities of its own
          // (repoRows' third source). It has no counts to show and no
          // Remove to offer, so without saying so it would read as an
          // empty row nobody asked for -- and the Capabilities button
          // beside it is the only way to reach the set that put it here.
          const defaultsOnly = r.defaults && !r.configured && r.total === 0;
          return (
            <li key={r.repo}>
              <div className="repo-list-row" onClick={() => onOpenRepo(r.repo)}>
                <IconButton
                  size="small"
                  aria-label={isOpen ? `Hide tasks for ${r.repo}` : `Show tasks for ${r.repo}`}
                  onClick={(evt) => { evt.stopPropagation(); toggleExpanded(r.repo); }}
                >
                  {isOpen ? <ExpandMoreIcon fontSize="small" /> : <ChevronRightIcon fontSize="small" />}
                </IconButton>
                <span className="repo-list-name">{r.repo}</span>
                <span className="chips">
                  {/* Each chip here counts a repo's tasks in one state, not one
                      task's own status, so "running" keeps the plain CSS spin
                      (style.css's .badge-running) rather than StateDot's grain
                      mark (bwsalmon/agents#586) -- there is no single task for
                      the mark to represent. */}
                  {STATE_ORDER.filter((s) => r.counts[s]).map((s) => (
                    <Chip key={s} size="small" className={`badge badge-${s}`} label={`${STATE_LABELS[s]} ${r.counts[s]}`} />
                  ))}
                  {r.blocked > 0 && <Chip size="small" color="error" label={`Blocked ${r.blocked}`} />}
                  {defaultsOnly && (
                    <Chip
                      size="small"
                      variant="outlined"
                      label="Defaults only"
                      title="No tasks, and not on this deployment's target repos -- listed here because it has default capabilities of its own, which Capabilities edits."
                    />
                  )}
                </span>
                <Typography variant="caption" color="text.secondary" whiteSpace="nowrap">
                  {r.total} task{r.total === 1 ? "" : "s"}
                </Typography>
                <IconButton
                  size="small"
                  aria-label={`New task under ${r.repo}`}
                  title={`New task under ${r.repo}`}
                  onClick={(evt) => { evt.stopPropagation(); onNewTask(r.repo); }}
                >
                  <AddIcon fontSize="small" />
                </IconButton>
                <Button
                  size="small"
                  variant="outlined"
                  onClick={(evt) => toggleBranchForm(evt, r.repo)}
                >
                  New branch
                </Button>
                <Button
                  size="small"
                  variant="outlined"
                  onClick={(evt) => toggleCapabilitiesForm(evt, r.repo)}
                >
                  Capabilities
                </Button>
                <Button
                  size="small"
                  variant="outlined"
                  onClick={(evt) => { evt.stopPropagation(); onOpenReleases(r.repo); }}
                >
                  Releases
                </Button>
                {r.configured && (
                  <Button size="small" variant="outlined" color="error" onClick={(evt) => removeRepo(evt, r.repo)}>
                    Remove
                  </Button>
                )}
              </div>
              {branchRepo === r.repo && (
                <Box sx={{ px: "1.75rem", py: 1.25, borderTop: "1px solid", borderColor: "divider" }}>
                  <Stack component="form" direction="row" spacing={1} alignItems="flex-start" onSubmit={(evt) => createBranch(evt, r.repo)}>
                    <TextField
                      name="branchName" label="New branch name" placeholder="feature/foo"
                      helperText="Created from the repo's current default branch"
                      autoComplete="off" required InputLabelProps={{ required: false }} size="small"
                    />
                    <Button type="submit" variant="contained" size="small">Create branch</Button>
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
              )}
              {capsRepo === r.repo && (
                <Box sx={{ px: "1.75rem", py: 1.25, borderTop: "1px solid", borderColor: "divider" }}>
                  {caps === null ? (
                    <Typography variant="body2" color="text.secondary">Loading capabilities…</Typography>
                  ) : (
                    <Stack component="form" spacing={1} onSubmit={(evt) => saveCapabilities(evt, r.repo)}>
                      <FormControl fullWidth size="small">
                        <InputLabel id={`repo-capabilities-label-${r.repo.replace("/", "-")}`}>Default capabilities</InputLabel>
                        <Select
                          labelId={`repo-capabilities-label-${r.repo.replace("/", "-")}`}
                          label="Default capabilities"
                          multiple
                          value={capsSelection}
                          onChange={(e) => setCapsSelection(e.target.value)}
                          renderValue={(selected) => (
                            <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                              {selected.map((id) => (
                                <Chip key={id} size="small" label={capabilityName(config, id)} />
                              ))}
                            </Box>
                          )}
                        >
                          {/* The same grantable-only listing Settings'
                              own default-capabilities picker offers, and
                              for the same reason: a default no task can
                              be granted by hand would fail at every
                              filing, and PUT rejects one anyway. It
                              needs no filter of its own to say that,
                              unlike Settings' settings.capabilities:
                              config.capabilities *is*
                              ui.OfferedCapabilities, which is what
                              "grantable" means, where that one reports
                              every provider grain ships and flags the
                              ungrantable ones. A capability the
                              deployment already defaults is still
                              offered rather than hidden -- ticking it
                              here is how a repo keeps it if the
                              deployment-wide entry is later dropped --
                              and says so in its own row, so nobody reads
                              a blank box as "not on here". An id this
                              repo stored before the build retired it
                              gets a row too, purely so it can be
                              unticked (capabilityRows, state.js). */}
                          {capabilityRows(config?.capabilities, capsSelection).map((c) => (
                            <MenuItem key={c.id} value={c.id} title={c.description}>
                              <Checkbox checked={capsSelection.includes(c.id)} size="small" />
                              <ListItemText
                                primary={c.name}
                                secondary={c.retired
                                  ? c.description
                                  : (caps.deploymentDefaultCapabilities || []).includes(c.id)
                                    ? "already a deployment default -- on here either way"
                                    : null}
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
                        A task filed against {r.repo} starts with:{" "}
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
              )}
              {isOpen && (
                <ul className="task-sublist">
                  {tasks.filter((t) => t.repo === r.repo).map((t) => (
                    <li key={t.id}>
                      <TaskRow t={t} config={config} onOpenTask={onOpenTask} />
                    </li>
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      </ul>
      {repos.length === 0 && (
        <ListEmpty>No repos yet -- add one above, or file a task with a target repo.</ListEmpty>
      )}
      {repos.length > 0 && visible.length === 0 && (
        <ListEmpty>No repos match your search.</ListEmpty>
      )}
    </main>
  );
}
