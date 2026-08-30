import { useState } from "react";

// CUSTOM is the select's escape hatch, not a real option a repo could
// itself be named -- model.ParseRepo requires a "/", which this value
// deliberately has none of, so it can never collide with one.
const CUSTOM = "custom";

// RepoField is the repo dropdown bwsalmon/agents#447 asked for in place
// of a bare "type an owner/name and hope you spelled it right" input:
// options is normally knownRepos(config, tasks) (state.js), the same
// repos CreateTask would actually accept plus whatever existing tasks
// already target. Typing stays reachable through "Other…", since
// options is never the full universe of valid repos -- an unrestricted
// deployment's very first task, or a restricted one CreateTask hasn't
// widened targetRepos for yet, both need a way to name a repo nothing
// has used before.
//
// Falls back to a plain text input outright when there is nothing to
// choose from yet, rather than rendering a one-item "Other…" dropdown
// that would only cost a click for no benefit.
export default function RepoField({ name, options, defaultValue = "", required = false, placeholder = "owner/name" }) {
  const knownDefault = defaultValue === "" || options.includes(defaultValue);
  const [custom, setCustom] = useState(options.length === 0 || !knownDefault);

  if (custom) {
    return (
      <>
        <input name={name} placeholder={placeholder} defaultValue={defaultValue} required={required} autoComplete="off" />
        {options.length > 0 && (
          <button type="button" className="link-button" onClick={() => setCustom(false)}>
            Choose from list
          </button>
        )}
      </>
    );
  }

  return (
    <select
      name={name}
      defaultValue={defaultValue}
      required={required}
      onChange={(e) => { if (e.target.value === CUSTOM) setCustom(true); }}
    >
      {!required && <option value="">—</option>}
      {options.map((r) => (
        <option key={r} value={r}>{r}</option>
      ))}
      <option value={CUSTOM}>Other…</option>
    </select>
  );
}
