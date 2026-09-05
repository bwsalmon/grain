import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import api from "../api.js";
import Sparkline from "./Sparkline.jsx";

// REFRESH_MS matches LogsPage's own polling cadence -- the System overlay
// (bwsalmon/agents#640) these two panels share should feel like the same
// kind of live view.
const REFRESH_MS = 5000;

// HISTORY_LENGTH caps how many polls' worth of samples each trend chart
// keeps (bwsalmon/agents#566), at 5 minutes of history at REFRESH_MS's own
// cadence -- long enough to see a trend developing, short enough that a
// pane left open overnight does not grow its history without bound.
const HISTORY_LENGTH = 60;

const emptySeries = { cpu: [], mem: [], disk: [] };

// emptyHostSeries is emptySeries' own shape for the host, whose disk
// history is one series per filesystem rather than one series -- the host
// section reports two or three of them now (grain/task-148: the store's
// disk, the sandbox volume, docker's data root), where a sandbox still
// has the one root filesystem.
const emptyHostSeries = { cpu: [], mem: [], disks: {} };

function formatMemory(usedMB, totalMB) {
  if (!totalMB) return "—";
  return `${usedMB} / ${totalMB} MB`;
}

// formatDisk is formatMemory for a figure that arrives in MB but is
// normally counted in GB: a sandbox disk is tens of gigabytes where its
// memory is a couple, and "3174 / 20480 MB" is a worse answer to "how
// full is it" than "3.1 / 20.0 GB". Anything under a gigabyte stays in
// MB, where the same argument runs the other way.
function formatDisk(usedMB, totalMB) {
  if (!totalMB) return "—";
  if (totalMB < 1024) return `${usedMB} / ${totalMB} MB`;
  return `${(usedMB / 1024).toFixed(1)} / ${(totalMB / 1024).toFixed(1)} GB`;
}

// pushSample appends a value to a capped history array, dropping the
// oldest sample once HISTORY_LENGTH is exceeded. null/undefined/NaN mean
// "no reading this poll" (a sandbox that errored, or host stats that
// failed) and are skipped rather than plotted as zero, which would read
// as a real dip in usage instead of a missing sample.
function pushSample(series, value) {
  if (value === null || value === undefined || Number.isNaN(value))
    return series;
  return [...series, value].slice(-HISTORY_LENGTH);
}

// appendDiskHistory folds one poll's host disk figures into the running
// per-filesystem history, keyed by the path each reading was taken
// through -- not by array position, so a chart follows the same
// filesystem across polls even as docker's data root appears in the list
// or drops out of it. A filesystem the host has stopped reporting drops
// its series with it, which is what keeps this from growing without
// bound.
function appendDiskHistory(prev, disks) {
  const next = {};
  for (const d of disks || []) {
    // 0/0 is how both ends spell "no reading available" for one disk (an
    // unreadable path, and see the disk's own `error`), and plotting the
    // 0 would read as an empty disk rather than as a missing sample.
    next[d.path] = pushSample(prev[d.path] || [], d.totalMB ? d.usedMB : null);
  }
  return next;
}

// appendHistory folds one GET /api/sandboxes response into the running
// per-host and per-sandbox history SandboxHealthPage charts from. Load
// average, not a CPU percentage, is what SandboxHealth/HostPressure
// actually report (see orchestrator.SandboxHealth's own doc comment on
// why), so the
// "CPU" trend charts below are the 1-minute load average over time, same
// as the existing text summary already shows.
export function appendHistory(prev, result) {
  const host = result?.host
    ? {
        cpu: pushSample(prev.host.cpu, result.host.loadAverage1),
        mem: pushSample(prev.host.mem, result.host.memoryUsedMB),
        disks: appendDiskHistory(prev.host.disks, result.host.disks),
      }
    : prev.host;

  const sandboxes = { ...prev.sandboxes };
  for (const s of result?.sandboxes || []) {
    const existing = sandboxes[s.sandbox] || emptySeries;
    const load1 =
      s.ready && s.loadAverage ? parseFloat(s.loadAverage.split(" ")[0]) : null;
    sandboxes[s.sandbox] = {
      cpu: pushSample(existing.cpu, load1),
      mem: pushSample(existing.mem, s.ready ? s.memoryUsedMB : null),
      disk: pushSample(
        existing.disk,
        s.ready && s.diskTotalMB ? s.diskUsedMB : null,
      ),
    };
  }
  return { host, sandboxes };
}

// HostDisks is the host section's disk figures: one row per filesystem
// the daemon's own state sits on, rather than the single "Disk: x / y GB"
// this showed while that state was assumed to be one volume.
//
// It is a table rather than another entry in the text line above because
// there is now more than one number and each needs saying which disk it
// is: a deployment sized the way terraform/gcp is has a small store disk
// that stays near-empty beside a large sandbox volume that a runaway
// build fills, and it was the store's figure alone the pane used to show
// (grain/task-148). The "Holds" column is what of grain's own state
// lives there -- two words in one row when the daemon found them to be
// the same filesystem, which is the ordinary case on a single-disk host.
function HostDisks({ disks, history }) {
  if (!disks || disks.length === 0) {
    return (
      <Typography variant="body2" color="text.secondary" sx={{ mb: "1rem" }}>
        No disk figures available.
      </Typography>
    );
  }
  return (
    <Table size="small" sx={{ mb: "1.5rem", maxWidth: "44rem" }}>
      <TableHead>
        <TableRow>
          <TableCell>Holds</TableCell>
          <TableCell>Path</TableCell>
          <TableCell>Usage</TableCell>
          <TableCell>Trend</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {disks.map((d) => (
          <TableRow key={d.path}>
            <TableCell>{(d.holds || []).join(", ")}</TableCell>
            <TableCell sx={{ fontFamily: "monospace", fontSize: "0.8rem" }}>
              {d.path}
            </TableCell>
            <TableCell>
              {/* A filesystem that has stopped answering says why rather
                  than showing a bare dash. This is the reading an
                  operator is here for, and "the sandbox volume is no
                  longer mounted" is the answer they want -- not a row
                  that has simply gone quiet. */}
              {d.error ? (
                <Chip size="small" color="warning" label={d.error} />
              ) : (
                formatDisk(d.usedMB, d.totalMB)
              )}
            </TableCell>
            <TableCell>
              <Sparkline
                data={history[d.path] || []}
                width={80}
                height={24}
                color="#2e7d32"
              />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// SandboxHealthPage is the Sandbox health panel of SystemOverlay.jsx -- it
// used to be a full nav entry of its own (bwsalmon/agents#536), then a
// tab inside Settings alongside Logs and the reboot control (bwsalmon/
// agents#623), before settling on a sidebar entry of its own,
// apart from Settings (bwsalmon/agents#640). It shows a live view of
// every live run's own sandbox -- a kontur VM or a host directory,
// whichever backend this deployment runs -- plus the daemon's own host
// machine's CPU/RAM/disk pressure. Both come back from the same
// GET /api/sandboxes call since a sandbox that looks stuck is often
// really the host it runs on being starved, so debugging one usually
// means looking at both at once. GET /api/sandboxes' own "enabled" flag
// says whether this deployment has either piece configured at all, the
// same convention LogsPage's own GET /api/logs already establishes for
// the System overlay this panel sits alongside.
export default function SandboxHealthPage({ showError }) {
  const [data, setData] = useState(null);
  const [history, setHistory] = useState({
    host: emptyHostSeries,
    sandboxes: {},
  });

  const refresh = useCallback(async () => {
    try {
      const result = await api("/api/sandboxes");
      setData(result);
      setHistory((prev) => appendHistory(prev, result));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  useEffect(() => {
    const interval = setInterval(refresh, REFRESH_MS);
    return () => clearInterval(interval);
  }, [refresh]);

  return (
    <section className="sandbox-health-panel">
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          mb: 1,
        }}
      >
        <Typography variant="subtitle2">Sandbox health</Typography>
        {data?.enabled && (
          <Button size="small" variant="outlined" onClick={refresh}>
            Refresh
          </Button>
        )}
      </Box>
      {data === null && (
        <Box sx={{ display: "flex", justifyContent: "center", py: 3 }}>
          <CircularProgress size={28} aria-label="Loading sandbox health" />
        </Box>
      )}
      {data !== null && !data.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this deployment has no sandbox pool or host stats
          configured.
        </Alert>
      )}
      {data !== null && data.enabled && (
        <Box>
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ mb: "0.5rem" }}
          >
            Host
          </Typography>
          {data.hostError && (
            <Alert severity="warning" sx={{ mb: "1rem" }}>
              Host stats unavailable: {data.hostError}
            </Alert>
          )}
          {data.host && (
            <>
              <Typography variant="body2" sx={{ mb: "1rem" }}>
                Load average (1/5/15 min): {data.host.loadAverage1.toFixed(2)} /{" "}
                {data.host.loadAverage5.toFixed(2)} /{" "}
                {data.host.loadAverage15.toFixed(2)}
                {" · "}
                Memory:{" "}
                {formatMemory(data.host.memoryUsedMB, data.host.memoryTotalMB)}
              </Typography>
              <div
                style={{
                  display: "flex",
                  gap: "2.5rem",
                  marginBottom: "1.5rem",
                }}
              >
                <div>
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    component="div"
                  >
                    CPU (1 min load average)
                  </Typography>
                  <Sparkline data={history.host.cpu} />
                </div>
                <div>
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    component="div"
                  >
                    Memory (MB)
                  </Typography>
                  <Sparkline data={history.host.mem} color="#9c27b0" />
                </div>
              </div>
              <HostDisks disks={data.host.disks} history={history.host.disks} />
            </>
          )}
          {!data.host && !data.hostError && (
            <Typography
              variant="body2"
              color="text.secondary"
              sx={{ mb: "1rem" }}
            >
              Not configured for this deployment.
            </Typography>
          )}

          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ mb: "0.5rem" }}
          >
            Sandboxes
          </Typography>
          {!data.sandboxes || data.sandboxes.length === 0 ? (
            <Typography variant="body2" color="text.secondary">
              No runs in flight, so no sandboxes exist.
            </Typography>
          ) : (
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Run</TableCell>
                  <TableCell>Backend</TableCell>
                  <TableCell>Name</TableCell>
                  <TableCell>Status</TableCell>
                  <TableCell>Load average</TableCell>
                  <TableCell>CPU trend</TableCell>
                  <TableCell>Memory</TableCell>
                  <TableCell>Memory trend</TableCell>
                  <TableCell>Disk</TableCell>
                  <TableCell>Disk trend</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {data.sandboxes.map((s) => {
                  const series = history.sandboxes[s.sandbox] || emptySeries;
                  return (
                    <TableRow key={s.sandbox}>
                      <TableCell>{s.sandbox}</TableCell>
                      <TableCell>{s.backend}</TableCell>
                      <TableCell
                        sx={{ fontFamily: "monospace", fontSize: "0.8rem" }}
                      >
                        {s.name}
                      </TableCell>
                      <TableCell>
                        {s.error ? (
                          <Chip size="small" color="error" label={s.error} />
                        ) : (
                          <Chip size="small" color="success" label="ready" />
                        )}
                      </TableCell>
                      <TableCell>{s.loadAverage || "—"}</TableCell>
                      <TableCell>
                        <Sparkline data={series.cpu} width={80} height={24} />
                      </TableCell>
                      <TableCell>
                        {formatMemory(s.memoryUsedMB, s.memoryTotalMB)}
                      </TableCell>
                      <TableCell>
                        <Sparkline
                          data={series.mem}
                          width={80}
                          height={24}
                          color="#9c27b0"
                        />
                      </TableCell>
                      <TableCell>
                        {formatDisk(s.diskUsedMB, s.diskTotalMB)}
                      </TableCell>
                      <TableCell>
                        <Sparkline
                          data={series.disk}
                          width={80}
                          height={24}
                          color="#2e7d32"
                        />
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </Box>
      )}
    </section>
  );
}
