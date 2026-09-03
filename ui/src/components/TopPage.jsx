import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Checkbox, FormControlLabel, Typography } from "@mui/material";
import api from "../api.js";

// REFRESH_MS is LogsPage's own cadence, for the same reason: every panel
// of the Debug overlay should feel like the same kind of live view. It is
// also about as often as `top` itself redraws when an operator runs it in
// a terminal.
const REFRESH_MS = 5000;

// LINES_TO_FETCH is the summary block plus enough process rows to cover
// anything actually using the machine -- the server sorts by %CPU, so
// what falls off the end is idle (pkg/hosttop).
const LINES_TO_FETCH = 60;

// TopPage is the Top tab of DebugOverlay.jsx: `top` on the machine the
// daemon itself runs on, over GET /api/host/top (grain/task-120).
//
// It sits beside Sandbox health rather than inside it because it answers
// the next question rather than the same one. That panel's host section
// is aggregate -- load average, memory, disk -- and says the machine is
// under pressure without ever being able to say by what. This is the
// per-process view an operator would otherwise open an SSH session for.
//
// Auto-refresh is a checkbox the operator can clear, which the Logs pane
// has no equivalent of and needs none: a log is append-only, so a poll
// only ever adds lines below what was already read, while every poll here
// re-sorts the whole table under the cursor -- which is exactly what one
// does not want while reading a row. Each poll costs a `top` run on the
// daemon's own machine besides.
//
// GET /api/host/top's own "enabled" flag says whether this deployment has
// a reader configured at all; when it doesn't (`grain demo`'s throwaway
// UI, or an image without procps) this shows a note rather than a pane
// that could only ever error, the same convention LogsPage and
// SandboxHealthPage already follow.
export default function TopPage({ showError }) {
  const [data, setData] = useState(null);
  const [auto, setAuto] = useState(true);

  const refresh = useCallback(async () => {
    try {
      setData(await api(`/api/host/top?lines=${LINES_TO_FETCH}`));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  // The poll stops while auto-refresh is off, and never starts at all on
  // a deployment with no reader configured -- there is nothing behind the
  // call there but a repeated "not available".
  const enabled = data !== null && data.enabled;
  useEffect(() => {
    if (!auto || !enabled) return undefined;
    const interval = setInterval(refresh, REFRESH_MS);
    return () => clearInterval(interval);
  }, [auto, enabled, refresh]);

  if (data === null) return null;

  return (
    <section className="top-panel">
      <Typography variant="subtitle2" sx={{ mb: 1 }}>Top</Typography>
      {!data.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this deployment has no process snapshot configured.
        </Alert>
      )}
      {data.enabled && (
        <>
          <Box className="logs-toolbar">
            <Typography variant="body2" color="text.secondary" sx={{ flexGrow: 1 }}>
              Processes on the machine the daemon runs on, busiest first.
            </Typography>
            <FormControlLabel
              control={<Checkbox size="small" checked={auto} onChange={(e) => setAuto(e.target.checked)} />}
              label="Auto-refresh"
            />
            <Button size="small" variant="outlined" onClick={refresh}>Refresh</Button>
          </Box>
          <pre className="logs-view top-view">
            {data.lines && data.lines.length > 0 ? data.lines.join("\n") : "(no output)"}
          </pre>
        </>
      )}
    </section>
  );
}
