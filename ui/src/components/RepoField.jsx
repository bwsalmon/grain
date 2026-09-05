import { useState } from "react";
import ItemGlyph from "./ItemGlyph.jsx";

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
//
// onChange fires on every keystroke of the text path, which is what a
// form collecting an answer to submit later wants and exactly what a
// field that saves each edit as it is made does not: PATCHing per
// keystroke would send "a", "ac", "acm"… on the way to one repo name.
// onCommit is that second reading -- the value somebody has settled on:
// a pick from the list (a choice is complete the moment it is made), or
// the text in the box once it is left or Enter is pressed. Both are
// optional and independent, so a caller can take either reading alone.
export default function RepoField({
  name,
  options,
  defaultValue = "",
  required = false,
  placeholder = "owner/name",
  onChange,
  onCommit,
}) {
  const knownDefault = defaultValue === "" || options.includes(defaultValue);
  const [custom, setCustom] = useState(options.length === 0 || !knownDefault);

  if (custom) {
    return (
      <div className="repo-field">
        <div className="repo-field-control">
          <RepoGlyph />
          <input
            name={name}
            placeholder={placeholder}
            defaultValue={defaultValue}
            required={required}
            autoComplete="off"
            onChange={(e) => onChange?.(e.target.value)}
            onBlur={(e) => onCommit?.(e.target.value)}
            // Enter is swallowed only for a caller that asked for
            // onCommit: in a form (NewTaskOverlay, TemplateOverlay) it is
            // the key that submits, and taking that away from a field
            // whose caller reads the value at submit time would be a
            // change nobody asked for.
            onKeyDown={(e) => {
              if (!onCommit || e.key !== "Enter") return;
              e.preventDefault();
              onCommit(e.target.value);
            }}
          />
        </div>
        {options.length > 0 && (
          <button
            type="button"
            className="link-button"
            onClick={() => setCustom(false)}
          >
            Choose from list
          </button>
        )}
      </div>
    );
  }

  return (
    <div className="repo-field">
      <div className="repo-field-control">
        <RepoGlyph />
        <select
          name={name}
          defaultValue={defaultValue}
          required={required}
          onChange={(e) => {
            if (e.target.value === CUSTOM) {
              setCustom(true);
              return;
            }
            onChange?.(e.target.value);
            onCommit?.(e.target.value);
          }}
        >
          {!required && <option value="">—</option>}
          {options.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
          <option value={CUSTOM}>Other…</option>
        </select>
      </div>
    </div>
  );
}

// The repos figure in front of the field, not on each option: an
// <option> can hold no markup, and this stays a native <select> for the
// semantics its own tests and a phone's own picker rely on. In front of
// the whole control the ring says the same thing once -- what this field
// names -- for a picker whose caption is often the single word "Repo",
// and it is the same figure the Repos page and the read-only repos
// picker beside this one carry (ItemGlyph.jsx).
function RepoGlyph() {
  return (
    <span className="repo-field-icon">
      <ItemGlyph kind="repos" />
    </span>
  );
}
