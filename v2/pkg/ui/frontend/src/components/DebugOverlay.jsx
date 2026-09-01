import { Button, Divider, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import LogsPage from "./LogsPage.jsx";
import SandboxHealthPage from "./SandboxHealthPage.jsx";

// DebugOverlay is Logs, Sandbox health and the reboot control's own
// sidebar destination (bwsalmon/agents#640) -- operator-only,
// deployment-wide diagnostics, but distinct enough from Settings' own
// configuration that it warranted a nav entry of its own again rather
// than staying folded into Settings' Debug tab (bwsalmon/agents#623).
export default function DebugOverlay({ config, onClose, showError }) {
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

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Debug</Typography>
      <LogsPage showError={showError} />
      <Divider sx={{ my: 3 }} />
      <SandboxHealthPage showError={showError} />
      {config && config.rebootEnabled && (
        <>
          <Divider sx={{ my: 3 }} />
          <fieldset>
            <legend>Danger zone</legend>
            <p className="hint">Reboots the machine grain itself is running on.</p>
            <Button variant="outlined" color="error" onClick={rebootHost}>Reboot host</Button>
          </fieldset>
        </>
      )}
    </Overlay>
  );
}
