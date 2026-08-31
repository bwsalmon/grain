import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Chip, CircularProgress, Table, TableBody, TableCell, TableHead, TableRow, Typography } from "@mui/material";
import api from "../api.js";
import Sparkline from "./Sparkline.jsx";

// REFRESH_MS matches LogsPage's own polling cadence -- the debug section
// (bwsalmon/agents#536) these two panes share should feel like the same
// kind of live view.
const REFRESH_MS = 5000;

// HISTORY_LENGTH caps how many polls' worth of samples each trend chart
// keeps (bwsalmon/agents#566), at 5 minutes of history at REFRESH_MS's own
// cadence -- long enough to see a trend developing, short enough that a
// pane left open overnight does not grow its history without bound.
const HISTORY_LENGTH = 60;

const emptySeries = { cpu: [], mem: [] };

function formatMemory(usedMB, totalMB) {
  if (!totalMB) return "—";
  return `${usedMB} / ${totalMB} MB`;
}

// pushSample appends a value to a capped history array, dropping the
// oldest sample once HISTORY_LENGTH is exceeded. null/undefined/NaN mean
// "no reading this poll" (a sandbox that errored, or host stats that
// failed) and are skipped rather than plotted as zero, which would read
// as a real dip in usage instead of a missing sample.
function pushSample(series, value) {
  if (value === null || value === undefined || Number.isNaN(value)) return series;
  return [...series, value].slice(-HISTORY_LENGTH);
}

// appendHistory folds one GET /api/sandboxes response into the running
// per-host and per-slot history SandboxHealthPage charts from. Load
// average, not a CPU percentage, is what SlotHealth/HostPressure actually
// report (see orchestrator.SlotHealth's own doc comment on why), so the
// "CPU" trend charts below are the 1-minute load average over time, same
// as the existing text summary already shows.
export function appendHistory(prev, result) {
  const host = result?.host
    ? { cpu: pushSample(prev.host.cpu, result.host.loadAverage1), mem: pushSample(prev.host.mem, result.host.memoryUsedMB) }
    : prev.host;

  const sandboxes = { ...prev.sandboxes };
  for (const s of result?.sandboxes || []) {
    const existing = sandboxes[s.slot] || emptySeries;
    const load1 = s.ready && s.loadAverage ? parseFloat(s.loadAverage.split(" ")[0]) : null;
    sandboxes[s.slot] = {
      cpu: pushSample(existing.cpu, load1),
      mem: pushSample(existing.mem, s.ready ? s.memoryUsedMB : null),
    };
  }
  return { host, sandboxes };
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
  const [history, setHistory] = useState({ host: emptySeries, sandboxes: {} });

  const refresh = useCallback(async () => {
    try {
      const result = await api("/api/sandboxes");
      setData(result);
      setHistory((prev) => appendHistory(prev, result));
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
            <>
              <Typography variant="body2" sx={{ mb: "1rem" }}>
                Load average (1/5/15 min): {data.host.loadAverage1.toFixed(2)} / {data.host.loadAverage5.toFixed(2)} / {data.host.loadAverage15.toFixed(2)}
                {" · "}
                Memory: {formatMemory(data.host.memoryUsedMB, data.host.memoryTotalMB)}
              </Typography>
              <div style={{ display: "flex", gap: "2.5rem", marginBottom: "1.5rem" }}>
                <div>
                  <Typography variant="caption" color="text.secondary" component="div">CPU (1 min load average)</Typography>
                  <Sparkline data={history.host.cpu} />
                </div>
                <div>
                  <Typography variant="caption" color="text.secondary" component="div">Memory (MB)</Typography>
                  <Sparkline data={history.host.mem} color="#9c27b0" />
                </div>
              </div>
            </>
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
                  <TableCell>CPU trend</TableCell>
                  <TableCell>Memory</TableCell>
                  <TableCell>Memory trend</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {data.sandboxes.map((s) => {
                  const series = history.sandboxes[s.slot] || emptySeries;
                  return (
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
                      <TableCell><Sparkline data={series.cpu} width={80} height={24} /></TableCell>
                      <TableCell>{formatMemory(s.memoryUsedMB, s.memoryTotalMB)}</TableCell>
                      <TableCell><Sparkline data={series.mem} width={80} height={24} color="#9c27b0" /></TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </div>
      )}
    </main>
  );
}
