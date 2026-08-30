import { useRef, useState } from "react";
import { Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import fileToAttachment from "../attachments.js";
import { knownRepos } from "../state.js";
import AttachmentPicker from "./AttachmentPicker.jsx";
import Overlay from "./Overlay.jsx";
import RepoField from "./RepoField.jsx";
import TaskPicker from "./TaskPicker.jsx";

export default function NewTaskOverlay({ tasks, config, defaultRepo, onClose, onCreated, showError }) {
  const formRef = useRef(null);
  const repoOptions = knownRepos(config, tasks);
  // dependsOn is picked tasks ({id, title}), not just ids -- keeping the
  // title lets the chips below the picker read as "task 12 Fix the
  // thing" instead of a bare number nobody can place.
  const [dependsOn, setDependsOn] = useState([]);
  const [capabilities, setCapabilities] = useState([]);
  // attachments is File objects, not yet read -- AttachmentPicker's own
  // doc comment on why that read is deferred to submit.
  const [attachments, setAttachments] = useState([]);

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
      repo: data.get("repo") || "",
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      sandboxCpus: parseInt(data.get("sandboxCpus"), 10) || 0,
      sandboxMemoryMb: parseInt(data.get("sandboxMemoryMb"), 10) || 0,
      capabilities,
      dependsOn: dependsOn.map((t) => t.id),
      reads,
      approved: form.elements.approved.checked,
      attachments: await Promise.all(attachments.map((f) => fileToAttachment(f))),
    };
    try {
      await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      setDependsOn([]);
      setCapabilities([]);
      setAttachments([]);
      onClose();
      await onCreated();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>New task</Typography>
      <form ref={formRef} onSubmit={submit}>
        <TextField name="title" label="Title" required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />
        <TextField name="description" label="Description" multiline rows={5} fullWidth margin="normal" />
        <AttachmentPicker files={attachments} onChange={setAttachments} />
        <Box component="label" sx={{ display: "block", mt: 2, mb: 1 }}>
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
            Target repo <span className="hint">owner/name, optional</span>
          </Typography>
          {/* Pre-filled from the repo the "+ New task" button was opened
              from -- the repo-centric task list's whole point is filing
              work against the repo you're already looking at without
              retyping it. */}
          <RepoField name="repo" options={repoOptions} defaultValue={defaultRepo || ""} />
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
        <FormControlLabel
          control={<Checkbox name="approved" />}
          label="Queue immediately (unchecked files it as a proposal, needing approval)"
          sx={{ display: "flex", mt: 1 }}
        />
        <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
          <Button type="submit" variant="contained">Create task</Button>
        </Stack>
      </form>
    </Overlay>
  );
}
