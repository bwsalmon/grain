import { useCallback, useEffect, useState } from "react";
import { Box, Button, Typography } from "@mui/material";
import api from "../api.js";

// REFRESH_MS is how often this page re-fetches the selected source's log
// lines while it is the active view -- the same order of magnitude as
// App.jsx's own POLL_INTERVAL_MS, so a log tailed here moves about as
// often as the task list it sits alongside does.
const REFRESH_MS = 5000;
const LINES_TO_FETCH = 500;

// LogsPage is the logs pane (bwsalmon/agents#457): a full nav entry
// alongside Repos and Scheduled tasks rather than a button that pops a
// modal, the same promotion bwsalmon/agents#455 already gave
// SchedulesList. GET /api/logs' own "enabled" flag says whether this
// deployment has any log sources configured at all; when it doesn't
// (`grain demo`'s throwaway UI, or any UI not colocated with a real
// daemon), this shows a note instead of a pane that could only ever
// 404, the same convention UpgradePanel/SecretsPanel (Settings' own
// tabs) already use for their own optional pieces.
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
    <main>
      <Box sx={{ px: "1.5rem" }}>
        <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Logs</Typography>
        {!sources.enabled && (
          <p className="unconfigured-note">
            Not available: this deployment has no log sources configured (bwsalmon/agents#444).
          </p>
        )}
        {sources.enabled && (
          <>
            <div className="logs-toolbar">
              <label>Source
                <select value={source || ""} onChange={(e) => setSource(e.target.value)}>
                  {sources.sources.map((s) => <option key={s} value={s}>{s}</option>)}
                </select>
              </label>
              <Button size="small" variant="outlined" onClick={refresh}>Refresh</Button>
            </div>
            <pre className="logs-view">{lines.length > 0 ? lines.join("\n") : "(no log lines)"}</pre>
          </>
        )}
      </Box>
    </main>
  );
}
