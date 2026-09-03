import { useRef, useState } from "react";
import { Accordion, AccordionDetails, AccordionSummary, Box, Button, Checkbox, Chip, FormControl, FormControlLabel, FormHelperText, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Tooltip, Typography } from "@mui/material";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import api from "../api.js";
import fileToAttachment from "../attachments.js";
import { frameworkLabel, knownRepos, lastBaseForRepo, suggestsBase } from "../state.js";
import AttachmentPicker from "./AttachmentPicker.jsx";
import Overlay from "./Overlay.jsx";
import RepoField from "./RepoField.jsx";
import TaskPicker from "./TaskPicker.jsx";

export default function NewTaskOverlay({ tasks, config, defaultRepo, onClose, onCreated, onOpenTask, showError }) {
  const formRef = useRef(null);
  const repoOptions = knownRepos(config, tasks);
  // title and repo are lifted to state, unlike most of this form's other
  // text fields, because the Create button's disabled state (below) has
  // to read their live value on every keystroke rather than only at
  // submit.
  const [title, setTitle] = useState("");
  const [repo, setRepo] = useState(defaultRepo || "");
  // noRepo is the explicit "this task has no repo" choice
  // (bwsalmon/agents#614): distinct from repo simply being blank, which
  // used to be the only way to skip it and silently fell back to the
  // deployment default (or failed validation with no warning until
  // submit). Checking it hides RepoField outright, so its <select>/
  // <input> is never in the DOM to be submitted -- payload.repo comes
  // back "" the same way an unset field always has, with noRepo now
  // saying why.
  const [noRepo, setNoRepo] = useState(false);
  // base tracks Base branch as state, unlike most text fields on this
  // form, so picking a repo (below) can prefill it from
  // lastBaseForRepo without fighting an uncontrolled <input>'s own
  // defaultValue (bwsalmon/agents#641).
  const [base, setBase] = useState(() => lastBaseForRepo(tasks, defaultRepo || ""));
  // baseEdited tracks whether the human has typed into Base branch
  // themselves, so handleRepoChange's prefill (below) never clobbers a
  // value they already chose -- only ever a prefill from a previous repo
  // pick, or the field's own initial empty state.
  const baseEdited = useRef(false);
  // dependsOn is picked tasks ({id, title}), not just ids -- keeping the
  // title lets the chips below the picker read as "task 12 Fix the
  // thing" instead of a bare number nobody can place.
  const [dependsOn, setDependsOn] = useState([]);
  // capabilities starts as whatever this deployment attaches to every
  // new task (GET /api/config's defaultCapabilities, model.Config's own
  // field of the same name) -- ticked, in the picker below, so it is
  // visible before the task is filed and can be unticked here rather
  // than detached afterwards. The payload always names the resulting
  // list, so what is ticked when Create is clicked is exactly what the
  // task is filed with; the server only falls back to its own defaults
  // for a caller that names no list at all (ui.CreateTaskRequest.
  // Capabilities).
  const [capabilities, setCapabilities] = useState(() => config?.defaultCapabilities || []);
  // attachments is File objects, not yet read -- AttachmentPicker's own
  // doc comment on why that read is deferred to submit.
  const [attachments, setAttachments] = useState([]);
  // interactive is lifted to state, unlike autoMerge/approved below,
  // because whether "Queue immediately" even makes sense to show depends
  // on its live value (bwsalmon/agents#539: an interactive task always
  // queues at once, so that checkbox would just be a second control for
  // the same fact).
  const [interactive, setInteractive] = useState(false);

  // handleRepoChange prefills Base branch from whatever the newly-picked
  // repo's own last task used (bwsalmon/agents#641), rather than
  // clobbering something the human already typed (baseEdited.current) or
  // a repo with no history to prefill from. That "no history" check is
  // gated on whether the repo has any task suggestsBase counts at all,
  // not on lastBaseForRepo's return value -- that value is "" both when
  // there is no history and when the most recent task deliberately used
  // the default branch, and only the former should leave a
  // manually-typed base alone.
  const handleRepoChange = (r) => {
    setRepo(r);
    if (baseEdited.current) return;
    const hasHistory = (tasks || []).some((t) => t.repo === r && suggestsBase(t));
    if (hasHistory) setBase(lastBaseForRepo(tasks, r));
  };

  const addDependency = (t) => {
    setDependsOn((prev) => (prev.some((p) => p.id === t.id) ? prev : [...prev, t]));
  };
  const removeDependency = (id) => {
    setDependsOn((prev) => prev.filter((p) => p.id !== id));
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const reads = (data.get("reads") || "")
      .split(",").map((repo) => repo.trim()).filter((repo) => repo !== "");
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: noRepo ? "" : (data.get("repo") || ""),
      noRepo,
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      sandboxCpus: parseInt(data.get("sandboxCpus"), 10) || 0,
      sandboxMemoryMb: parseInt(data.get("sandboxMemoryMb"), 10) || 0,
      // "" is the deployment default, not a framework: the server reads
      // an empty agentFramework as "whichever one this deployment is set
      // to when the task dispatches" (model.Task.AgentFramework).
      agentFramework: data.get("agentFramework") || "",
      capabilities,
      dependsOn: dependsOn.map((t) => t.id),
      reads,
      // form.elements.approved does not exist once interactive has
      // hidden it -- interactive queues immediately regardless (see the
      // checkbox below), so there is nothing to read from the form in
      // that case anyway.
      approved: interactive || form.elements.approved.checked,
      interactive,
      attachments: await Promise.all(attachments.map((f) => fileToAttachment(f))),
    };
    try {
      const task = await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      setTitle("");
      setRepo(defaultRepo || "");
      setNoRepo(false);
      setBase(lastBaseForRepo(tasks, defaultRepo || ""));
      setDependsOn([]);
      setCapabilities(config?.defaultCapabilities || []);
      setAttachments([]);
      setInteractive(false);
      onClose();
      await onCreated();
      // An interactive task's whole point is the chat, not the task
      // list it was just filed from -- open it straight away rather
      // than making whoever asked for a live conversation go find it
      // (bwsalmon/agents#539).
      if (payload.interactive) {
        onOpenTask(task.id);
      }
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>New task</Typography>
      <form ref={formRef} onSubmit={submit}>
        <TextField
          name="title"
          label="Title"
          required
          InputLabelProps={{ required: false }}
          autoComplete="off"
          fullWidth
          margin="normal"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
        />
        <TextField name="description" label="Description" multiline rows={5} fullWidth margin="normal" />
        <AttachmentPicker files={attachments} onChange={setAttachments} />
        <Box sx={{ mt: 2, mb: 1 }}>
          <Box component="label" sx={{ display: "block", mb: 0.5 }}>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
              Target repo <span className="hint">owner/name, required</span>
            </Typography>
            {/* Pre-filled from the repo the "+ New task" button was
                opened from -- the repo-centric task list's whole point
                is filing work against the repo you're already looking
                at without retyping it. Hidden once "No repo" is
                checked, so its <select>/<input> never submits a stray
                value alongside noRepo. */}
            {!noRepo && (
              <RepoField name="repo" options={repoOptions} defaultValue={defaultRepo || ""} onChange={handleRepoChange} />
            )}
          </Box>
          <FormControlLabel
            control={
              <Checkbox
                checked={noRepo}
                onChange={(e) => { setNoRepo(e.target.checked); if (e.target.checked) setRepo(""); }}
              />
            }
            label="No repo (standalone task -- nothing to check out)"
            sx={{ display: "flex" }}
          />
        </Box>
        <TextField
          name="base"
          label="Base branch"
          helperText="optional"
          placeholder="main"
          autoComplete="off"
          fullWidth
          margin="normal"
          value={base}
          onChange={(e) => { baseEdited.current = true; setBase(e.target.value); }}
        />
        <TextField
          name="reads"
          label="Read-only repos"
          helperText="owner/name, comma-separated, optional"
          placeholder="owner/shared-lib, owner/schema"
          autoComplete="off"
          fullWidth
          margin="normal"
        />
        <FormControlLabel
          control={<Checkbox name="autoMerge" defaultChecked={!!config?.autoMergeByDefault} />}
          label="Auto-merge once checks pass"
          sx={{ display: "flex", mt: 1 }}
        />
        <FormControl fullWidth margin="normal" size="small">
          <InputLabel id="new-task-capabilities-label">Capabilities</InputLabel>
          <Select
            labelId="new-task-capabilities-label"
            label="Capabilities"
            multiple
            value={capabilities}
            onChange={(e) => setCapabilities(e.target.value)}
            renderValue={(selected) => (
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5 }}>
                {selected.map((id) => {
                  const c = (config?.capabilities || []).find((cap) => cap.id === id);
                  return <Chip key={id} size="small" label={c ? c.name : id} />;
                })}
              </Box>
            )}
          >
            {(config?.capabilities || []).map((c) => (
              <MenuItem key={c.id} value={c.id} title={c.description}>
                <Checkbox checked={capabilities.includes(c.id)} size="small" />
                <ListItemText primary={c.name} />
              </MenuItem>
            ))}
          </Select>
          {(config?.defaultCapabilities || []).length > 0 && (
            <FormHelperText>
              Pre-ticked ones are this deployment&apos;s defaults (Settings &gt; Capabilities) -- untick any this
              task should not have.
            </FormHelperText>
          )}
        </FormControl>
        <fieldset>
          <legend>Depends on <span className="hint">optional</span></legend>
          {dependsOn.length > 0 && (
            // One full-width chip per line, the shape DetailOverlay's
            // dependency list uses: a picked task then reads as a row,
            // rather than a bubble sized by however long its title
            // happens to be, wrapped in beside the others.
            <Stack spacing={0.6} sx={{ mb: 1 }}>
              {dependsOn.map((t) => (
                <Tooltip key={t.id} title={`${t.id} ${t.title}`} placement="left">
                  <Chip
                    label={`${t.id} ${t.title}`}
                    onDelete={() => removeDependency(t.id)}
                    deleteIcon={<span title={`Remove dependency on ${t.id}`}>×</span>}
                    sx={{ width: "100%", justifyContent: "space-between", "& .MuiChip-label": { flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis" } }}
                  />
                </Tooltip>
              ))}
            </Stack>
          )}
          <TaskPicker
            tasks={tasks || []}
            exclude={dependsOn.map((t) => t.id)}
            onPick={addDependency}
            placeholder="Search tasks to depend on…"
          />
        </fieldset>
        <Accordion disableGutters sx={{ mt: 2 }}>
          <AccordionSummary expandIcon={<ExpandMoreIcon />} aria-controls="new-task-advanced-content" id="new-task-advanced-header">
            <Typography>Advanced options</Typography>
          </AccordionSummary>
          <AccordionDetails>
            <FormControlLabel
              control={<Checkbox checked={interactive} onChange={(e) => setInteractive(e.target.checked)} />}
              label="Interactive session (open a live chat here instead of running in the background)"
              sx={{ display: "flex" }}
            />
            <TextField
              select
              name="agentFramework"
              label="Agent framework"
              defaultValue=""
              // displayEmpty: "" is a real choice here (the deployment's
              // own framework), not the absence of one, so the closed
              // select has to show its label rather than a blank box.
              SelectProps={{ displayEmpty: true }}
              helperText="which agent drives this one task -- set the deployment's own default, and each framework's key, in Settings"
              fullWidth
              margin="normal"
              size="small"
            >
              <MenuItem value="">
                {`Deployment default${config && config.agentFramework ? ` (${frameworkLabel(config.agentFramework)})` : ""}`}
              </MenuItem>
              <MenuItem value="antigravity">Antigravity</MenuItem>
              <MenuItem value="claude">Claude</MenuItem>
            </TextField>
            <fieldset>
              <legend>Sandbox shape override <span className="hint">optional, kontur-managed deployments only</span></legend>
              <TextField
                name="sandboxCpus"
                label="vCPUs"
                helperText="blank/0 uses the deployment default"
                type="number"
                inputProps={{ min: 0, step: 1 }}
                autoComplete="off"
                fullWidth
                margin="normal"
                size="small"
              />
              <TextField
                name="sandboxMemoryMb"
                label="Memory (MiB)"
                helperText="blank/0 uses the deployment default"
                type="number"
                inputProps={{ min: 0, step: 1 }}
                autoComplete="off"
                fullWidth
                margin="normal"
                size="small"
              />
            </fieldset>
          </AccordionDetails>
        </Accordion>
        {interactive ? (
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 1 }}>
            Interactive sessions always queue immediately.
          </Typography>
        ) : (
          <FormControlLabel
            control={<Checkbox name="approved" defaultChecked={!!config?.approvedByDefault} />}
            label="Queue immediately (unchecked files it as a proposal, needing approval)"
            sx={{ display: "flex", mt: 1 }}
          />
        )}
        <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
          <Button type="submit" variant="contained" disabled={!title.trim() || (!noRepo && !repo.trim())}>
            Create task
          </Button>
        </Stack>
      </form>
    </Overlay>
  );
}
