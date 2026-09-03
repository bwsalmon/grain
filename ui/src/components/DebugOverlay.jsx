import { useState } from "react";
import { Alert, Button, Tab, Tabs, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import LogsPage from "./LogsPage.jsx";
import MetricsPage from "./MetricsPage.jsx";
import SandboxHealthPage from "./SandboxHealthPage.jsx";

const TABS = [
  { id: "logs", label: "Logs" },
  { id: "sandboxHealth", label: "Sandbox health" },
  { id: "metrics", label: "Metrics" },
  { id: "restart", label: "Restart" },
];

// DebugOverlay is Logs, Sandbox health and the reboot control's own
// sidebar destination (bwsalmon/agents#640) -- operator-only,
// deployment-wide diagnostics, but distinct enough from Settings' own
// configuration that it warranted a nav entry of its own again rather
// than staying folded into Settings' Debug tab (bwsalmon/agents#623).
// Each gets its own tab, the same layout SettingsOverlay.jsx already
// uses for its own General/Capabilities/Secrets/Upgrade split.
//
// Metrics (GET /api/metrics) joined them later rather than taking a
// sidebar entry of its own: it is the same kind of thing the
// other tabs are -- a read-only, deployment-wide view of how the machine
// is behaving, reached when somebody is asking a question about the
// deployment rather than about a task -- and "why is this slow" is
// usually answered by looking at it next to sandbox health anyway.
//
// onOpenTask is threaded through for the one link out of these panels:
// the metrics backlog names the oldest queued task, and the useful thing
// to do with that is go and look at it. App closes this overlay on the
// way, since two stacked dialogs would put the task behind the one it
// was opened from.
export default function DebugOverlay({ config, onClose, onOpenTask, showError }) {
  const [tab, setTab] = useState("logs");
  // rebootHost is deliberately its own confirm/try, separate from any
  // settings-form save flow: it is not a settings field, and unlike a
  // failed save there is no "current" state to fall back on showing
  // afterward -- a successful call cuts this same connection along with
  // everything else on the machine.
  //
  // That cut connection is exactly what makes this button look broken
  // (bwsalmon/agents#581): the reboot itself starts before the daemon's
  // 200 response finishes its round trip back through this deployment's
  // load balancer/proxy hops, so the browser's fetch commonly rejects
  // with its own network-level failure -- a TypeError, per the Fetch
  // spec -- even though the reboot is under way. api()'s own throw for
  // a real non-2xx response is always a plain Error carrying the
  // server's message (api.js), never a TypeError, so that's the signal
  // this can key on to tell "the machine is going down, as asked" apart
  // from an actual failure (disabled, or the sudo command itself
  // erroring) worth showing the operator.
  const rebootHost = async () => {
    if (!confirm("Reboot the host machine? Every task currently running is interrupted, and this UI will be unreachable until the machine comes back up.")) return;
    try {
      await api("/api/host/reboot", { method: "POST" });
    } catch (err) {
      if (!(err instanceof TypeError)) showError(err);
    }
  };

  // wide: every panel in here is either a table too many columns across
  // for the default width (sandbox health, metrics) or a <pre> of log
  // lines that wraps badly without it.
  return (
    <Overlay onClose={onClose} wide>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Debug</Typography>
      <Tabs value={tab} onChange={(_, value) => setTab(value)} sx={{ mb: 2 }}>
        {TABS.map((t) => (
          <Tab key={t.id} value={t.id} label={t.label} />
        ))}
      </Tabs>
      {tab === "logs" && <LogsPage showError={showError} />}
      {tab === "sandboxHealth" && <SandboxHealthPage showError={showError} />}
      {tab === "metrics" && <MetricsPage showError={showError} onOpenTask={onOpenTask} />}
      {tab === "restart" && (
        config && config.rebootEnabled ? (
          <fieldset>
            <legend>Danger zone</legend>
            <p className="hint">Reboots the machine grain itself is running on.</p>
            <Button variant="outlined" color="error" onClick={rebootHost}>Reboot host</Button>
          </fieldset>
        ) : (
          <Alert severity="info">Not available: rebooting the host is not enabled for this deployment.</Alert>
        )
      )}
    </Overlay>
  );
}
