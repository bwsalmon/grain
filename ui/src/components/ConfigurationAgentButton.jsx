import { useState } from "react";
import { Fab, Tooltip } from "@mui/material";
import SettingsSuggestIcon from "@mui/icons-material/SettingsSuggest";
import api from "../api.js";

// ConfigurationAgentButton is bwsalmon/agents#621's overlay: reachable
// from the bottom-right corner of the screen no matter what view is on
// screen, it files grain's own configuration agent -- an interactive
// task the backend (ui.Client.CreateTask) bundles with the self-debug/
// self-repair/bootstrap-playbooks grants (the third added by
// bwsalmon/agents#620, so it can read and act on the GCP/GitHub setup
// runbooks under pkg/capability/bootstrap/playbooks/) and a prompt
// oriented at helping with a problem, a question, grain's own
// configuration, or one of those bootstrap flows -- and opens its chat
// the moment it exists, the same way NewTaskOverlay already does for an
// ordinary interactive task (bwsalmon/agents#539).
//
// One click, no form: everything the task needs (Interactive,
// Capabilities, a default Title/Description) is assembled server-side
// from `configuration: true` alone, precisely so reaching for this needs
// no more thought than clicking it. defaultRepo carries whatever repo
// the screen is already about (App.jsx's own openRepo), the same
// value NewTaskOverlay defaults its own repo field from -- empty falls
// back to the deployment's own default target, and a deployment with
// neither surfaces CreateTask's own "no repo given" error through
// showError rather than silently doing nothing.
export default function ConfigurationAgentButton({ defaultRepo, onOpenTask, showError }) {
  const [busy, setBusy] = useState(false);

  const start = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const task = await api("/api/tasks", {
        method: "POST",
        body: JSON.stringify({ configuration: true, repo: defaultRepo || "" }),
      });
      onOpenTask(task.id);
    } catch (err) {
      showError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Tooltip title="Configuration agent — ask a question, debug a problem, or change grain's own configuration">
      <Fab
        color="primary"
        onClick={start}
        disabled={busy}
        aria-label="Open configuration agent"
        sx={{ position: "fixed", right: 24, bottom: 24, zIndex: 1200 }}
      >
        <SettingsSuggestIcon />
      </Fab>
    </Tooltip>
  );
}
