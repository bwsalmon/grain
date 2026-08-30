import { useState } from "react";
import { Box, Button, Checkbox, Chip, FormControlLabel, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { knownRepos } from "../state.js";
import RepoField from "./RepoField.jsx";

// SchedulesList is the schedules pane (bwsalmon/agents#455): a full nav
// entry alongside Repos rather than a button that pops a modal, so a
// deployment with more than a couple of schedules has somewhere to see
// and edit all of them at once. It keeps ScheduledTasksOverlay's own
// list-plus-form shape (bwsalmon/agents#376) almost exactly, adding only
// a per-row "Edit" mode -- the PATCH endpoint it uses already accepted
// every field, just unreachable from the old overlay's UI.
export default function SchedulesList({ schedules, config, tasks, onRefresh, showError }) {
  const [editingId, setEditingId] = useState(null);
  const repoOptions = knownRepos(config, tasks);

  const toggleEnabled = async (s) => {
    try {
      await api(`/api/schedules/${s.id}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !s.enabled }),
      });
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async (s) => {
    if (!confirm(`Delete the schedule "${s.title}"? Tasks it already filed are not affected.`)) return;
    try {
      await api(`/api/schedules/${s.id}`, { method: "DELETE" });
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  const fieldsFrom = (form) => {
    const data = new FormData(form);
    return {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo"),
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      interval: data.get("interval"),
    };
  };

  const create = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    try {
      await api("/api/schedules", { method: "POST", body: JSON.stringify(fieldsFrom(form)) });
      form.reset();
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  const save = async (s, evt) => {
    evt.preventDefault();
    try {
      await api(`/api/schedules/${s.id}`, { method: "PATCH", body: JSON.stringify(fieldsFrom(evt.target)) });
      setEditingId(null);
      await onRefresh();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <main>
      <Box sx={{ px: "1.5rem" }}>
        <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Scheduled tasks</Typography>
        <ul className="schedules-list">
          {schedules.map((s) => (
            <li className="schedule-row" key={s.id}>
              {editingId === s.id ? (
                <ScheduleEditForm schedule={s} repoOptions={repoOptions} onCancel={() => setEditingId(null)} onSubmit={(evt) => save(s, evt)} />
              ) : (
                <>
                  <div className="schedule-summary">
                    <span className="schedule-title">{s.title}</span>
                    <Chip size="small" label={s.repo} />
                    <Chip size="small" label={`every ${s.interval}`} />
                    {!s.enabled && <Chip size="small" color="error" label="Paused" />}
                  </div>
                  <div className="schedule-meta hint">
                    Next run {formatWhen(s.nextRunAt)}
                    {s.lastRunAt && <> · last ran {formatWhen(s.lastRunAt)}</>}
                  </div>
                  <Stack direction="row" spacing={1} sx={{ mt: 0.5 }}>
                    <Button size="small" variant="outlined" onClick={() => setEditingId(s.id)}>
                      Edit
                    </Button>
                    <Button size="small" variant="outlined" onClick={() => toggleEnabled(s)}>
                      {s.enabled ? "Pause" : "Resume"}
                    </Button>
                    <Button size="small" variant="outlined" color="error" onClick={() => remove(s)}>
                      Delete
                    </Button>
                  </Stack>
                </>
              )}
            </li>
          ))}
        </ul>
        {schedules.length === 0 && <p className="empty">No scheduled tasks.</p>}

        <Typography variant="subtitle1" sx={{ mt: 2 }}>New schedule</Typography>
        <form onSubmit={create}>
          <TextField name="title" label="Title" required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />
          <TextField name="description" label="Description" multiline rows={4} fullWidth margin="normal" />
          <Box component="label" sx={{ display: "block", mt: 2, mb: 1 }}>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
              Target repo <span className="hint">owner/name</span>
            </Typography>
            <RepoField name="repo" options={repoOptions} required />
          </Box>
          <TextField name="base" label="Base branch" helperText="optional" placeholder="main" autoComplete="off" fullWidth margin="normal" />
          <TextField name="interval" label="Interval" helperText="Go duration, e.g. 24h" placeholder="24h" required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />
          <FormControlLabel
            control={<Checkbox name="autoMerge" />}
            label="Auto-merge once checks pass"
            sx={{ display: "flex", mt: 1 }}
          />
          <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
            <Button type="submit" variant="contained">Add schedule</Button>
          </Stack>
        </form>
      </Box>
    </main>
  );
}

// ScheduleEditForm is the same field set as the create form below, just
// pre-filled from an existing schedule and with Save/Cancel in place of
// Add -- editing in place, rather than in a separate overlay, since a
// pane already has the room a modal was working around.
function ScheduleEditForm({ schedule, repoOptions, onCancel, onSubmit }) {
  return (
    <form onSubmit={onSubmit}>
      <TextField name="title" label="Title" defaultValue={schedule.title} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="dense" />
      <TextField name="description" label="Description" defaultValue={schedule.description} multiline rows={3} fullWidth margin="dense" />
      <Box component="label" sx={{ display: "block", mt: 1.5, mb: 1 }}>
        <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
          Target repo <span className="hint">owner/name</span>
        </Typography>
        <RepoField name="repo" options={repoOptions} defaultValue={schedule.repo} required />
      </Box>
      <TextField name="base" label="Base branch" defaultValue={schedule.base} helperText="optional" placeholder="main" autoComplete="off" fullWidth margin="dense" />
      <TextField name="interval" label="Interval" defaultValue={schedule.interval} helperText="Go duration, e.g. 24h" placeholder="24h" required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="dense" />
      <FormControlLabel
        control={<Checkbox name="autoMerge" defaultChecked={schedule.autoMerge} />}
        label="Auto-merge once checks pass"
        sx={{ display: "flex", mt: 1 }}
      />
      <Stack direction="row" spacing={1} justifyContent="flex-end" sx={{ mt: 1 }}>
        <Button size="small" onClick={onCancel}>Cancel</Button>
        <Button size="small" type="submit" variant="contained">Save</Button>
      </Stack>
    </form>
  );
}

function formatWhen(iso) {
  return new Date(iso).toLocaleString();
}
