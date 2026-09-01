import { useRef, useState } from "react";
import { Accordion, AccordionDetails, AccordionSummary, Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import api from "../api.js";
import fileToAttachment from "../attachments.js";
import { knownRepos } from "../state.js";
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
  // dependsOn is picked tasks ({id, title}), not just ids -- keeping the
  // title lets the chips below the picker read as "task 12 Fix the
  // thing" instead of a bare number nobody can place.
  const [dependsOn, setDependsOn] = useState([]);
  const [capabilities, setCapabilities] = useState([]);
  // attachments is File objects, not yet read -- AttachmentPicker's own
  // doc comment on why that read is deferred to submit.
  const [attachments, setAttachments] = useState([]);
  // interactive is lifted to state, unlike autoMerge/approved below,
  // because whether "Queue immediately" even makes sense to show depends
  // on its live value (bwsalmon/agents#539: an interactive task always
  // queues at once, so that checkbox would just be a second control for
  // the same fact).
  const [interactive, setInteractive] = useState(false);

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
      setDependsOn([]);
      setCapabilities([]);
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
              <RepoField name="repo" options={repoOptions} defaultValue={defaultRepo || ""} onChange={setRepo} />
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
        <TextField name="base" label="Base branch" helperText="optional" placeholder="main" autoComplete="off" fullWidth margin="normal" />
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
          control={<Checkbox name="autoMerge" />}
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
        </FormControl>
        <fieldset>
          <legend>Depends on <span className="hint">optional</span></legend>
          {dependsOn.length > 0 && (
            <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.6, mb: 1 }}>
              {dependsOn.map((t) => (
                <Chip
                  key={t.id}
                  size="small"
                  label={`${t.id} ${t.title}`}
                  onDelete={() => removeDependency(t.id)}
                  deleteIcon={<span title={`Remove dependency on ${t.id}`}>×</span>}
                />
              ))}
            </Box>
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
            control={<Checkbox name="approved" />}
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
