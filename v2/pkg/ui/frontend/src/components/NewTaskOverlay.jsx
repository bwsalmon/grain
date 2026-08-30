import { useRef, useState } from "react";
import { Box, Button, Checkbox, Chip, FormControlLabel, FormGroup, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { knownRepos } from "../state.js";
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
    const capabilities = (config?.capabilities || [])
      .filter((c) => form.elements["cap-" + c.id] && form.elements["cap-" + c.id].checked)
      .map((c) => c.id);
    const reads = (data.get("reads") || "")
      .split(",").map((repo) => repo.trim()).filter((repo) => repo !== "");
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo") || "",
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      capabilities,
      dependsOn: dependsOn.map((t) => t.id),
      reads,
      approved: form.elements.approved.checked,
    };
    try {
      await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      setDependsOn([]);
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
          <legend>Capabilities</legend>
          <FormGroup>
            {(config?.capabilities || []).map((c) => (
              <FormControlLabel
                key={c.id}
                title={c.description}
                control={<Checkbox name={"cap-" + c.id} />}
                label={c.name}
              />
            ))}
          </FormGroup>
        </fieldset>
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
