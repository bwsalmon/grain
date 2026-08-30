import { useCallback, useEffect, useState } from "react";
import { Box, Button, Checkbox, Chip, FormControlLabel, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import { knownRepos } from "../state.js";
import Overlay from "./Overlay.jsx";
import RepoField from "./RepoField.jsx";

// ScheduledTasksOverlay manages schedules (bwsalmon/agents#376): each row
// is a standing declaration -- "file this task every N" -- that graind's
// own schedule reconciler turns into a real task each time it comes due.
// SecretsOverlay's own shape fits here almost exactly: a list fetched on
// open, refreshed after every mutation, plus a form that only ever adds.
export default function ScheduledTasksOverlay({ config, tasks, onClose, showError }) {
  const [schedules, setSchedules] = useState(null);
  const repoOptions = knownRepos(config, tasks);

  const refresh = useCallback(async () => {
    try {
      setSchedules(await api("/api/schedules"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const toggleEnabled = async (s) => {
    try {
      await api(`/api/schedules/${s.id}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !s.enabled }),
      });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async (s) => {
    if (!confirm(`Delete the schedule "${s.title}"? Tasks it already filed are not affected.`)) return;
    try {
      await api(`/api/schedules/${s.id}`, { method: "DELETE" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);
    const payload = {
      title: data.get("title"),
      description: data.get("description") || "",
      repo: data.get("repo"),
      base: data.get("base") || "",
      autoMerge: form.elements.autoMerge.checked,
      interval: data.get("interval"),
    };
    try {
      await api("/api/schedules", { method: "POST", body: JSON.stringify(payload) });
      form.reset();
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  if (schedules === null) return null;

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Scheduled tasks</Typography>
      <ul className="schedules-list">
        {schedules.map((s) => (
          <li className="schedule-row" key={s.id}>
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
              <Button size="small" variant="outlined" onClick={() => toggleEnabled(s)}>
                {s.enabled ? "Pause" : "Resume"}
              </Button>
              <Button size="small" variant="outlined" color="error" onClick={() => remove(s)}>
                Delete
              </Button>
            </Stack>
          </li>
        ))}
      </ul>
      {schedules.length === 0 && <p className="empty">No scheduled tasks.</p>}
      <form onSubmit={submit}>
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
    </Overlay>
  );
}

function formatWhen(iso) {
  return new Date(iso).toLocaleString();
}
