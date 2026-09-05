import { useEffect, useState } from "react";
import {
  Autocomplete,
  Box,
  createFilterOptions,
  TextField,
  Typography,
} from "@mui/material";
import api from "../api.js";

// MUI's own default matcher, kept to hand so the exception below can be
// made against it rather than instead of it: what is typed still narrows
// the list, as on any autocomplete.
const matchTyping = createFilterOptions();

// The Gemini model setting, as a picker over what the deployment's own
// agy says it can run (grain/task-365).
//
// It was a plain text box, and a wrong name typed into it was not refused
// where it was typed: agy resolves --model when a run starts, so a typo
// or a model this account cannot reach surfaced as a dispatch that failed
// before it did anything. The names are agy's to define, they change when
// that binary is upgraded, and the daemon has the binary right there --
// so GET /api/agent-models asks it (`agy models`, plus the effort
// vocabulary its --help documents) and this offers the answer.
//
// It stays a text box all the same, which is the point of the freeSolo
// autocomplete rather than a Select: the catalog is a fetch that needs a
// binary and a working credential, and every way it can fail -- agy not
// installed, no Gemini key yet, a key out of quota, a listing this build
// cannot parse, a UI not colocated with a daemon at all -- must still
// leave an operator able to set a model. So the options are a
// convenience over a field that behaves exactly as it always did, and a
// name the list does not have is typed in and saved unchanged.
export default function GeminiModelField({ defaultValue }) {
  const [value, setValue] = useState(defaultValue || "");
  // catalog is what the endpoint said, verbatim: available false for a
  // deployment with nothing to ask, error for a lister that was asked and
  // could not answer. Both render the same field; only the line under it
  // differs, because only one of them is worth acting on.
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
        // not a failed action, and a pane-wide error banner over a field
        // that still works would read as one.
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
  const efforts = (catalog && catalog.efforts) || [];
  const byID = new Map(models.map((m) => [m.id, m]));
  const known = byID.has(value.trim());

  return (
    <Autocomplete
      freeSolo
      // freeSolo hides the arrow by default, which would leave the one
      // thing this change is for -- that there is a list to pick from --
      // discoverable only by clicking into the box and deleting what is
      // in it. It is a drop-down that also takes a written-in value, so
      // it looks like one.
      forcePopupIcon
      options={models.map((m) => m.id)}
      // A field already holding a listed model shows the whole list
      // rather than the one entry its own name matches: with the value
      // in the box as the filter, opening the picker on a configured
      // deployment would otherwise offer nothing but what is already
      // set, and switching effort means clearing the field first.
      filterOptions={(options, state) =>
        byID.has(state.inputValue.trim())
          ? options
          : matchTyping(options, state)
      }
      // agy spells the reasoning effort into the model name rather than
      // taking a separate flag, so one family is several entries that
      // differ only by suffix -- grouped under the family so the choice
      // reads as "which model, then how hard it thinks".
      groupBy={(id) => (byID.get(id) || {}).family || ""}
      inputValue={value}
      onInputChange={(_evt, next) => setValue(next)}
      renderOption={(props, id) => {
        const { key, ...rest } = props;
        const model = byID.get(id) || {};
        return (
          <Box component="li" key={key} {...rest}>
            <Box>
              <Typography variant="body2">{id}</Typography>
              {model.label && (
                <Typography variant="caption" color="text.secondary">
                  {model.label}
                </Typography>
              )}
            </Box>
          </Box>
        );
      }}
      renderInput={(params) => (
        <TextField
          {...params}
          // The name is what SettingsOverlay's own submit reads the value
          // off the form by -- the field is still an ordinary input in an
          // ordinary form, autocomplete or not.
          name="geminiModel"
          label="Gemini model"
          helperText={helperText({ loading, catalog, efforts, value, known })}
          autoComplete="off"
          fullWidth
          margin="normal"
        />
      )}
    />
  );
}

// helperText is the whole of what this field says about itself, and each
// case is a different thing for an operator to do: wait, ignore, fix the
// deployment, or carry on typing.
function helperText({ loading, catalog, efforts, value, known }) {
  if (loading) return "reading the model catalog from agy...";
  if (!catalog || !catalog.available) {
    return "this deployment cannot read agy's model catalog -- type a model name";
  }
  if (catalog.error) {
    return `could not read agy's model catalog (${catalog.error}) -- type a model name`;
  }
  const vocabulary = efforts.length
    ? `; reasoning effort is part of the name (${efforts.join(", ")})`
    : "";
  if (value.trim() && !known) {
    return `not in agy's own catalog -- it is passed to agy exactly as typed, and a name agy does not know fails the next run${vocabulary}`;
  }
  return `what \`agy models\` lists on this host${vocabulary}. Any other name can be typed in.`;
}
