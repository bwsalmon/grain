import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// ReleasesOverlay is bwsalmon/agents#398's whole UI: configure a repo's
// prod/rc branches, release branch prefix and major version, then cut and
// promote release candidates against them. Repos are picked by owner/name
// text rather than a dropdown sourced from tasks -- release management has
// no notion of "every repo grain knows about" (model.RepoRef is a value
// embedded on a task, not a stored entity), only the ones somebody has
// configured release settings for, which /api/release-configs lists.
export default function ReleasesOverlay({ config, onClose, showError }) {
  const [repos, setRepos] = useState(null);
  const [repo, setRepo] = useState("");
  const [releaseConfig, setReleaseConfig] = useState(null);
  const [candidates, setCandidates] = useState([]);

  const refreshRepos = useCallback(async () => {
    try {
      const list = await api("/api/release-configs");
      setRepos(list);
      if (!repo && list.length > 0) {
        setRepo(list[0].repo);
      } else if (!repo && config && config.defaultTarget) {
        setRepo(config.defaultTarget);
      }
    } catch (err) {
      showError(err);
    }
    // repo deliberately left out: this only ever seeds the initial
    // selection, never overrides one already made.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [config, showError]);

  useEffect(() => { refreshRepos(); }, [refreshRepos]);

  const refreshRepo = useCallback(async (owner, name) => {
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
  }, [showError]);

  useEffect(() => {
    const [owner, name] = repo.split("/");
    if (!owner || !name) {
      setReleaseConfig(null);
      setCandidates([]);
      return;
    }
    refreshRepo(owner, name);
  }, [repo, refreshRepo]);

  const submitConfig = async (evt) => {
    evt.preventDefault();
    const [owner, name] = repo.split("/");
    if (!owner || !name) {
      showError(new Error("repo must be owner/name"));
      return;
    }
    const form = evt.target;
    const payload = {
      prodBranch: form.elements.prodBranch.value.trim(),
      rcBranch: form.elements.rcBranch.value.trim(),
      releaseBranchPrefix: form.elements.releaseBranchPrefix.value.trim(),
      majorVersion: parseInt(form.elements.majorVersion.value, 10) || 0,
    };
    try {
      await api(`/api/repos/${owner}/${name}/release-config`, { method: "PUT", body: JSON.stringify(payload) });
      await refreshRepo(owner, name);
      await refreshRepos();
    } catch (err) {
      showError(err);
    }
  };

  const cut = async () => {
    const [owner, name] = repo.split("/");
    try {
      await api(`/api/repos/${owner}/${name}/candidates`, { method: "POST" });
      await refreshRepo(owner, name);
    } catch (err) {
      showError(err);
    }
  };

  const promote = async () => {
    const [owner, name] = repo.split("/");
    try {
      await api(`/api/repos/${owner}/${name}/candidates/promote`, { method: "POST" });
      await refreshRepo(owner, name);
    } catch (err) {
      showError(err);
    }
  };

  if (repos === null) return null;

  const current = candidates.length > 0 ? candidates[0] : null;
  const canCut = releaseConfig && releaseConfig.configured && (!current || current.status === "promoted");
  const canPromote = current && current.status === "active";

  return (
    <Overlay onClose={onClose}>
      <Typography variant="h6" component="h2" sx={{ mt: 0 }}>Releases</Typography>

      <TextField
        label="Repo"
        helperText="owner/name"
        value={repo}
        placeholder="acme/widgets"
        autoComplete="off"
        fullWidth
        margin="normal"
        inputProps={{ list: "release-repos" }}
        onChange={(e) => setRepo(e.target.value.trim())}
      />
      <datalist id="release-repos">
        {repos.map((r) => <option key={r.repo} value={r.repo} />)}
      </datalist>

      {repo.includes("/") && releaseConfig && (
        <>
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
        </>
      )}
    </Overlay>
  );
}
