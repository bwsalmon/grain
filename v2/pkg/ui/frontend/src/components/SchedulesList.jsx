import { useState } from "react";
import { Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
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
export default function SchedulesList({ schedules, templates = [], config, tasks, onRefresh, showError }) {
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

  const create = async (payload) => {
    try {
      await api("/api/schedules", { method: "POST", body: JSON.stringify(payload) });
      await onRefresh();
      return true;
    } catch (err) {
      showError(err);
      return false;
    }
  };

  const save = async (s, payload) => {
    try {
      await api(`/api/schedules/${s.id}`, { method: "PATCH", body: JSON.stringify(payload) });
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
                <ScheduleForm
                  schedule={s}
                  repoOptions={repoOptions}
                  templates={templates}
                  config={config}
                  submitLabel="Save"
                  onCancel={() => setEditingId(null)}
                  onSubmit={(payload) => save(s, payload)}
                />
              ) : (
                <>
                  <div className="schedule-summary">
                    <span className="schedule-title">{s.title}</span>
                    <Chip size="small" label={s.repo} />
                    <Chip size="small" label={describeRecurrence(s.recurrence)} />
                    {s.templateId && <Chip size="small" variant="outlined" label={`Template: ${s.templateName || s.templateId}`} />}
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
        <ScheduleForm repoOptions={repoOptions} templates={templates} config={config} submitLabel="Add schedule" onSubmit={create} isNew />
      </Box>
    </main>
  );
}

const WEEKDAYS = ["sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"];

function capitalize(s) {
  return s[0].toUpperCase() + s.slice(1);
}

// describeRecurrence renders a schedule's cadence the way a human reads
// it -- the row summary's own short form of what RecurrenceFields lets a
// human set.
function describeRecurrence(r) {
  if (!r) return "";
  switch (r.kind) {
    case "everyNHours": return `every ${r.everyNHours}h`;
    case "daily": return `daily at ${r.timeOfDay}`;
    case "weekly": return `${capitalize(r.weekday)}s at ${r.timeOfDay}`;
    case "monthly": return `monthly on day ${r.dayOfMonth} at ${r.timeOfDay}`;
    default: return r.kind;
  }
}

// RecurrenceFields is the "every N hours, or at a fixed time daily/
// weekly/monthly" picker bwsalmon/agents#464 asks for, replacing the old
// bare "Go duration" text field. Kind and Weekday are lifted into React
// state (rather than left as plain form fields FormData could read)
// because Kind decides which of the other inputs even render, and MUI's
// Select needs a controlled value either way -- ScheduleForm's submit
// handler reads them off the same state rather than off the form.
function RecurrenceFields({ defaultValue, kind, setKind, weekday, setWeekday }) {
  return (
    <Box sx={{ mt: 1 }}>
      <FormControl fullWidth margin="normal" size="small">
        <InputLabel id="recurrence-kind-label">Repeat</InputLabel>
        <Select
          labelId="recurrence-kind-label"
          label="Repeat"
          value={kind}
          onChange={(e) => setKind(e.target.value)}
        >
          <MenuItem value="everyNHours">Every N hours</MenuItem>
          <MenuItem value="daily">Daily, at a time</MenuItem>
          <MenuItem value="weekly">Weekly, on a day and time</MenuItem>
          <MenuItem value="monthly">Monthly, on a day and time</MenuItem>
        </Select>
      </FormControl>
      {kind === "everyNHours" && (
        <TextField
          name="everyNHours"
          label="Every N hours"
          type="number"
          defaultValue={defaultValue?.everyNHours || 24}
          inputProps={{ min: 1 }}
          required
          fullWidth
          margin="dense"
        />
      )}
      {kind !== "everyNHours" && (
        <TextField
          name="timeOfDay"
          label="Time (UTC)"
          type="time"
          defaultValue={defaultValue?.timeOfDay || "09:00"}
          required
          fullWidth
          margin="dense"
          InputLabelProps={{ shrink: true }}
        />
      )}
      {kind === "weekly" && (
        <FormControl fullWidth margin="dense" size="small">
          <InputLabel id="recurrence-weekday-label">Day of week</InputLabel>
          <Select
            labelId="recurrence-weekday-label"
            label="Day of week"
            value={weekday}
            onChange={(e) => setWeekday(e.target.value)}
          >
            {WEEKDAYS.map((d) => <MenuItem key={d} value={d}>{capitalize(d)}</MenuItem>)}
          </Select>
        </FormControl>
      )}
      {kind === "monthly" && (
        <TextField
          name="dayOfMonth"
          label="Day of month"
          type="number"
          defaultValue={defaultValue?.dayOfMonth || 1}
          inputProps={{ min: 1, max: 31 }}
          required
          fullWidth
          margin="dense"
          helperText="1-31; a shorter month fires on its last day"
        />
      )}
    </Box>
  );
}

// ScheduleForm is the create-a-schedule form and the per-row edit form,
// the same component either way: bwsalmon/agents#464 widens the field
// set to match NewTaskOverlay's own (reads and capabilities join the
// fields the two already shared), which made the two forms' duplication
// worth removing outright rather than growing further apart.
//
// dependsOn and "approved" have no place here, unlike NewTaskOverlay --
// CreateScheduleRequest's own doc comment explains why: a one-shot
// dependency link makes no sense against a task this schedule refiles
// indefinitely, and a firing always lands already approved by design.
//
// templateId (bwsalmon/agents#516) is its own bit of state, not a plain
// form field, because it decides whether the title/description/repo/
// base/reads/capabilities/auto-merge fields below even render: picking a
// template hands that content over entirely (ui.CreateScheduleRequest's
// own doc comment on why a template and per-field overrides do not mix),
// so this form hides them rather than showing fields the request would
// silently ignore.
function ScheduleForm({ schedule, repoOptions, templates = [], config, submitLabel, onSubmit, onCancel, isNew }) {
  const [capabilities, setCapabilities] = useState(schedule?.capabilities || []);
  const [kind, setKind] = useState(schedule?.recurrence?.kind || "everyNHours");
  const [weekday, setWeekday] = useState(schedule?.recurrence?.weekday || "monday");
  const [templateId, setTemplateId] = useState(schedule?.templateId || "");
  const margin = isNew ? "normal" : "dense";

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const data = new FormData(form);

    const recurrence = { kind };
    if (kind === "everyNHours") {
      recurrence.everyNHours = Number(data.get("everyNHours"));
    } else {
      recurrence.timeOfDay = data.get("timeOfDay");
      if (kind === "weekly") recurrence.weekday = weekday;
      if (kind === "monthly") recurrence.dayOfMonth = Number(data.get("dayOfMonth"));
    }

    // templateId is always sent, even "" -- CreateScheduleRequest reads
    // "" as "no template"; UpdateScheduleRequest reads a given-at-all
    // templateId (its own doc comment) the same way, "" meaning detach
    // rather than leave alone, which is exactly what re-submitting this
    // form with "None" selected should do.
    const payload = { templateId, recurrence };
    if (templateId === "") {
      const reads = (data.get("reads") || "")
        .split(",").map((r) => r.trim()).filter((r) => r !== "");
      payload.title = data.get("title");
      payload.description = data.get("description") || "";
      payload.repo = data.get("repo") || "";
      payload.base = data.get("base") || "";
      payload.autoMerge = form.elements.autoMerge.checked;
      payload.reads = reads;
      payload.capabilities = capabilities;
    }

    const ok = await onSubmit(payload);
    if (isNew && ok !== false) {
      form.reset();
      setCapabilities([]);
      setKind("everyNHours");
      setWeekday("monday");
      setTemplateId("");
    }
  };

  return (
    <form onSubmit={submit}>
      <FormControl fullWidth margin={margin} size="small">
        <InputLabel id={`schedule-template-label-${schedule?.id || "new"}`}>Template</InputLabel>
        <Select
          labelId={`schedule-template-label-${schedule?.id || "new"}`}
          label="Template"
          value={templateId}
          onChange={(e) => setTemplateId(e.target.value)}
        >
          <MenuItem value="">None -- fill in the fields below</MenuItem>
          {templates.map((t) => (
            <MenuItem key={t.id} value={t.id}>{t.name}</MenuItem>
          ))}
        </Select>
      </FormControl>
      {templateId !== "" ? (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1, mb: 1 }}>
          Title, description, repo, base branch, reads, capabilities and auto-merge
          all come from the selected template, and stay in sync with it.
        </Typography>
      ) : (
        <>
          <TextField name="title" label="Title" defaultValue={schedule?.title} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin={margin} />
          <TextField name="description" label="Description" defaultValue={schedule?.description} multiline rows={isNew ? 4 : 3} fullWidth margin={margin} />
          <Box component="label" sx={{ display: "block", mt: isNew ? 2 : 1.5, mb: 1 }}>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
              Target repo <span className="hint">owner/name</span>
            </Typography>
            <RepoField name="repo" options={repoOptions} defaultValue={schedule?.repo || ""} required />
          </Box>
          <TextField name="base" label="Base branch" defaultValue={schedule?.base} helperText="optional" placeholder="main" autoComplete="off" fullWidth margin={margin} />
          <TextField
            name="reads"
            label="Read-only repos"
            defaultValue={(schedule?.reads || []).join(", ")}
            helperText="owner/name, comma-separated, optional"
            placeholder="owner/shared-lib, owner/schema"
            autoComplete="off"
            fullWidth
            margin={margin}
          />
          <FormControlLabel
            control={<Checkbox name="autoMerge" defaultChecked={schedule?.autoMerge} />}
            label="Auto-merge once checks pass"
            sx={{ display: "flex", mt: 1 }}
          />
          <FormControl fullWidth margin={margin} size="small">
            <InputLabel id={`schedule-capabilities-label-${schedule?.id || "new"}`}>Capabilities</InputLabel>
            <Select
              labelId={`schedule-capabilities-label-${schedule?.id || "new"}`}
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
        </>
      )}
      <RecurrenceFields defaultValue={schedule?.recurrence} kind={kind} setKind={setKind} weekday={weekday} setWeekday={setWeekday} />
      <Stack direction="row" spacing={1} justifyContent="flex-end" sx={{ mt: 2 }}>
        {onCancel && <Button size="small" onClick={onCancel}>Cancel</Button>}
        <Button size={isNew ? "medium" : "small"} type="submit" variant="contained">{submitLabel}</Button>
      </Stack>
    </form>
  );
}

function formatWhen(iso) {
  return new Date(iso).toLocaleString();
}
