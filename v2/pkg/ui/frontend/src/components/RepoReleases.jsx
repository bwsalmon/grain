import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// RepoReleases is a single repo's release pane (bwsalmon/agents#459):
// configure its prod/rc branches, release branch prefix and major
// version, then cut and promote release candidates against them. It
// replaces the old ReleasesOverlay modal (bwsalmon/agents#398), which
// asked the caller to type an owner/name even though releases are a
// property of a repo -- this only ever renders from the repo pane
// (RepoList's own "Releases" button), already knowing which repo it means.
export default function RepoReleases({ repo, onBack, showError }) {
  const [owner, name] = repo.split("/");
  const [releaseConfig, setReleaseConfig] = useState(null);
  const [candidates, setCandidates] = useState([]);

  const refresh = useCallback(async () => {
    try {
      const [cfg, list] = await Promise.all([
        api(`/api/repos/${owner}/${name}/release-config`),
        api(`/api/repos/${owner}/${name}/candidates`),
      ]);
      setReleaseConfig(cfg);
      setCandidates(list);
    } catch (err) {
      showError(err);
    }
  }, [owner, name, showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const submitConfig = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const payload = {
      prodBranch: form.elements.prodBranch.value.trim(),
      rcBranch: form.elements.rcBranch.value.trim(),
      releaseBranchPrefix: form.elements.releaseBranchPrefix.value.trim(),
      majorVersion: parseInt(form.elements.majorVersion.value, 10) || 0,
    };
    try {
      await api(`/api/repos/${owner}/${name}/release-config`, { method: "PUT", body: JSON.stringify(payload) });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const cut = async () => {
    try {
      await api(`/api/repos/${owner}/${name}/candidates`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const promote = async () => {
    try {
      await api(`/api/repos/${owner}/${name}/candidates/promote`, { method: "POST" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  if (releaseConfig === null) return null;

  const current = candidates.length > 0 ? candidates[0] : null;
  const canCut = releaseConfig.configured && (!current || current.status === "promoted");
  const canPromote = current && current.status === "active";

  return (
    <main>
      <Box sx={{ px: "1.5rem" }}>
        <Button onClick={onBack} sx={{ mb: 1, ml: -0.9 }}>&larr; Repos</Button>
        <Typography variant="h6" component="h2" sx={{ mt: 0 }}>{repo} releases</Typography>

        {!releaseConfig.configured && (
          <Alert severity="info" sx={{ mb: 2 }}>
            {repo} has no release configuration yet -- set its prod branch, rc branch and
            release branch prefix below before cutting a release candidate.
          </Alert>
        )}
        <form onSubmit={submitConfig}>
          <TextField name="prodBranch" label="Prod branch" defaultValue={releaseConfig.prodBranch || ""} autoComplete="off" required InputLabelProps={{ required: false }} fullWidth margin="normal" />
          <TextField name="rcBranch" label="RC branch" helperText="the moving pointer a fresh cut repoints" defaultValue={releaseConfig.rcBranch || ""} autoComplete="off" required InputLabelProps={{ required: false }} fullWidth margin="normal" />
          <TextField name="releaseBranchPrefix" label="Release branch prefix" defaultValue={releaseConfig.releaseBranchPrefix || ""} autoComplete="off" placeholder="release/" fullWidth margin="normal" />
          <TextField name="majorVersion" label="Major version" helperText="hand-edited; grain never changes this" type="number" inputProps={{ min: 0, step: 1 }} defaultValue={String(releaseConfig.majorVersion || 0)} fullWidth margin="normal" />
          <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
            <Button type="submit" variant="contained">Save</Button>
          </Stack>
        </form>

        <Typography variant="subtitle1" sx={{ mt: 2 }}>Current candidate</Typography>
        {current ? (
          <div className="candidate-current">
            <p>
              <strong>{current.label}</strong> -- {current.status}
              {current.error && <span className="candidate-error"> ({current.error})</span>}
            </p>
            <p className="hint">branch: {current.branch}{current.releaseBranch ? `, release branch: ${current.releaseBranch}` : ""}</p>
          </div>
        ) : (
          <p className="empty">No release candidate cut yet.</p>
        )}
        <Stack direction="row" spacing={1} sx={{ mt: 1, mb: 2 }}>
          <Button variant="contained" disabled={!canCut} onClick={cut}>Cut new RC</Button>
          <Button variant="outlined" disabled={!canPromote} onClick={promote}>Promote current RC</Button>
        </Stack>

        <Typography variant="subtitle1">History</Typography>
        {candidates.length === 0 && <p className="empty">No candidates yet.</p>}
        {candidates.length > 0 && (
          <ul className="candidate-history">
            {candidates.map((c) => (
              <li key={c.id}>
                <strong>{c.label}</strong> -- {c.status}
                {c.releaseBranch ? ` -> ${c.releaseBranch}` : ""}
              </li>
            ))}
          </ul>
        )}
      </Box>
    </main>
  );
}
