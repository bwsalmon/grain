import { useState } from "react";
import { Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// TemplateOverlay is the templates page's own sub page (bwsalmon/
// agents#545): the "+" on TemplatesList opens this blank, and clicking
// an existing row opens the same overlay pre-filled with that
// template's fields, the same NewTaskOverlay/DetailOverlay split
// between a flat list of key details and everything about one item
// living behind a click rather than crowding the list -- so templates
// stop being the one list on this page that also carries an always-open
// form of its own.
//
// No target repo or branch here: a template carries no target of its
// own (model.TaskTemplate's own doc comment on why) -- whatever fires
// from this template (ScheduleOverlay.jsx, SuiteRunOverlay.jsx) asks for
// a repo and branch of its own at the point of use instead.
export default function TemplateOverlay({ template, config, onClose, onSaved, showError }) {
  const isNew = !template;
  const [capabilities, setCapabilities] = useState(template?.capabilities || []);

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
      autoMerge: form.elements.autoMerge.checked,
      reads,
      capabilities,
    };
    try {
      const saved = isNew
        ? await api("/api/templates", { method: "POST", body: JSON.stringify(payload) })
        : await api(`/api/templates/${template.id}`, { method: "PATCH", body: JSON.stringify(payload) });
      await onSaved(saved);
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete the template "${template.name}"? Schedules using it must be repointed or deleted first.`)) return;
    try {
      await api(`/api/templates/${template.id}`, { method: "DELETE" });
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose} pane>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>{isNew ? "New template" : "Edit template"}</Typography>
      <form className="pane-form" onSubmit={submit}>
        <TextField name="name" label="Name" defaultValue={template?.name} required InputLabelProps={{ required: false }} helperText="shown wherever a template is picked" autoComplete="off" fullWidth margin="normal" />
        <TextField name="title" label="Task title" defaultValue={template?.title} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />
        <TextField name="description" label="Description" defaultValue={template?.description} multiline rows={4} fullWidth margin="normal" />
        <TextField
          name="reads"
          label="Read-only repos"
          defaultValue={(template?.reads || []).join(", ")}
          helperText="owner/name, comma-separated, optional"
          placeholder="owner/shared-lib, owner/schema"
          autoComplete="off"
          fullWidth
          margin="normal"
        />
        <FormControlLabel
          control={<Checkbox name="autoMerge" defaultChecked={template?.autoMerge} />}
          label="Auto-merge once checks pass"
          sx={{ display: "flex", mt: 1 }}
        />
        <FormControl fullWidth margin="normal" size="small">
          <InputLabel id="template-capabilities-label">Capabilities</InputLabel>
          <Select
            labelId="template-capabilities-label"
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
        <Stack direction="row" justifyContent={isNew ? "flex-end" : "space-between"} alignItems="center" sx={{ mt: 2 }}>
          {!isNew && <Button color="error" onClick={remove}>Delete</Button>}
          <Stack direction="row" spacing={1}>
            <Button onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="contained">{isNew ? "Add template" : "Save"}</Button>
          </Stack>
        </Stack>
      </form>
    </Overlay>
  );
}
