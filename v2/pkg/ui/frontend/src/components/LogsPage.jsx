import { useCallback, useEffect, useState } from "react";
import { Alert, Button, FormControl, InputLabel, MenuItem, Select, Typography } from "@mui/material";
import api from "../api.js";

// REFRESH_MS is how often this panel re-fetches the selected source's log
// lines while the Debug overlay is open -- the same order of magnitude
// as App.jsx's own POLL_INTERVAL_MS, so a log tailed here moves about as
// often as the task list does.
const REFRESH_MS = 5000;
const LINES_TO_FETCH = 500;

// LogsPage is the Logs panel of DebugOverlay.jsx -- it used to be a full
// nav entry of its own (bwsalmon/agents#457), then a tab inside Settings
// alongside Sandbox health and the reboot control (bwsalmon/agents#623),
// before settling on its own "Debugging" sidebar entry, apart from
// Settings (bwsalmon/agents#640). GET /api/logs' own "enabled" flag says
// whether this deployment has any log sources configured at all; when it
// doesn't (`grain demo`'s throwaway UI, or any UI not colocated with a
// real daemon), this shows a note instead of a pane that could only ever
// 404, the same convention SecretsPanel/UpgradePanel already use for
// their own optional pieces.
export default function LogsPage({ showError }) {
  const [sources, setSources] = useState(null);
  const [source, setSource] = useState(null);
  const [lines, setLines] = useState([]);

  useEffect(() => {
    (async () => {
      try {
        const res = await api("/api/logs");
        setSources(res);
        if (res.enabled && res.sources.length > 0) setSource(res.sources[0]);
      } catch (err) {
        showError(err);
      }
    })();
  }, [showError]);

  const refresh = useCallback(async () => {
    if (!source) return;
    try {
      const res = await api(`/api/logs/${encodeURIComponent(source)}?lines=${LINES_TO_FETCH}`);
      setLines(res.lines || []);
    } catch (err) {
      showError(err);
    }
  }, [source, showError]);

  useEffect(() => { refresh(); }, [refresh]);

  useEffect(() => {
    if (!source) return;
    const interval = setInterval(refresh, REFRESH_MS);
    return () => clearInterval(interval);
  }, [source, refresh]);

  if (sources === null) return null;

  return (
    <section className="logs-panel">
      <Typography variant="subtitle2" sx={{ mb: 1 }}>Logs</Typography>
      {!sources.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this deployment has no log sources configured (bwsalmon/agents#444).
        </Alert>
      )}
      {sources.enabled && (
        <>
          <div className="logs-toolbar">
            <FormControl size="small" sx={{ minWidth: 220 }}>
              <InputLabel id="logs-source-label">Source</InputLabel>
              <Select
                labelId="logs-source-label"
                label="Source"
                value={source || ""}
                onChange={(e) => setSource(e.target.value)}
              >
                {sources.sources.map((s) => <MenuItem key={s} value={s}>{s}</MenuItem>)}
              </Select>
            </FormControl>
            <Button size="small" variant="outlined" onClick={refresh}>Refresh</Button>
          </div>
          <pre className="logs-view">{lines.length > 0 ? lines.join("\n") : "(no log lines)"}</pre>
        </>
      )}
    </section>
  );
}
