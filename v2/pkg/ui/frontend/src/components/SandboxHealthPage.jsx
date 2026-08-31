import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Chip, CircularProgress, Table, TableBody, TableCell, TableHead, TableRow, Typography } from "@mui/material";
import api from "../api.js";

// REFRESH_MS matches LogsPage's own polling cadence -- the debug section
// (bwsalmon/agents#536) these two panes share should feel like the same
// kind of live view.
const REFRESH_MS = 5000;

function formatMemory(usedMB, totalMB) {
  if (!totalMB) return "—";
  return `${usedMB} / ${totalMB} MB`;
}

// SandboxHealthPage is the sandbox health pane (bwsalmon/agents#536): a
// live view of every dispatch slot's own sandbox -- a kontur VM or a host
// directory, whichever backend this deployment runs -- plus the daemon's
// own host machine's CPU/RAM pressure. Both come back from the same
// GET /api/sandboxes call since a sandbox that looks stuck is often
// really the host it runs on being starved, so debugging one usually
// means looking at both at once. GET /api/sandboxes' own "enabled" flag
// says whether this deployment has either piece configured at all, the
// same convention LogsPage's own GET /api/logs already establishes for
// the debug section this pane sits alongside.
export default function SandboxHealthPage({ showError }) {
  const [data, setData] = useState(null);

  const refresh = useCallback(async () => {
    try {
      setData(await api("/api/sandboxes"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  useEffect(() => {
    const interval = setInterval(refresh, REFRESH_MS);
    return () => clearInterval(interval);
  }, [refresh]);

  return (
    <main className="logs-page">
      <div className="content-header">
        <Typography variant="h6" component="h2" sx={{ m: 0, fontSize: "1rem", fontWeight: 600 }}>Sandbox health</Typography>
        {data?.enabled && <Button size="small" variant="outlined" onClick={refresh}>Refresh</Button>}
      </div>
      {data === null && (
        <div style={{ flex: 1, minHeight: 0, display: "flex", alignItems: "center", justifyContent: "center" }}>
          <CircularProgress size={28} aria-label="Loading sandbox health" />
        </div>
      )}
      {data !== null && !data.enabled && (
        <Alert severity="info" sx={{ mx: "1.75rem", mt: "1rem" }}>
          Not available: this deployment has no sandbox pool or host stats configured.
        </Alert>
      )}
      {data !== null && data.enabled && (
        <div style={{ flex: 1, minHeight: 0, overflow: "auto", padding: "0 1.75rem 1.75rem" }}>
          <Typography variant="subtitle2" sx={{ mt: "1rem", mb: "0.5rem" }}>Host</Typography>
          {data.hostError && (
            <Alert severity="warning" sx={{ mb: "1rem" }}>Host stats unavailable: {data.hostError}</Alert>
          )}
          {data.host && (
            <Typography variant="body2" sx={{ mb: "1rem" }}>
              Load average (1/5/15 min): {data.host.loadAverage1.toFixed(2)} / {data.host.loadAverage5.toFixed(2)} / {data.host.loadAverage15.toFixed(2)}
              {" · "}
              Memory: {formatMemory(data.host.memoryUsedMB, data.host.memoryTotalMB)}
            </Typography>
          )}
          {!data.host && !data.hostError && (
            <Typography variant="body2" color="text.secondary" sx={{ mb: "1rem" }}>
              Not configured for this deployment.
            </Typography>
          )}

          <Typography variant="subtitle2" sx={{ mb: "0.5rem" }}>Sandboxes</Typography>
          {(!data.sandboxes || data.sandboxes.length === 0) ? (
            <Typography variant="body2" color="text.secondary">No sandboxes tracked yet.</Typography>
          ) : (
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Slot</TableCell>
                  <TableCell>Backend</TableCell>
                  <TableCell>Name</TableCell>
                  <TableCell>Status</TableCell>
                  <TableCell>Load average</TableCell>
                  <TableCell>Memory</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {data.sandboxes.map((s) => (
                  <TableRow key={s.slot}>
                    <TableCell>{s.slot}</TableCell>
                    <TableCell>{s.backend}</TableCell>
                    <TableCell sx={{ fontFamily: "monospace", fontSize: "0.8rem" }}>{s.name}</TableCell>
                    <TableCell>
                      {s.error
                        ? <Chip size="small" color="error" label={s.error} />
                        : <Chip size="small" color="success" label="ready" />}
                    </TableCell>
                    <TableCell>{s.loadAverage || "—"}</TableCell>
                    <TableCell>{formatMemory(s.memoryUsedMB, s.memoryTotalMB)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      )}
    </main>
  );
}
