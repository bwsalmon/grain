import { useState } from "react";
import { Box, Button, Checkbox, Chip, FormControl, FormControlLabel, InputLabel, ListItemText, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import RepoField from "./RepoField.jsx";

const WEEKDAYS = ["sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"];

function capitalize(s) {
  return s[0].toUpperCase() + s.slice(1);
}

// RecurrenceFields is the "every N hours, or at a fixed time daily/
// weekly/monthly" picker bwsalmon/agents#464 asks for. Kind and Weekday
// are lifted into React state (rather than left as plain form fields
// FormData could read) because Kind decides which of the other inputs
// even render, and MUI's Select needs a controlled value either way --
// ScheduleOverlay's submit handler reads them off the same state rather
// than off the form.
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

// ScheduleOverlay is the schedules page's own sub page (bwsalmon/
// agents#547): the "+" on SchedulesList opens this blank, and clicking
// an existing row opens the same overlay pre-filled with that schedule's
// fields -- TemplateOverlay's own split (bwsalmon/agents#545) applied to
// schedules, so a schedule stops being the one list on its page with a
// full form and its own row of buttons crowding the row itself. Pause/
// Resume and Delete move in here too, alongside the editable fields, the
// same place DetailOverlay keeps a task's own Close/Reopen next to its
// editable fields rather than on TaskList's row.
//
// dependsOn and "approved" have no place here, unlike NewTaskOverlay --
// CreateScheduleRequest's own doc comment explains why: a one-shot
// dependency link makes no sense against a task this schedule refiles
// indefinitely, and a firing always lands already approved by design.
//
// templateId (bwsalmon/agents#516) is its own bit of state, not a plain
// form field, because it decides whether the title/description/reads/
// capabilities fields below even render: picking a template hands that
// content over entirely (ui.CreateScheduleRequest's own doc comment on
// why a template and per-field overrides do not mix), so this form hides
// them rather than showing fields the request would silently ignore.
// Repo and base branch are never among them -- a template carries no
// target of its own (model.TaskTemplate's own doc comment on why) -- so
// those two always render, template selected or not.
//
// "Fires" (fires/suiteId) is the same idea one step out: a schedule can
// run a whole task suite on its cadence instead of filing one task, in
// which case the suite decides everything a template would have and
// more, so every content field here gives way to a single suite picker.
// It is offered on a new schedule only -- what a schedule fires is fixed
// when it is created (ui.UpdateScheduleRequest's own doc comment on why),
// though which suite a suite-backed one runs stays editable.
export default function ScheduleOverlay({ schedule, repoOptions, templates = [], suites = [], config, onClose, onSaved, showError }) {
  const isNew = !schedule;
  const [capabilities, setCapabilities] = useState(schedule?.capabilities || []);
  const [kind, setKind] = useState(schedule?.recurrence?.kind || "everyNHours");
  const [weekday, setWeekday] = useState(schedule?.recurrence?.weekday || "monday");
  const [templateId, setTemplateId] = useState(schedule?.templateId || "");
  const [fires, setFires] = useState(schedule?.suiteId ? "suite" : "task");
  const [suiteId, setSuiteId] = useState(schedule?.suiteId || "");
  const firesSuite = fires === "suite";

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

    // Caught here rather than left to the API, which would read a blank
    // suiteId as "this schedule files a task" and complain about a
    // missing title instead of about the picker actually left empty.
    if (firesSuite && suiteId === "") {
      showError(new Error("choose a task suite for this schedule to run"));
      return;
    }

    // templateId is always sent for a task schedule, even "" --
    // CreateScheduleRequest reads "" as "no template";
    // UpdateScheduleRequest reads a given-at-all templateId (its own doc
    // comment) the same way, "" meaning detach rather than leave alone,
    // which is exactly what re-submitting this form with "None" selected
    // should do. A suite schedule sends suiteId in its place and nothing
    // else: the suite decides the content, the passes, the approval and
    // the auto-merge of everything a firing runs. repo/base are always
    // sent either way -- neither a template nor a suite carries a target
    // of its own, so they are this schedule's own fields whatever it
    // fires.
    const payload = {
      ...(firesSuite ? { suiteId } : { templateId }),
      recurrence,
      repo: data.get("repo") || "",
      base: data.get("base") || "",
    };
    if (!firesSuite && templateId === "") {
      const reads = (data.get("reads") || "")
        .split(",").map((r) => r.trim()).filter((r) => r !== "");
      payload.title = data.get("title");
      payload.description = data.get("description") || "";
      payload.autoMerge = form.elements.autoMerge.checked;
      payload.reads = reads;
      payload.capabilities = capabilities;
    }

    try {
      if (isNew) {
        await api("/api/schedules", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await api(`/api/schedules/${schedule.id}`, { method: "PATCH", body: JSON.stringify(payload) });
      }
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  const toggleEnabled = async () => {
    try {
      await api(`/api/schedules/${schedule.id}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !schedule.enabled }),
      });
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete the schedule "${schedule.title}"? Tasks it already filed are not affected.`)) return;
    try {
      await api(`/api/schedules/${schedule.id}`, { method: "DELETE" });
      await onSaved();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>{isNew ? "New schedule" : "Edit schedule"}</Typography>
      <form onSubmit={submit}>
        {isNew && (
          <FormControl fullWidth margin="normal" size="small">
            <InputLabel id="schedule-fires-label">Fires</InputLabel>
            <Select
              labelId="schedule-fires-label"
              label="Fires"
              value={fires}
              onChange={(e) => setFires(e.target.value)}
            >
              <MenuItem value="task">A task</MenuItem>
              <MenuItem value="suite">A task suite</MenuItem>
            </Select>
          </FormControl>
        )}
        {firesSuite ? (
          <FormControl fullWidth margin="normal" size="small">
            <InputLabel id="schedule-suite-label">Task suite</InputLabel>
            <Select
              labelId="schedule-suite-label"
              label="Task suite"
              value={suiteId}
              onChange={(e) => setSuiteId(e.target.value)}
            >
              {suites.map((s) => (
                <MenuItem key={s.id} value={s.id}>{s.name}</MenuItem>
              ))}
            </Select>
          </FormControl>
        ) : (
          <FormControl fullWidth margin="normal" size="small">
            <InputLabel id="schedule-template-label">Template</InputLabel>
            <Select
              labelId="schedule-template-label"
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
        )}
        <Box component="label" sx={{ display: "block", mt: 2, mb: 1 }}>
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
            Target repo <span className="hint">owner/name</span>
          </Typography>
          <RepoField name="repo" options={repoOptions} defaultValue={schedule?.repo || ""} required />
        </Box>
        <TextField
          name="base"
          label="Base branch"
          defaultValue={schedule?.base}
          helperText={firesSuite ? "required: a suite run stacks its tasks against one branch" : "optional"}
          placeholder="main"
          required={firesSuite}
          InputLabelProps={{ required: false }}
          autoComplete="off"
          fullWidth
          margin="normal"
        />
        {firesSuite ? (
          <Typography variant="body2" color="text.secondary" sx={{ mt: 1, mb: 1 }}>
            Every firing starts one run of the selected suite against this repo
            and branch -- the suite decides which tasks run, how many passes,
            and whether they need approval.
          </Typography>
        ) : templateId !== "" ? (
          <Typography variant="body2" color="text.secondary" sx={{ mt: 1, mb: 1 }}>
            Title, description, reads, capabilities and auto-merge all come from
            the selected template, and stay in sync with it.
          </Typography>
        ) : (
          <>
            <TextField name="title" label="Title" defaultValue={schedule?.title} required InputLabelProps={{ required: false }} autoComplete="off" fullWidth margin="normal" />
            <TextField name="description" label="Description" defaultValue={schedule?.description} multiline rows={4} fullWidth margin="normal" />
            <TextField
              name="reads"
              label="Read-only repos"
              defaultValue={(schedule?.reads || []).join(", ")}
              helperText="owner/name, comma-separated, optional"
              placeholder="owner/shared-lib, owner/schema"
              autoComplete="off"
              fullWidth
              margin="normal"
            />
            <FormControlLabel
              control={<Checkbox name="autoMerge" defaultChecked={schedule?.autoMerge} />}
              label="Auto-merge once checks pass"
              sx={{ display: "flex", mt: 1 }}
            />
            <FormControl fullWidth margin="normal" size="small">
              <InputLabel id="schedule-capabilities-label">Capabilities</InputLabel>
              <Select
                labelId="schedule-capabilities-label"
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
        <Stack direction="row" justifyContent={isNew ? "flex-end" : "space-between"} alignItems="center" sx={{ mt: 2 }}>
          {!isNew && (
            <Stack direction="row" spacing={1}>
              <Button color="error" onClick={remove}>Delete</Button>
              <Button onClick={toggleEnabled}>{schedule.enabled ? "Pause" : "Resume"}</Button>
            </Stack>
          )}
          <Stack direction="row" spacing={1}>
            <Button onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="contained">{isNew ? "Add schedule" : "Save"}</Button>
          </Stack>
        </Stack>
      </form>
    </Overlay>
  );
}
