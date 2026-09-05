import { useState } from "react";
import { Alert, Button, Tab, Tabs, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import LogsPage from "./LogsPage.jsx";
import RootShellPage from "./RootShellPage.jsx";
import SandboxHealthPage from "./SandboxHealthPage.jsx";
import TopPage from "./TopPage.jsx";

const TABS = [
  { id: "logs", label: "Logs" },
  { id: "sandboxHealth", label: "Sandbox health" },
  { id: "top", label: "Top" },
  { id: "rootShell", label: "Root shell" },
  { id: "restart", label: "Restart" },
];

// SystemOverlay is Logs, Sandbox health and the reboot control's own
// sidebar destination (bwsalmon/agents#640) -- operator-only,
// deployment-wide diagnostics, but distinct enough from Settings' own
// configuration that it warranted a nav entry of its own again rather
// than staying folded into Settings' Debug tab (bwsalmon/agents#623).
// Each gets its own tab, the same layout SettingsOverlay.jsx already
// uses for its own General/Capabilities/Upgrade split.
//
// "System" rather than the "Debug" this pane was called until now
// (grain/task-12): what is behind it is the state of the machine this
// deployment runs on -- its logs, its sandboxes, its processes, its
// power switch -- which an operator reads to see what the deployment is
// doing, not only when they have a bug to chase. "Debug" named the mood
// somebody opens it in rather than what it shows, and it was the same
// word Settings' old tab and the self-debug capability already carry.
//
// Top (GET /api/host/top) sits directly after Sandbox health because it
// is the question that follows it: that panel's host section says the
// daemon's machine is loaded, and only a per-process view says by what
// (grain/task-120).
//
// Root shell (POST /api/host/shell, grain/task-13) comes last of the
// four reading panes and just before the danger zone, which is where it
// belongs in both directions: it is the pane an operator reaches only
// once the three above have failed to explain anything, and it is the
// only one that changes the machine rather than reporting on it.
//
// Metrics (GET /api/metrics) was a fourth tab here for a while, on the
// reasoning that it is the same kind of read-only, deployment-wide view
// the others are. It has its own sidebar entry now (MetricsOverlay.jsx,
// grain/task-173): what is left in here is what an operator opens
// because something is wrong right now, and a throughput report is the
// opposite of that -- it is read when nothing is wrong at all.
export default function SystemOverlay({ config, onClose, showError }) {
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
    if (
      !confirm(
        "Reboot the host machine? Every task currently running is interrupted, and this UI will be unreachable until the machine comes back up.",
      )
    )
      return;
    try {
      await api("/api/host/reboot", { method: "POST" });
    } catch (err) {
      if (!(err instanceof TypeError)) showError(err);
    }
  };

  // The title and the tab strip are the pane's fixed header, so they
  // stay reachable while a long log tail or a wide sandbox table scrolls
  // under them.
  const header = (
    <>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>
        System
      </Typography>
      <Tabs
        value={tab}
        onChange={(_, value) => setTab(value)}
        variant="scrollable"
        scrollButtons="auto"
      >
        {TABS.map((t) => (
          <Tab key={t.id} value={t.id} label={t.label} />
        ))}
      </Tabs>
    </>
  );

  // pane, and nothing capping the width inside it (grain/task-115):
  // every panel in here is either a table too many columns across for a
  // dialog (sandbox health) or a <pre> of log lines that wraps
  // badly in one. This was the widest centered box Overlay draws and it
  // was still the wrong shape -- what these panels want is the whole
  // content area beside the sidebar, which is what a pane is.
  return (
    <Overlay onClose={onClose} pane header={header}>
      {tab === "logs" && <LogsPage showError={showError} />}
      {tab === "sandboxHealth" && <SandboxHealthPage showError={showError} />}
      {tab === "top" && <TopPage showError={showError} />}
      {tab === "rootShell" && (
        <RootShellPage config={config} showError={showError} />
      )}
      {tab === "restart" &&
        (config && config.rebootEnabled ? (
          <fieldset>
            <legend>Danger zone</legend>
            <p className="hint">
              Reboots the machine grain itself is running on.
            </p>
            <Button variant="outlined" color="error" onClick={rebootHost}>
              Reboot host
            </Button>
          </fieldset>
        ) : (
          <Alert severity="info">
            Not available: rebooting the host is not enabled for this
            deployment.
          </Alert>
        ))}
    </Overlay>
  );
}
