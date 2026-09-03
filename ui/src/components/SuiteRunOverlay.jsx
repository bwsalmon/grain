import { useState } from "react";
import { Box, Button, FormControl, InputLabel, MenuItem, Select, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";
import RepoField from "./RepoField.jsx";

// SuiteRunOverlay starts a suite running against a repo and branch
// (bwsalmon/agents#642's own "run the template against a repo and
// branch") -- opened from a suite row's own "Run" action, with that
// suite preselected but still changeable, since a suite is meant to be
// run against more than one repo/branch over its life.
export default function SuiteRunOverlay({ suites, suiteId, repoOptions, onClose, onStarted, showError }) {
  const [selectedSuiteId, setSelectedSuiteId] = useState(suiteId || (suites[0] && suites[0].id) || "");

  const submit = async (evt) => {
    evt.preventDefault();
    const data = new FormData(evt.target);
    const payload = {
      suiteId: selectedSuiteId,
      repo: data.get("repo") || "",
      base: data.get("base") || "",
    };
    try {
      await api("/api/suite-runs", { method: "POST", body: JSON.stringify(payload) });
      await onStarted();
      onClose();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Run a suite</Typography>
      <form onSubmit={submit}>
        <FormControl fullWidth margin="normal" size="small">
          <InputLabel id="suite-run-suite-label">Suite</InputLabel>
          <Select
            labelId="suite-run-suite-label" label="Suite" value={selectedSuiteId}
            onChange={(e) => setSelectedSuiteId(e.target.value)} required
          >
            {suites.map((s) => <MenuItem key={s.id} value={s.id}>{s.name}</MenuItem>)}
          </Select>
        </FormControl>
        <Box component="label" sx={{ display: "block", mt: 2, mb: 1 }}>
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", mb: 0.5 }}>
            Repo <span className="hint">owner/name</span>
          </Typography>
          <RepoField name="repo" options={repoOptions} defaultValue="" required />
        </Box>
        <TextField
          name="base" label="Branch" required InputLabelProps={{ required: false }}
          helperText="every task this run files targets this branch, and (with auto-merge) lands back on it -- bwsalmon/agents#642's own stacking"
          placeholder="my-feature-branch" autoComplete="off" fullWidth margin="normal"
        />
        <Stack direction="row" justifyContent="flex-end" spacing={1} sx={{ mt: 2 }}>
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="contained" disabled={!selectedSuiteId}>Run</Button>
        </Stack>
      </form>
    </Overlay>
  );
}
