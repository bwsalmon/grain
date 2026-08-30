import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// STATUS_POLL_MS is how often this panel re-fetches /api/upgrade while
// an upgrade is running -- fast enough that a restart landing (the
// daemon process, and this connection to it, both dying mid-poll) is
// noticed quickly, no faster than App.jsx's own task-list poll.
const STATUS_POLL_MS = 3000;

// bwsalmon/agents#396: lets an operator pick a branch and kick off an
// in-place upgrade -- checkout, containerised build, install, and
// (if this deployment configured one) a restart to bring the new binary
// up. GET /api/upgrade's own "enabled" flag says whether that is wired
// up on this deployment at all; when it isn't, this shows a note instead
// of a form that could only ever 404, the same convention SettingsOverlay/
// SecretsPanel already use for their own optional pieces.
//
// bwsalmon/agents#456: lives as a tab inside SettingsOverlay rather than
// its own top-level overlay, so it renders its content only -- the
// shared Overlay/Dialog chrome is SettingsOverlay's.
export default function UpgradePanel({ showError }) {
  const [status, setStatus] = useState(null);
  const [branch, setBranch] = useState("");
  const polling = useRef(false);

  const refresh = useCallback(async () => {
    if (polling.current) return;
    polling.current = true;
    try {
      setStatus(await api("/api/upgrade"));
    } catch (err) {
      showError(err);
    } finally {
      polling.current = false;
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  useEffect(() => {
    if (!status || status.phase !== "running") return;
    const interval = setInterval(refresh, STATUS_POLL_MS);
    return () => clearInterval(interval);
  }, [status, refresh]);

  const submit = async (evt) => {
    evt.preventDefault();
    const trimmed = branch.trim();
    if (trimmed === "") return;
    try {
      setStatus(await api("/api/upgrade", { method: "POST", body: JSON.stringify({ branch: trimmed }) }));
    } catch (err) {
      showError(err);
    }
  };

  if (status === null) return null;

  const running = status.phase === "running";

  return (
    <>
      {!status.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this deployment has no -upgrade-src-dir configured (bwsalmon/agents#396), so there is
          nothing here that can check out, build and install a branch.
        </Alert>
      )}
      {status.enabled && (
        <>
          <Typography variant="body2" color="text.secondary" sx={{ mt: -1, mb: 2 }}>
            Fetches the branch below into this deployment's own checkout, rebuilds bin/grain with the
            containerised build, installs it, and restarts to run it -- one upgrade at a time, with no
            rollback if it goes wrong.
          </Typography>
          <form onSubmit={submit}>
            <TextField
              name="branch"
              label="Branch"
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              placeholder="main"
              autoComplete="off"
              disabled={running}
              required
              InputLabelProps={{ required: false }}
              fullWidth
              margin="normal"
            />
            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
              <Button type="submit" variant="contained" disabled={running}>
                {running ? "Upgrading…" : "Upgrade"}
              </Button>
            </Stack>
          </form>
          {status.phase && (
            <dl className="upgrade-status">
              <dt>Branch</dt>
              <dd>{status.branch}</dd>
              <dt>Status</dt>
              <dd className={`upgrade-phase upgrade-phase-${status.phase}`}>{status.phase}</dd>
              {status.detail && <><dt>Detail</dt><dd>{status.detail}</dd></>}
            </dl>
          )}
        </>
      )}
    </>
  );
}
