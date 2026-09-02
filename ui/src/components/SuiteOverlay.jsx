import { useState } from "react";
import { Box, Button, Checkbox, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import TemplateOverlay from "./TemplateOverlay.jsx";

// SuiteOverlay is the task suites page's own sub page (bwsalmon/
// agents#642), TemplateOverlay's own "+ on the list opens this blank,
// clicking a row opens it pre-filled" split: a task suite is a saved
// combination of task templates plus how to run them (run them Count
// times, or run them until a pass produces no pull request and no
// follow-up task), created here and then run against any repo and
// branch from SuiteRunOverlay.
//
// A suite is useless with no templates, so building one from scratch
// would otherwise mean leaving this overlay for the templates page and
// back -- "+ New template" opens TemplateOverlay right on top of this
// one instead, and the template it saves is both added to the picker
// and preselected, config/onTemplatesChanged existing only to make that
// nested overlay work.
export default function SuiteOverlay({ suite, templates = [], config, onClose, onSaved, onTemplatesChanged, showError }) {
  const isNew = !suite;
  const [templateIds, setTemplateIds] = useState((suite?.items || []).map((it) => it.templateId));
  const [mode, setMode] = useState(suite?.mode || "until_clean");
  const [showNewTemplate, setShowNewTemplate] = useState(false);

  const templateCreated = async (tmpl) => {
    await onTemplatesChanged();
    setTemplateIds((prev) => [...prev, tmpl.id]);
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const payload = {
      name: data.get("name"),
      templateIds,
      mode,
      count: mode === "count" ? Number(data.get("count")) : undefined,
      maxPasses: mode === "until_clean" ? Number(data.get("maxPasses")) : undefined,
      requireApproval: form.elements.requireApproval.checked,
      autoMerge: form.elements.autoMerge.checked,
    };
    try {
      if (isNew) {
        await api("/api/suites", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await api(`/api/suites/${suite.id}`, { method: "PATCH", body: JSON.stringify(payload) });
      }
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete the task suite "${suite.name}"? Runs already started from it are unaffected.`)) return;
    try {
      await api(`/api/suites/${suite.id}`, { method: "DELETE" });
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>{isNew ? "New task suite" : "Edit task suite"}</Typography>
      <form onSubmit={submit}>
        <TextField name="name" label="Name" defaultValue={suite?.name} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />

        <FormControl fullWidth margin="normal" size="small">
          <InputLabel id="suite-templates-label">Task templates</InputLabel>
          <Select
            labelId="suite-templates-label"
            label="Task templates"
            multiple
            value={templateIds}
            onChange={(e) => setTemplateIds(e.target.value)}
            renderValue={(selected) => selected.map((id) => templates.find((t) => t.id === id)?.name || id).join(", ")}
          >
            {templates.map((t) => (
              <MenuItem key={t.id} value={t.id}>
                <Checkbox checked={templateIds.includes(t.id)} size="small" />
                <ListItemText primary={t.name} secondary={t.title} />
              </MenuItem>
            ))}
          </Select>
        </FormControl>
        <Button size="small" onClick={() => setShowNewTemplate(true)} sx={{ mt: -1, mb: 1 }}>+ New template</Button>

        <FormControl fullWidth margin="normal" size="small">
          <InputLabel id="suite-mode-label">Run mode</InputLabel>
          <Select labelId="suite-mode-label" label="Run mode" value={mode} onChange={(e) => setMode(e.target.value)}>
            <MenuItem value="until_clean">Run until no repo changes or new tasks</MenuItem>
            <MenuItem value="count">Run a fixed number of times</MenuItem>
          </Select>
        </FormControl>

        {mode === "count" ? (
          <TextField
            name="count" label="Number of times" type="number" defaultValue={suite?.count || 1}
            inputProps={{ min: 1 }} required InputLabelProps={{ required: false }} margin="normal" sx={{ maxWidth: 220 }}
          />
        ) : (
          <TextField
            name="maxPasses" label="Max passes" type="number" defaultValue={suite?.maxPasses || 5}
            helperText="stops and reports a failure if no pass ever comes back clean" inputProps={{ min: 1 }}
            required InputLabelProps={{ required: false }} margin="normal" sx={{ maxWidth: 220 }}
          />
        )}

        <FormControlLabel
          control={<Checkbox name="autoMerge" defaultChecked={suite ? suite.autoMerge : true} />}
          label="Auto-merge each pull request once checks pass"
          sx={{ display: "flex", mt: 1 }}
        />
        <FormControlLabel
          control={<Checkbox name="requireApproval" defaultChecked={suite?.requireApproval} />}
          label="Require approval before each pass's tasks run"
          sx={{ display: "flex" }}
        />

        <Box sx={{ mt: 1 }}>
          <Typography variant="caption" color="text.secondary">
            By default a suite's tasks auto-queue and auto-merge -- check "require approval" to review them first.
          </Typography>
        </Box>

        <Stack direction="row" justifyContent={isNew ? "flex-end" : "space-between"} alignItems="center" sx={{ mt: 2 }}>
          {!isNew && <Button color="error" onClick={remove}>Delete</Button>}
          <Stack direction="row" spacing={1}>
            <Button onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="contained">{isNew ? "Add suite" : "Save"}</Button>
          </Stack>
        </Stack>
      </form>
      {showNewTemplate && (
        <TemplateOverlay config={config} onClose={() => setShowNewTemplate(false)} onSaved={templateCreated} showError={showError} />
      )}
    </Overlay>
  );
}
