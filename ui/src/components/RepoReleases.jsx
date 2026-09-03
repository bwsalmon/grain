import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert, Box, Button, Checkbox, Chip, FormControl, FormControlLabel,
  InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography,
} from "@mui/material";
import api from "../api.js";
import { STATE_LABELS } from "../state.js";
import StateDot, { isLiveRunning } from "./StateDot.jsx";

// POLL_INTERVAL_MS mirrors App.jsx's own poll: a candidate can move
// through "cutting"/"promoting", a release through
// "provisioning"/"merge_requested", and a qualification run's tasks
// through queued/running/completed, entirely server-side (graind
// dispatch, a run finishing), so without a poll this pane only ever
// moves when a human takes some action that happens to re-render it.
const POLL_INTERVAL_MS = 3000;

const RELEASE_STATUS_LABELS = {
  provisioning: "Provisioning",
  active: "Active",
  merge_requested: "Merge requested",
  merged: "Merged",
};

// RepoReleases is a single repo's release pane (bwsalmon/agents#571): pick
// or create a named release, then cut and promote its own rc candidates
// against the latest/rc/prod branches that name derives -- "myfeat" gives
// "myfeat.latest", "myfeat.rc.N" and "myfeat"; "2.1" gives "2.1.latest",
// "2.1.rc.N" and "2.1". This replaces bwsalmon/agents#398's own single
// prod/rc/prefix/major-version form, since a repo may now have any number
// of releases going at once, each independent.
//
// bwsalmon/agents#518's qualification plan editor and, for the current
// candidate, its qualification run's own summary are unchanged from
// before -- see QualificationPlanEditor and QualificationSummary below --
// since a plan is still one repo-wide policy, not a per-release one.
export default function RepoReleases({ repo, templates = [], onBack, showError }) {
  const [owner, name] = repo.split("/");
  const [releases, setReleases] = useState(null);
  const [selected, setSelected] = useState(null);
  const [candidates, setCandidates] = useState([]);
  const [qualificationPlan, setQualificationPlan] = useState(null);
  const [qualificationRun, setQualificationRun] = useState(null);
  const polling = useRef(false);
  const selectedRef = useRef(null);
  selectedRef.current = selected;

  const refresh = useCallback(async () => {
    try {
      const [list, plan] = await Promise.all([
        api(`/api/repos/${owner}/${name}/releases`),
        api(`/api/repos/${owner}/${name}/qualification-plan`),
      ]);
      setReleases(list);
      setQualificationPlan(plan);

      const current = selectedRef.current && list.some((r) => r.name === selectedRef.current)
        ? selectedRef.current
        : (list.length > 0 ? list[0].name : null);
      setSelected(current);

      if (current) {
        const cs = await api(`/api/repos/${owner}/${name}/releases/${current}/candidates`);
        setCandidates(cs);
        const currentCandidate = cs.length > 0 ? cs[0] : null;
        setQualificationRun(
          currentCandidate ? await api(`/api/repos/${owner}/${name}/candidates/${currentCandidate.id}/qualification`) : null
        );
      } else {
        setCandidates([]);
        setQualificationRun(null);
      }
    } catch (err) {
      showError(err);
    }
  }, [owner, name, showError]);

  useEffect(() => { refresh(); }, [refresh]);

  // Poll for the same reason App.jsx does: a release's or a candidate's
  // status, and a qualification run's tasks, all move server-side, with
  // no other event to tell this pane it should re-fetch. visibilitychange
  // catches up immediately on tab-back rather than waiting out a stale
  // interval.
  useEffect(() => {
    async function poll() {
      if (polling.current || document.visibilityState === "hidden") return;
      polling.current = true;
      try {
        await refresh();
      } finally {
        polling.current = false;
      }
    }
    const interval = setInterval(poll, POLL_INTERVAL_MS);
    const onVisible = () => {
      if (document.visibilityState === "visible") poll();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [refresh]);

  const createRelease = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const releaseName = form.elements.releaseName.value.trim();
    try {
      const created = await api(`/api/repos/${owner}/${name}/releases`, {
        method: "POST", body: JSON.stringify({ name: releaseName }),
      });
      form.reset();
      setSelected(created.name);
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const cut = async () => {
    try {
      await api(`/api/repos/${owner}/${name}/releases/${selected}/candidates`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const promote = async () => {
    try {
      await api(`/api/repos/${owner}/${name}/releases/${selected}/candidates/promote`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const requestMerge = async () => {
    try {
      await api(`/api/repos/${owner}/${name}/releases/${selected}/merge`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const savePlan = async (payload) => {
    try {
      await api(`/api/repos/${owner}/${name}/qualification-plan`, { method: "PUT", body: JSON.stringify(payload) });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const approveQualification = async (candidateId) => {
    try {
      await api(`/api/repos/${owner}/${name}/candidates/${candidateId}/qualification/approve`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  if (releases === null || qualificationPlan === null) return null;

  const current = releases.find((r) => r.name === selected) || null;
  const currentCandidate = candidates.length > 0 ? candidates[0] : null;
  const canCut = current && current.status === "active" && (!currentCandidate || currentCandidate.status === "promoted");
  const canPromote = current && current.status === "active" && currentCandidate && currentCandidate.status === "active";
  const canRequestMerge = current && current.status === "active";

  return (
    <main>
      <Box sx={{ px: "1.5rem" }}>
        {/* Back to the repo's own page, which is the only way in here
            now that the repo list's rows carry no buttons of their own
            (grain/task-111) -- so it names that repo rather than the
            list two steps up. */}
        <Button onClick={onBack} sx={{ mb: 1, ml: -0.9 }}>&larr; {repo}</Button>
        <Typography variant="h6" component="h2" sx={{ mt: 0 }}>{repo} releases</Typography>

        <form onSubmit={createRelease}>
          <Stack direction="row" spacing={1} alignItems="flex-start" sx={{ mt: 1 }}>
            <TextField
              name="releaseName" label="New release name" placeholder="myfeat, or 2.1"
              helperText="Gives myfeat.latest, myfeat.rc.N and myfeat" autoComplete="off" required
              InputLabelProps={{ required: false }} size="small"
            />
            <Button type="submit" variant="contained" size="small">Create release</Button>
          </Stack>
        </form>

        {releases.length === 0 && (
          <Alert severity="info" sx={{ mt: 2 }}>
            {repo} has no releases yet -- create one above to get a latest, rc and prod branch.
          </Alert>
        )}

        {releases.length > 0 && (
          <FormControl size="small" sx={{ mt: 2, minWidth: 240 }}>
            <InputLabel id="release-picker-label">Release</InputLabel>
            <Select
              labelId="release-picker-label" label="Release" value={selected || ""}
              onChange={(e) => setSelected(e.target.value)}
            >
              {releases.map((r) => (
                <MenuItem key={r.name} value={r.name}>
                  {r.name} -- {RELEASE_STATUS_LABELS[r.status] || r.status}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
        )}

        {current && (
          <Box sx={{ mt: 2 }}>
            <p className="hint">
              latest: {current.latestBranch}, prod: {current.prodBranch}
              {current.error && <span className="candidate-error"> ({current.error})</span>}
            </p>

            <Typography variant="subtitle1" sx={{ mt: 1 }}>Current candidate</Typography>
            {currentCandidate ? (
              <div className="candidate-current">
                <p>
                  <strong>{currentCandidate.branch}</strong> -- {currentCandidate.status}
                  {currentCandidate.error && <span className="candidate-error"> ({currentCandidate.error})</span>}
                </p>
              </div>
            ) : (
              <p className="empty">No release candidate cut yet.</p>
            )}
            <Stack direction="row" spacing={1} sx={{ mt: 1, mb: 2 }}>
              <Button variant="contained" disabled={!canCut} onClick={cut}>Cut new RC</Button>
              <Button variant="outlined" disabled={!canPromote} onClick={promote}>Promote current RC</Button>
            </Stack>

            {currentCandidate && (
              <QualificationSummary
                run={qualificationRun}
                onApprove={() => approveQualification(currentCandidate.id)}
              />
            )}

            <Typography variant="subtitle1" sx={{ mt: 2 }}>History</Typography>
            {candidates.length === 0 && <p className="empty">No candidates yet.</p>}
            {candidates.length > 0 && (
              <ul className="candidate-history">
                {candidates.map((c) => (
                  <li key={c.id}>
                    <strong>{c.branch}</strong> -- {c.status}
                  </li>
                ))}
              </ul>
            )}

            <Typography variant="subtitle1" sx={{ mt: 2 }}>Merge back to default</Typography>
            {current.status === "merged" ? (
              <p className="hint">
                Merge requested{current.pullRequestUrl ? <> -- <a href={current.pullRequestUrl} target="_blank" rel="noreferrer">pull request</a></> : "."}
              </p>
            ) : (
              <Stack direction="row" spacing={1} sx={{ mb: 2 }}>
                <Button variant="outlined" disabled={!canRequestMerge} onClick={requestMerge}>
                  Merge {current.prodBranch} into the default branch
                </Button>
              </Stack>
            )}
          </Box>
        )}

        <Typography variant="subtitle1" sx={{ mt: 3 }}>Qualification plan</Typography>
        <p className="hint">
          Templates a fresh, active release candidate schedules automatically -- run this many
          times each, in dependency order, against the candidate's own branch. Pick from any
          template declared under Templates.
        </p>
        <QualificationPlanEditor plan={qualificationPlan} templates={templates} onSave={savePlan} />
      </Box>
    </main>
  );
}

const QUALIFICATION_STATUS_LABELS = {
  pending_approval: "Pending approval",
  running: "Running",
  succeeded: "Succeeded",
  failed: "Failed",
};

const QUALIFICATION_STATUS_COLORS = {
  pending_approval: "warning",
  running: "info",
  succeeded: "success",
  failed: "error",
};

// QualificationSummary is the current candidate's own qualification
// progress: a status chip, an action or outcome banner, and every task
// instance a run has scheduled -- ordered failures first by the API, so
// the issue's own "show...any failures up front" needs no sort here.
// run is null both before a plan has ever scheduled one for this
// candidate and when the repo has no plan configured at all -- rendering
// nothing either way, since there is nothing yet to summarize.
function QualificationSummary({ run, onApprove }) {
  if (!run) return null;
  const label = QUALIFICATION_STATUS_LABELS[run.status] || run.status;
  const color = QUALIFICATION_STATUS_COLORS[run.status] || "default";

  return (
    <Box sx={{ mb: 2 }}>
      <Stack direction="row" spacing={1} alignItems="center">
        <Typography variant="subtitle1" sx={{ mt: 0 }}>Qualification</Typography>
        <Chip size="small" color={color} label={label} />
      </Stack>
      {run.status === "pending_approval" && (
        <Alert severity="warning" sx={{ mt: 1 }} action={<Button size="small" onClick={onApprove}>Approve all</Button>}>
          This candidate's qualification tasks need approval before any of them can run.
        </Alert>
      )}
      {run.status === "succeeded" && (
        <Alert severity="success" sx={{ mt: 1 }}>Qualification passed -- ready to promote.</Alert>
      )}
      {run.status === "failed" && (
        <Alert severity="error" sx={{ mt: 1 }}>Qualification failed -- see below.</Alert>
      )}
      <ul className="qualification-task-list">
        {run.tasks.map((t) => (
          <li key={t.taskId}>
            <span
              className={`badge badge-icon badge-${t.state}${isLiveRunning(t.state) ? " badge-mark" : ""}`}
              title={STATE_LABELS[t.state] || t.state}
            >
              <StateDot state={t.state} title={STATE_LABELS[t.state] || t.state} />
            </span>
            {t.templateName}
            {t.repeat > 1 && <span className="hint"> ({t.instanceIndex}/{t.repeat})</span>}
            {!t.approved && <Chip size="small" label="Unapproved" sx={{ ml: 1 }} />}
          </li>
        ))}
      </ul>
    </Box>
  );
}

function emptyItem() {
  return { templateId: "", repeat: 1, dependsOn: [] };
}

// QualificationPlanEditor is the whole-plan form: the two switches
// bwsalmon/agents#518 asks for, and every item, added, edited and
// removed in place -- PutQualificationPlan replaces the whole plan
// wholesale on Save, so there is no separate per-item save action.
//
// Each item's own content -- title, body, reads, capabilities -- comes
// entirely from the template it names (bwsalmon/agents#516); this form
// only ever picks which template, how many times, and what it waits on,
// the same "template takes over the content fields" split ScheduleOverlay's
// own picker already draws.
function QualificationPlanEditor({ plan, templates, onSave }) {
  const [requireApproval, setRequireApproval] = useState(plan.requireApproval);
  const [autoPromote, setAutoPromote] = useState(plan.autoPromote);
  const [items, setItems] = useState(
    plan.items.map((it) => ({ templateId: it.templateId, repeat: it.repeat, dependsOn: it.dependsOn || [] }))
  );

  const updateItem = (i, patch) => {
    setItems((prev) => prev.map((it, idx) => (idx === i ? { ...it, ...patch } : it)));
  };
  const removeItem = (i) => {
    setItems((prev) => prev.filter((_, idx) => idx !== i));
  };
  const addItem = () => {
    setItems((prev) => [...prev, emptyItem()]);
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const payload = {
      requireApproval,
      autoPromote,
      items: items
        .filter((it) => it.templateId !== "")
        .map((it) => ({ templateId: it.templateId, repeat: Number(it.repeat) || 1, dependsOn: it.dependsOn || [] })),
    };
    await onSave(payload);
  };

  const templateName = (id) => (templates.find((tmpl) => tmpl.id === id) || {}).name || id;

  return (
    <form onSubmit={submit}>
      <FormControlLabel
        control={<Checkbox checked={requireApproval} onChange={(e) => setRequireApproval(e.target.checked)} />}
        label="Require approval before running qualification tasks"
        sx={{ display: "flex", mt: 1 }}
      />
      <FormControlLabel
        control={<Checkbox checked={autoPromote} onChange={(e) => setAutoPromote(e.target.checked)} />}
        label="Promote automatically once qualification succeeds"
        sx={{ display: "flex" }}
      />

      {templates.length === 0 && (
        <Alert severity="info" sx={{ mt: 2 }}>
          No templates exist yet -- create one under Templates first.
        </Alert>
      )}

      {items.map((it, i) => {
        const otherItems = items.filter((_, idx) => idx !== i && items[idx].templateId !== "");
        return (
          <Box key={i} className="qualification-item-row" sx={{ border: "1px solid", borderColor: "divider", borderRadius: 1, p: 1.5, mt: 1.5 }}>
            <Stack direction="row" spacing={1} alignItems="flex-start">
              <FormControl size="small" sx={{ flex: 2 }}>
                <InputLabel id={`qualification-template-label-${i}`}>Template</InputLabel>
                <Select
                  labelId={`qualification-template-label-${i}`}
                  label="Template"
                  value={it.templateId}
                  onChange={(e) => updateItem(i, { templateId: e.target.value })}
                >
                  <MenuItem value="" disabled>Choose a template</MenuItem>
                  {templates.map((tmpl) => (
                    <MenuItem key={tmpl.id} value={tmpl.id}>{tmpl.name}</MenuItem>
                  ))}
                </Select>
              </FormControl>
              <TextField
                label="Repeat" type="number" inputProps={{ min: 1 }} value={it.repeat}
                onChange={(e) => updateItem(i, { repeat: e.target.value })} size="small" sx={{ width: 90 }}
              />
            </Stack>
            <FormControl fullWidth size="small" margin="dense">
              <InputLabel id={`qualification-dependson-label-${i}`}>Depends on</InputLabel>
              <Select
                labelId={`qualification-dependson-label-${i}`}
                label="Depends on"
                multiple
                value={it.dependsOn}
                onChange={(e) => updateItem(i, { dependsOn: e.target.value })}
                renderValue={(selected) => (
                  <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                    {selected.map((id) => <Chip key={id} size="small" label={templateName(id)} />)}
                  </Box>
                )}
              >
                {otherItems.length === 0 && <MenuItem disabled value="">No other items yet</MenuItem>}
                {otherItems.map((other) => (
                  <MenuItem key={other.templateId} value={other.templateId}>
                    <Checkbox checked={it.dependsOn.includes(other.templateId)} size="small" />
                    <ListItemText primary={templateName(other.templateId)} />
                  </MenuItem>
                ))}
              </Select>
            </FormControl>
            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 0.5 }}>
              <Button size="small" color="error" onClick={() => removeItem(i)}>Remove</Button>
            </Stack>
          </Box>
        );
      })}

      <Stack direction="row" spacing={1} sx={{ mt: 2 }}>
        <Button size="small" variant="outlined" onClick={addItem} disabled={templates.length === 0}>Add item</Button>
        <Button size="small" type="submit" variant="contained">Save qualification plan</Button>
      </Stack>
    </form>
  );
}
