// Sparkline is a minimal inline SVG line chart, deliberately dependency-free
// (bwsalmon/agents#566: the sandbox health pane wants time-series trends for
// CPU/RAM, and pulling in a charting library for a handful of small line
// charts would be a lot of weight for what a plain <svg> polyline already
// draws). Values are auto-scaled to the given height between the series' own
// min and max -- these are relative trend lines, not absolute-scale charts,
// so callers that need an absolute reference (e.g. "out of total RAM") show
// that as text alongside the chart rather than as part of it.
export default function Sparkline({
  data,
  width = 120,
  height = 32,
  color = "#1976d2",
}) {
  if (!data || data.length < 2) {
    return (
      <svg
        width={width}
        height={height}
        role="img"
        aria-label="Not enough data yet"
      >
        <title>Not enough data yet</title>
      </svg>
    );
  }

  const min = Math.min(...data);
  const max = Math.max(...data);
  const range = max - min || 1;
  const points = data
    .map((v, i) => {
      const x = (i / (data.length - 1)) * width;
      const y = height - ((v - min) / range) * height;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");

  const last = data[data.length - 1];
  return (
    <svg
      width={width}
      height={height}
      role="img"
      aria-label={`Trend, latest value ${last}`}
    >
      <title>{`Trend, latest value ${last}`}</title>
      <polyline points={points} fill="none" stroke={color} strokeWidth="1.5" />
    </svg>
  );
}
