import { useState } from "react";
import { Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { knownRepos } from "../state.js";
import RepoField from "./RepoField.jsx";

// TemplatesList is the templates pane (bwsalmon/agents#516): SchedulesList's
// own list-plus-form shape, minus everything about firing on a cadence --
// a template is never itself something that runs, only something a
// schedule (ScheduleForm's own "Template" picker) or a future caller
// fires from. Kept as its own pane, alongside Scheduled tasks, rather
// than folded into it, since a template is meant to be reusable across
// more than one schedule and does not belong to any single one of them.
export default function TemplatesList({ templates, config, onRefresh, showError }) {
  const [editingId, setEditingId] = useState(null);
  const repoOptions = knownRepos(config, []);

  const remove = async (tmpl) => {
    if (!confirm(`Delete the template "${tmpl.name}"? Schedules using it must be repointed or deleted first.`)) return;
    try {
      await api(`/api/templates/${tmpl.id}`, { method: "DELETE" });
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  const create = async (payload) => {
    try {
      await api("/api/templates", { method: "POST", body: JSON.stringify(payload) });
      await onRefresh();
      return true;
    } catch (err) {
      showError(err);
      return false;
    }
  };

  const save = async (tmpl, payload) => {
    try {
      await api(`/api/templates/${tmpl.id}`, { method: "PATCH", body: JSON.stringify(payload) });
      setEditingId(null);
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <main>
      <Box sx={{ px: "1.5rem" }}>
        <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Task templates</Typography>
        <ul className="schedules-list">
          {templates.map((tmpl) => (
            <li className="schedule-row" key={tmpl.id}>
              {editingId === tmpl.id ? (
                <TemplateForm
                  template={tmpl}
                  repoOptions={repoOptions}
                  config={config}
                  submitLabel="Save"
                  onCancel={() => setEditingId(null)}
                  onSubmit={(payload) => save(tmpl, payload)}
                />
              ) : (
                <>
                  <div className="schedule-summary">
                    <span className="schedule-title">{tmpl.name}</span>
                    <Chip size="small" label={tmpl.repo} />
                  </div>
                  <div className="schedule-meta hint">{tmpl.title}</div>
                  <Stack direction="row" spacing={1} sx={{ mt: 0.5 }}>
                    <Button size="small" variant="outlined" onClick={() => setEditingId(tmpl.id)}>
                      Edit
                    </Button>
                    <Button size="small" variant="outlined" color="error" onClick={() => remove(tmpl)}>
                      Delete
                    </Button>
                  </Stack>
                </>
              )}
            </li>
          ))}
        </ul>
        {templates.length === 0 && <p className="empty">No task templates.</p>}

        <Typography variant="subtitle1" sx={{ mt: 2 }}>New template</Typography>
        <TemplateForm repoOptions={repoOptions} config={config} submitLabel="Add template" onSubmit={create} isNew />
      </Box>
    </main>
  );
}

// TemplateForm is the create-a-template form and the per-row edit form,
// the same component either way -- ScheduleForm's own shape
// (SchedulesList.jsx), minus RecurrenceFields: a template carries no
// cadence of its own.
function TemplateForm({ template, repoOptions, config, submitLabel, onSubmit, onCancel, isNew }) {
  const [capabilities, setCapabilities] = useState(template?.capabilities || []);
  const margin = isNew ? "normal" : "dense";

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const reads = (data.get("reads") || "")
      .split(",").map((r) => r.trim()).filter((r) => r !== "");

    const payload = {
      name: data.get("name"),
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo") || "",
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      reads,
      capabilities,
    };
    const ok = await onSubmit(payload);
    if (isNew && ok !== false) {
      form.reset();
      setCapabilities([]);
    }
  };

  return (
    <form onSubmit={submit}>
      <TextField name="name" label="Name" defaultValue={template?.name} required InputLabelProps={{ required: false }} helperText="shown wherever a template is picked" autoComplete="off" fullWidth margin={margin} />
      <TextField name="title" label="Task title" defaultValue={template?.title} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin={margin} />
      <TextField name="description" label="Description" defaultValue={template?.description} multiline rows={isNew ? 4 : 3} fullWidth margin={margin} />
      <Box component="label" sx={{ display: "block", mt: isNew ? 2 : 1.5, mb: 1 }}>
        <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
          Target repo <span className="hint">owner/name</span>
        </Typography>
        <RepoField name="repo" options={repoOptions} defaultValue={template?.repo || ""} required />
      </Box>
      <TextField name="base" label="Base branch" defaultValue={template?.base} helperText="optional" placeholder="main" autoComplete="off" fullWidth margin={margin} />
      <TextField
        name="reads"
        label="Read-only repos"
        defaultValue={(template?.reads || []).join(", ")}
        helperText="owner/name, comma-separated, optional"
        placeholder="owner/shared-lib, owner/schema"
        autoComplete="off"
        fullWidth
        margin={margin}
      />
      <FormControlLabel
        control={<Checkbox name="autoMerge" defaultChecked={template?.autoMerge} />}
        label="Auto-merge once checks pass"
        sx={{ display: "flex", mt: 1 }}
      />
      <FormControl fullWidth margin={margin} size="small">
        <InputLabel id={`template-capabilities-label-${template?.id || "new"}`}>Capabilities</InputLabel>
        <Select
          labelId={`template-capabilities-label-${template?.id || "new"}`}
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
      <Stack direction="row" spacing={1} justifyContent="flex-end" sx={{ mt: 2 }}>
        {onCancel && <Button size="small" onClick={onCancel}>Cancel</Button>}
        <Button size={isNew ? "medium" : "small"} type="submit" variant="contained">{submitLabel}</Button>
      </Stack>
    </form>
  );
}
