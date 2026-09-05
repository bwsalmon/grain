// Formatting an absolute time, in the zone the *deployment* keeps rather
// than the one this browser happens to be in (grain/task-368).
//
// Every timestamp on screen used to be `new Date(iso).toLocaleString()`,
// which is the browser's own zone. That reads fine on a laptop sitting
// next to the daemon and badly everywhere else: a schedule fires on the
// deployment's clock (model.Config.TimeZone, which
// orchestrator.reconcileSchedule reads), so a "next run" printed on the
// reader's clock and a time-of-day typed into a schedule form were two
// different clocks with nothing on screen to say so. The zone comes from
// GET /api/config, is handed down through TimeZoneContext, and is what
// the four helpers here format against.
//
// An empty zone means "whatever this browser is in", which is what every
// one of these did before and what a test rendering a component outside
// a provider still gets.

// formatDateTime is the date and time of an ISO timestamp (or a Date, or
// anything the Date constructor takes), in zone.
//
// A value that isn't a time at all comes back as the empty string rather
// than "Invalid Date": these are printed inside sentences ("started ...")
// where a browser's own words for a parse failure would read as data.
export function formatDateTime(value, zone) {
  return format(value, zone, {});
}

// formatTime is the clock time alone -- the one thing a countdown's own
// "until" needs, where the date is either today or already said.
export function formatTime(value, zone) {
  return format(value, zone, {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
  });
}

function format(value, zone, options) {
  const at = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  const opts = { ...options };
  if (zone) opts.timeZone = zone;
  try {
    return at.toLocaleString(undefined, opts);
  } catch {
    // An unknown zone throws a RangeError rather than falling back. The
    // daemon validates the name before storing it, so this is a browser
    // whose ICU data is older than the zone somebody chose -- the time
    // is still worth printing, in the only zone this browser has.
    delete opts.timeZone;
    return at.toLocaleString(undefined, opts);
  }
}

// zoneAbbreviation is what a clock in this zone is *called* right now --
// "PDT", "GMT+2" -- for labelling a field whose value is a bare time of
// day. Empty when the browser cannot say, so a caller can leave the
// label alone rather than print a gap.
export function zoneAbbreviation(zone, at = new Date()) {
  if (!zone) return "";
  try {
    const parts = new Intl.DateTimeFormat(undefined, {
      timeZone: zone,
      timeZoneName: "short",
    }).formatToParts(at);
    return parts.find((p) => p.type === "timeZoneName")?.value || "";
  } catch {
    return "";
  }
}

// browserTimeZone is the zone this browser is in, for offering "the one
// you are actually looking at this from" as a choice. Empty if the
// browser will not say.
export function browserTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    return "";
  }
}

// FALLBACK_TIME_ZONES is the selector's vocabulary on a browser with no
// Intl.supportedValuesOf (which is every one of them older than 2022):
// enough zones to cover where a single-operator deployment plausibly
// runs, rather than a list nobody can complete by hand. A browser that
// does have it gets the whole IANA database instead, so this is a floor
// and not the offer.
export const FALLBACK_TIME_ZONES = [
  "UTC",
  "America/Los_Angeles",
  "America/Denver",
  "America/Chicago",
  "America/New_York",
  "America/Sao_Paulo",
  "Europe/London",
  "Europe/Berlin",
  "Europe/Moscow",
  "Asia/Dubai",
  "Asia/Kolkata",
  "Asia/Singapore",
  "Asia/Tokyo",
  "Australia/Sydney",
  "Pacific/Auckland",
];

// timeZoneOptions is every zone the selector offers, sorted, with
// `current` guaranteed to be among them -- a deployment set to a zone
// this browser has never heard of must still show what it is set to
// rather than silently reading as some other zone the moment somebody
// opens Settings.
export function timeZoneOptions(current = "") {
  let zones = [];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    zones = [];
  }
  if (!zones.length) zones = FALLBACK_TIME_ZONES;
  const all = new Set(zones);
  all.add("UTC");
  if (current) all.add(current);
  return [...all].sort();
}
