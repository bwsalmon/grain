import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert, Box, Button, Chip, CircularProgress, FormControl, InputLabel, Link,
  MenuItem, Select, Table, TableBody, TableCell, TableHead, TableRow, Tooltip, Typography,
} from "@mui/material";
import api from "../api.js";
import Sparkline from "./Sparkline.jsx";
import { STATE_LABELS, STATE_ORDER } from "../state.js";

// WINDOWS are the ?window= values this pane offers. They are the strings
// ui.ParseMetricsWindow already accepts (a Go duration, or a count of
// days or weeks), sent through verbatim rather than converted to
// seconds, so the picker and `grain metrics -window` speak the same
// vocabulary and a window read off one can be typed into the other.
export const WINDOWS = [
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "90d", label: "Last 90 days" },
];

// DEFAULT_WINDOW matches ui.DefaultMetricsWindow, so the first report
// anybody sees here is the same one `grain metrics` prints with no flags.
const DEFAULT_WINDOW = "7d";

// BUCKETS is how many points the trend lines carry -- twice pkg/metrics'
// own default of 12, because a sparkline is drawn from the shape of the
// series rather than read off point by point, and 24 is enough for that
// without the spans getting so short that a quiet deployment's line is
// mostly zeroes.
const BUCKETS = 24;

// Unlike the Logs and Sandbox health panels on the Debug pane, there is
// no poll here: a report costs a full scan of `task` and `task_run` every
// time it is asked for (README, "Measuring throughput and latency"), and
// nothing it shows moves fast enough to be worth that every few seconds.
// It loads once, reloads when the window changes, and otherwise waits for
// the Refresh button.

// formatSeconds renders one of the report's own second counts as a
// duration, the same reasoning as `grain metrics`' own `seconds`: a p50
// shown as "21m 30s" is read at a glance where "1290.4" is arithmetic
// homework. Coarser the longer it gets -- nobody reading a three-hour
// lead time needs the seconds on the end of it.
export function formatSeconds(value) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—";
  const s = Math.round(value);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

// isThinPercentile says whether a stage has too few samples for one of
// its percentiles to mean what its name says. A percentile only tells
// you something the maximum did not once at least one sample can fall
// in the tail it names, which takes 100/(100-p) of them: 2 for a p50,
// 10 for a p90, 100 for a p99. Below that the number *is* the maximum
// wearing a percentile's name -- exactly what pkg/ui's own MetricsStage
// comment warns about, and why `n` is a column here rather than a
// detail.
//
// The percentile is passed as a whole number (50, 90, 99) rather than a
// fraction so the threshold comes out exact: 1/(1-0.9) is 10.000000...2
// in floating point, which would quietly call a ten-sample p90 thin.
//
// A thin percentile is still the largest thing that happened, so the
// table dims and footnotes it rather than blanking it out.
export function isThinPercentile(n, percentile) {
  return n > 0 && n < 100 / (100 - percentile);
}

// sortedOutcomes puts the attempt outcomes in a stable order -- by count
// descending so the ending that dominates a window reads first, then by
// name so two renders of the same report never disagree. The same order
// `grain metrics`' own outcomeSummary prints them in.
export function sortedOutcomes(outcomes) {
  return Object.entries(outcomes || {}).sort((a, b) => (b[1] - a[1]) || a[0].localeCompare(b[0]));
}

// formatBytes renders a result size the way somebody sizing a truncation
// cap reads one. Binary units, because the cap they would set is written
// in them (mcp.maxToolResultBytes is 64 << 10), and no decimals below a
// megabyte: the difference between 63 KB and 64 KB matters and the
// difference between 63.4 and 63.5 does not.
export function formatBytes(value) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—";
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

// percent renders a rate (0..1) as whole percent. Rates here are read
// against each other -- edit_file's against run_command's -- rather than
// to a decimal place.
export function percent(rate) {
  if (!rate) return "0%";
  return `${Math.round(rate * 100)}%`;
}

// backlogRows orders the backlog by STATE_ORDER -- model.StateOf's own
// precedence, the order the sidebar already lists states in -- with any
// state this UI does not know about appended alphabetically rather than
// dropped, so a state added server-side still shows up here.
export function backlogRows(byState) {
  const entries = Object.entries(byState || {}).filter(([, n]) => n > 0);
  const known = STATE_ORDER.filter((s) => entries.some(([state]) => state === s));
  const unknown = entries.map(([state]) => state).filter((s) => !STATE_ORDER.includes(s)).sort();
  return [...known, ...unknown].map((state) => ({ state, count: byState[state] }));
}

function Stat({ label, value, sub, title }) {
  const body = (
    <Box>
      <Typography variant="h6" component="div" sx={{ fontVariantNumeric: "tabular-nums", lineHeight: 1.2 }}>{value}</Typography>
      <Typography variant="caption" color="text.secondary" component="div">{label}</Typography>
      {sub && <Typography variant="caption" color="text.secondary" component="div">{sub}</Typography>}
    </Box>
  );
  return title ? <Tooltip title={title}>{body}</Tooltip> : body;
}

// Trend is one bucketed series drawn as a line. The series is unlabelled
// on its own axis on purpose: Sparkline auto-scales to the series' own
// min and max, so these say "which way, and how steadily" rather than
// "how many" -- the count beside them is where an absolute number comes
// from.
function Trend({ label, data, color }) {
  return (
    <Box>
      <Typography variant="caption" color="text.secondary" component="div">{label}</Typography>
      <Sparkline data={data} color={color} />
    </Box>
  );
}

// Cell renders one percentile of one stage, dimmed and asterisked when
// the stage does not carry enough samples to support it (isThinPercentile).
function Cell({ stage, percentile, seconds }) {
  if (stage.n === 0) return <TableCell align="right">—</TableCell>;
  const thin = isThinPercentile(stage.n, percentile);
  const text = formatSeconds(seconds);
  return (
    <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums", color: thin ? "text.disabled" : "text.primary" }}>
      {thin ? (
        <Tooltip title={`Only ${stage.n} sample${stage.n === 1 ? "" : "s"} — at this count this is the maximum under another name, not a p${percentile}.`}>
          <span>{text}*</span>
        </Tooltip>
      ) : text}
    </TableCell>
  );
}

// MetricsPage is the body of MetricsOverlay.jsx: what this deployment
// delivered over a window and where a task's wall-clock time went
// getting there, from GET /api/metrics (pkg/metrics computes it;
// `grain metrics` prints the same report at a terminal). It is a
// destination of its own on the sidebar rather than a Settings tab
// because it is a read-only view of how the deployment is behaving, not
// a knob on it -- and its own entry rather than a Debug tab because it
// is the question asked when nothing is wrong (grain/task-173).
//
// The pane's header is what carries the "Metrics" title; this panel
// starts straight in on the window picker and Refresh.
//
// Two things the README's own "Measuring throughput and latency" section
// insists on are presentation decisions here, not just wording:
//
//   - The latency stages do not sum to the lead time and are not meant
//     to. A task can sit in awaiting_reply for a day, back off between
//     attempts, or wait on a dependency, and none of those is a stage
//     anything records the start of. So they are a table of independent
//     distributions, never a stacked bar, which would draw a claim about
//     them adding up that the numbers do not make.
//
//   - The backlog is a gauge read at the report's own moment, not
//     something measured over the window, so it is a separate section
//     with its own heading saying exactly that -- next to utilization,
//     which is the number it has to be read against (work waiting while
//     capacity sat idle is a scheduling problem, not a capacity one).
export default function MetricsPage({ showError, onOpenTask }) {
  const [selectedWindow, setSelectedWindow] = useState(DEFAULT_WINDOW);
  const [report, setReport] = useState(null);
  const [loading, setLoading] = useState(false);
  // Every fetch carries a sequence number and only the newest one is
  // allowed to land: changing the window twice quickly otherwise races,
  // and a slow report for a window nobody is looking at any more would
  // overwrite the one they are.
  const seq = useRef(0);

  const refresh = useCallback(async () => {
    const mine = ++seq.current;
    setLoading(true);
    try {
      const result = await api(`/api/metrics?window=${encodeURIComponent(selectedWindow)}&buckets=${BUCKETS}`);
      if (seq.current === mine) setReport(result);
    } catch (err) {
      showError(err);
    } finally {
      if (seq.current === mine) setLoading(false);
    }
  }, [selectedWindow, showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const buckets = report?.throughput?.buckets || [];
  const latency = report?.latency || [];
  const measured = latency.some((s) => s.n > 0);
  const thin = latency.some((s) => isThinPercentile(s.n, 90) || isThinPercentile(s.n, 99));
  const outcomes = sortedOutcomes(report?.runs?.outcomes);
  const endings = sortedOutcomes(report?.runs?.endings);
  const backlog = backlogRows(report?.backlog?.byState);
  // Both sections describe the inside of a run, and both are absent
  // rather than zeroed on a deployment whose runs predate the census --
  // "nobody measured this" is not "the tools never failed", so neither
  // renders at all until something recorded one.
  const tools = report?.tools;
  const checks = report?.checks;
  const verdicts = sortedOutcomes(checks?.verdicts);
  const oldestTaskId = report?.backlog?.oldestQueuedTaskId;

  return (
    <section className="metrics-panel">
      <Box sx={{ display: "flex", alignItems: "center", justifyContent: "flex-end", gap: 1, mb: 1.5 }}>
        <Box sx={{ display: "flex", alignItems: "center", gap: 1 }}>
          <FormControl size="small" sx={{ minWidth: 160 }}>
            <InputLabel id="metrics-window-label">Window</InputLabel>
            <Select
              labelId="metrics-window-label"
              label="Window"
              value={selectedWindow}
              onChange={(e) => setSelectedWindow(e.target.value)}
            >
              {WINDOWS.map((w) => <MenuItem key={w.value} value={w.value}>{w.label}</MenuItem>)}
            </Select>
          </FormControl>
          <Button size="small" variant="outlined" onClick={refresh} disabled={loading}>Refresh</Button>
        </Box>
      </Box>

      {report === null ? (
        <Box sx={{ display: "flex", justifyContent: "center", py: 3 }}>
          <CircularProgress size={28} aria-label="Loading metrics" />
        </Box>
      ) : (
        <Box>
          <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 2 }}>
            {new Date(report.since).toLocaleString()} → {new Date(report.until).toLocaleString()}
            {" · "}
            {formatSeconds(report.windowSeconds)}
          </Typography>

          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>Throughput</Typography>
          <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3, mb: 1.5 }}>
            <Stat label="Tasks filed" value={report.throughput.tasksFiled} sub={`${report.throughput.filedPerDay.toFixed(1)}/day`} />
            <Stat label="Tasks completed" value={report.throughput.tasksCompleted} sub={`${report.throughput.completedPerDay.toFixed(1)}/day`} />
            <Stat label="Tasks closed" value={report.throughput.tasksClosed} />
            <Stat label="Attempts started" value={report.throughput.runsStarted} />
            <Stat label="Attempts finished" value={report.throughput.runsFinished} sub={`${report.throughput.runsFinishedPerDay.toFixed(1)}/day`} />
            <Stat
              label="Attempts per completion"
              value={report.runs.attemptsPerCompletion.toFixed(2)}
              title="How many attempts an average completed task needed. Above 1, something is being retried."
            />
          </Box>
          <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3, mb: 1 }}>
            <Trend label="Filed" data={buckets.map((b) => b.filed)} />
            <Trend label="Completed" data={buckets.map((b) => b.completed)} color="#2e7d32" />
            <Trend label="Attempts finished" data={buckets.map((b) => b.runsFinished)} color="#9c27b0" />
          </Box>
          <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 2.5 }}>
            Oldest to newest across the window. Each line is scaled to its own range, so these are shapes, not counts.
          </Typography>

          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>Capacity</Typography>
          <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3, mb: 1 }}>
            <Stat
              label="Mean concurrent runs"
              value={report.runs.maxConcurrent > 0
                ? `${report.runs.meanConcurrent.toFixed(2)} of ${report.runs.maxConcurrent}`
                : report.runs.meanConcurrent.toFixed(2)}
              sub={report.runs.maxConcurrent > 0 ? `${Math.round(report.runs.utilization * 100)}% of the limit` : "no concurrency limit configured"}
            />
            <Stat label="Running right now" value={report.runs.live} />
          </Box>
          {outcomes.length > 0 && (
            <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.7, mb: endings.length > 0 ? 1 : 2.5 }}>
              {outcomes.map(([name, count]) => (
                <Chip key={name} size="small" variant="outlined" label={`${name} ${count}`} />
              ))}
            </Box>
          )}
          {endings.length > 0 && (
            <>
              <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 0.5 }}>
                …and how those attempts actually ended. The outcome words above cover more than one ending each:
                “cancelled” is both a human closing the task and a run hitting its wall-clock cap, and “failed” is both a
                broken framework and a run that used up its turns. Each has a different fix.
              </Typography>
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.7, mb: 2.5 }}>
                {endings.map(([name, count]) => (
                  <Chip key={name} size="small" variant="outlined" label={`${name} ${count}`} />
                ))}
              </Box>
            </>
          )}

          <Typography variant="body2" color="text.secondary" sx={{ mb: 0.5 }}>Latency</Typography>
          <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 1 }}>
            Stages that <em>ended</em> inside the window, in the order a task passes through them. Each is measured on its
            own and a missing moment is skipped rather than guessed, so their <code>n</code> legitimately differ and the
            stages do not add up to the lead time — a task can sit awaiting a reply, back off between attempts, or wait on
            a dependency, and none of that is a stage anything records the start of.
          </Typography>
          {!measured ? (
            <Typography variant="body2" color="text.secondary" sx={{ mb: 2.5 }}>
              Nothing finished inside this window, so there is no latency to report yet. Try a longer window.
            </Typography>
          ) : (
            <>
              <Table size="small" sx={{ mb: 0.5 }}>
                <TableHead>
                  <TableRow>
                    <TableCell>Stage</TableCell>
                    <TableCell align="right">n</TableCell>
                    <TableCell align="right">p50</TableCell>
                    <TableCell align="right">p90</TableCell>
                    <TableCell align="right">p99</TableCell>
                    <TableCell align="right">max</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {/* No re-sorting: the API sends the stages in the order
                      a task passes through them, which is the order they
                      are read in. min and mean are left to the API and
                      `grain metrics -json` -- the shape worth acting on
                      here is the median, the tails and how many samples
                      are behind them. */}
                  {latency.map((s) => (
                    <TableRow key={s.stage}>
                      <TableCell>
                        <Tooltip title={s.description}><span>{s.label}</span></Tooltip>
                      </TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>{s.n}</TableCell>
                      <Cell stage={s} percentile={50} seconds={s.p50Seconds} />
                      <Cell stage={s} percentile={90} seconds={s.p90Seconds} />
                      <Cell stage={s} percentile={99} seconds={s.p99Seconds} />
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>
                        {s.n === 0 ? "—" : formatSeconds(s.maxSeconds)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {thin && (
                <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 2.5 }}>
                  * Fewer samples than the percentile needs to mean anything: at this <code>n</code> it is the maximum
                  under another name.
                </Typography>
              )}
            </>
          )}

          {tools?.calls > 0 && (
            <>
              <Typography variant="body2" color="text.secondary" sx={{ mb: 0.5, mt: 2.5 }}>Tool use</Typography>
              <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 1 }}>
                What the window's runs spent their turns on, over the {tools.runs} attempt{tools.runs === 1 ? "" : "s"}{" "}
                that recorded it. An errored call is an ordinary turn of an agent's loop rather than a broken run, so
                these rates are never zero — what they are for is the trend, and the comparison between tools.
              </Typography>
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3, mb: 1 }}>
                <Stat label="Tool calls" value={tools.calls} sub={`${tools.callsPerRun.p50} per run at the median`} />
                <Stat label="Errored calls" value={tools.errored} sub={`${percent(tools.erroredShare)} of all calls`} />
              </Box>
              <Table size="small" sx={{ mb: 0.5 }}>
                <TableHead>
                  <TableRow>
                    <TableCell>Tool</TableCell>
                    <TableCell align="right">runs</TableCell>
                    <TableCell align="right">calls</TableCell>
                    <TableCell align="right">errors</TableCell>
                    <TableCell align="right">timed out</TableCell>
                    <TableCell align="right">mean result</TableCell>
                    <TableCell align="right">p95 result</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {tools.byTool.map((use) => (
                    <TableRow key={use.name}>
                      <TableCell>{use.name}</TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>{use.runs}</TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>{use.calls}</TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>
                        {use.errored} ({percent(use.errorRate)})
                      </TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>
                        {use.timedOut > 0 ? `${use.timedOut} (${percent(use.timeoutRate)})` : "—"}
                      </TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>
                        {formatBytes(use.resultBytes.meanBytes)}
                      </TableCell>
                      <TableCell align="right" sx={{ fontVariantNumeric: "tabular-nums" }}>
                        <Tooltip title="An upper bound: sizes are kept in base-2 buckets, so the real number is inside the octave below this. It is what should size the cap on a tool result.">
                          <span>≤ {formatBytes(use.resultBytes.p95AtMostBytes)}</span>
                        </Tooltip>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </>
          )}

          {checks?.waits > 0 && (
            <>
              <Typography variant="body2" color="text.secondary" sx={{ mb: 0.5, mt: 2.5 }}>CI waits</Typography>
              <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 1 }}>
                The loop every run is told to go round: push, wait for checks, fix, push again.{" "}
                {checks.waits} wait{checks.waits === 1 ? "" : "s"} across {checks.runs} attempt
                {checks.runs === 1 ? "" : "s"}. Mostly <code>timed_out</code> means the wait's own default is set wrong
                for this CI; mostly <code>no_checks</code> means runs are being sent to wait for CI that does not exist.
              </Typography>
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.7, mb: 1 }}>
                {verdicts.map(([name, count]) => (
                  <Chip key={name} size="small" variant="outlined" label={`${name} ${count}`} />
                ))}
              </Box>
              <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3 }}>
                <Stat
                  label="Blocked, median"
                  value={formatSeconds(checks.blocked.p50Seconds)}
                  sub={`p90 ${formatSeconds(checks.blocked.p90Seconds)} · max ${formatSeconds(checks.blocked.maxSeconds)}`}
                />
                {checks.greenRuns > 0 && (
                  <Stat
                    label="Pushes before green"
                    value={checks.pushesToGreen.mean.toFixed(1)}
                    sub={`${checks.pushesToGreen.max} at worst, over ${checks.greenRuns} attempt${checks.greenRuns === 1 ? "" : "s"} that went green`}
                    title="How many pushes a run had made when its first passing wait returned. 1.0 is CI right first time; the distance above it is what the loop costs."
                  />
                )}
              </Box>
            </>
          )}

          <Typography variant="body2" color="text.secondary" sx={{ mb: 0.5, mt: 2.5 }}>Backlog</Typography>
          <Typography variant="caption" color="text.secondary" component="div" sx={{ mb: 1 }}>
            Right now, not over the window — what was still unfinished at the moment this report was taken. Read it
            against the capacity above: work waiting while capacity sat idle is a scheduling problem, not a capacity one.
          </Typography>
          {backlog.length === 0 ? (
            <Typography variant="body2" color="text.secondary">Nothing unfinished — the backlog is empty.</Typography>
          ) : (
            <Box sx={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 0.7 }}>
              {backlog.map(({ state, count }) => (
                <Chip
                  key={state}
                  size="small"
                  variant="outlined"
                  icon={<span className={`dot dot-${state}`} style={{ marginLeft: 8 }} />}
                  label={`${STATE_LABELS[state] || state} ${count}`}
                />
              ))}
            </Box>
          )}
          {oldestTaskId && (
            <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
              Oldest queued:{" "}
              <Link component="button" type="button" onClick={() => onOpenTask?.(oldestTaskId)}>
                task {oldestTaskId}
              </Link>
              , waiting {formatSeconds(report.backlog.oldestQueuedSeconds)}.
            </Typography>
          )}

          {report.throughput.tasksFiled === 0 && report.throughput.runsStarted === 0 && (
            <Alert severity="info" sx={{ mt: 2 }}>
              Nothing was filed or attempted inside this window. Nothing is stored, so a report is only ever computed from
              rows that still exist — a longer window is the thing to try first.
            </Alert>
          )}
        </Box>
      )}
    </section>
  );
}
