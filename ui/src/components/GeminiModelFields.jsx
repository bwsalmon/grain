import { useEffect, useState } from "react";
import {
  Autocomplete,
  Box,
  createFilterOptions,
  MenuItem,
  TextField,
  Typography,
} from "@mui/material";
import api from "../api.js";

// MUI's own default matcher, kept to hand so the exception below can be
// made against it rather than instead of it: what is typed still narrows
// the list, as on any autocomplete.
const matchTyping = createFilterOptions();

// The two values agy is given for a model -- the name and the reasoning
// effort -- as pickers over what the deployment's own agy says it can run
// (grain/task-365).
//
// The model was a plain text box, and a wrong name typed into it was not
// refused where it was typed: agy resolves --model when a run starts, so
// a typo, a model this account cannot reach, or a bare family name
// without an effort surfaced as a dispatch that failed before it did
// anything. Those names are agy's to define, they change when the binary
// is upgraded, and the daemon has the binary right there -- so
// GET /api/agent-models asks it (`agy models`, plus the effort vocabulary
// --help documents) and this offers the answer.
//
// The model stays typable all the same, which is the point of the
// freeSolo autocomplete rather than a Select: reading the catalog needs
// an installed agy, a working credential and quota to spend, and every
// way it can fail -- including on the very pane where a missing key would
// be pasted in -- must still leave an operator able to set a model. The
// options are a convenience over a field that behaves exactly as it
// always did; a name the list does not have is saved unchanged.
//
// The effort is the opposite, and deliberately so: it is three words the
// server validates a save against (ui.UpdateSettings, against
// antigravity.Efforts), so it stays a Select over a vocabulary that
// always saves. What the catalog adds there is narrower and more useful
// than another list of the same three words -- which efforts the *chosen*
// model actually has, read off the ids agy printed, since Gemini 3.1 Pro
// is listed high and low and refuses medium.
export default function GeminiModelFields({
  defaultModel,
  defaultEffort,
  efforts,
}) {
  const [model, setModel] = useState(defaultModel || "");
  // catalog is what the endpoint said, verbatim: available false for a
  // deployment with nothing to ask, error for a lister that was asked and
  // could not answer. Both render the same fields; only the line under
  // them differs, because only one of them is worth acting on.
  const [catalog, setCatalog] = useState(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const resp = (await api("/api/agent-models")) || {};
        if (live) setCatalog(resp);
      } catch (err) {
        // Deliberately not showError: a picker that could not load is
        // not a failed action, and a pane-wide error banner over fields
        // that still work would read as one.
        if (live) setCatalog({ available: true, error: err.message });
      } finally {
        if (live) setLoading(false);
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  const models = (catalog && catalog.models) || [];
  const byID = new Map(models.map((m) => [m.id, m]));
  const known = byID.has(model.trim());
  // The vocabulary the server will accept is the one the server named
  // (Settings.geminiEfforts); the catalog only ever narrows it, never
  // widens it, so no option offered here can be one a save rejects.
  const vocabulary = efforts || [];
  const offered = effortsFor(model, models, vocabulary);

  return (
    <>
      <Autocomplete
        freeSolo
        // freeSolo hides the arrow by default, which would leave the one
        // thing this change is for -- that there is a list to pick from
        // -- discoverable only by clicking into the box and deleting what
        // is in it. It is a drop-down that also takes a written-in value,
        // so it looks like one.
        forcePopupIcon
        options={models.map((m) => m.id)}
        // A field already holding a listed model shows the whole list
        // rather than the one entry its own name matches: with the value
        // in the box as the filter, opening the picker on a configured
        // deployment would otherwise offer nothing but what is already
        // set, and changing model would mean clearing the field first.
        filterOptions={(options, state) =>
          byID.has(state.inputValue.trim())
            ? options
            : matchTyping(options, state)
        }
        // agy's catalog spells the effort into the name, so one model is
        // several entries differing only by suffix -- grouped under the
        // family so the list reads as one row per model.
        groupBy={(id) => (byID.get(id) || {}).family || ""}
        inputValue={model}
        onInputChange={(_evt, next) => setModel(next)}
        renderOption={(props, id) => {
          const { key, ...rest } = props;
          const option = byID.get(id) || {};
          return (
            <Box component="li" key={key} {...rest}>
              <Box>
                <Typography variant="body2">{id}</Typography>
                {option.label && (
                  <Typography variant="caption" color="text.secondary">
                    {option.label}
                  </Typography>
                )}
              </Box>
            </Box>
          );
        }}
        renderInput={(params) => (
          <TextField
            {...params}
            // The name is what SettingsOverlay's own submit reads the
            // value off the form by -- still an ordinary input in an
            // ordinary form, autocomplete or not.
            name="geminiModel"
            label="Gemini model"
            helperText={modelHelperText({ loading, catalog, model, known })}
            autoComplete="off"
            fullWidth
            margin="normal"
          />
        )}
      />
      {/* agy takes the model and the reasoning effort as two values, so
          this is a field of its own rather than part of the name above --
          the same model runs shallow or deep. It is a Select rather than
          a write-in because the three words are what a save is checked
          against, and anything else fails the run it reaches before that
          run starts. Which of them a given model has is the model's own
          business and only agy knows it: with a catalog to read, the
          options are the efforts that model was actually listed with. */}
      <TextField
        select
        name="geminiEffort"
        label="Gemini reasoning effort"
        helperText={effortHelperText({ known, model, offered, vocabulary })}
        defaultValue={defaultEffort || ""}
        fullWidth
        margin="normal"
      >
        {offered.map((effort) => (
          <MenuItem key={effort} value={effort}>
            {effort}
          </MenuItem>
        ))}
      </TextField>
    </>
  );
}

// effortsFor is which efforts to offer beside a model: the ones agy
// listed that model's family under, or -- for a model the catalog does
// not carry, or no catalog at all -- the server's whole vocabulary, since
// there is then nothing narrower that is actually known.
function effortsFor(model, models, vocabulary) {
  const family = (models.find((m) => m.id === model.trim()) || {}).family;
  if (!family) return vocabulary;
  const listed = models
    .filter((m) => m.family === family && m.effort)
    .map((m) => m.effort)
    .filter((effort) => vocabulary.includes(effort));
  return listed.length ? listed : vocabulary;
}

// modelHelperText is the whole of what the model field says about itself,
// and each case is a different thing for an operator to do: wait, ignore,
// fix the deployment, or carry on typing.
function modelHelperText({ loading, catalog, model, known }) {
  if (loading) return "reading the model catalog from agy...";
  if (!catalog || !catalog.available) {
    return "this deployment cannot read agy's model catalog -- type a model name";
  }
  if (catalog.error) {
    return `could not read agy's model catalog (${catalog.error}) -- type a model name`;
  }
  if (model.trim() && !known) {
    return "not in agy's own catalog -- passed to agy exactly as typed, and a name agy does not know fails the next run";
  }
  return "what `agy models` lists on this host. Any other name can be typed in.";
}

// effortHelperText says which of the two lists is being offered, because
// the difference matters: a model whose own catalog entries name two
// efforts genuinely has two, and agy refuses the third before the run
// starts.
function effortHelperText({ known, model, offered, vocabulary }) {
  const ignored =
    "ignored for a model name that already carries one, e.g. gemini-3.1-pro-high";
  if (known && offered.length && offered.length < vocabulary.length) {
    return `${model.trim()} is listed for ${offered.join(" and ")} only; ${ignored}`;
  }
  return ignored;
}
