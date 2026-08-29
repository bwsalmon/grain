import { useCallback, useEffect, useRef, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// STATUS_POLL_MS is how often this overlay re-fetches /api/upgrade while
// an upgrade is running -- fast enough that a restart landing (the
// daemon process, and this connection to it, both dying mid-poll) is
// noticed quickly, no faster than App.jsx's own task-list poll.
const STATUS_POLL_MS = 3000;

// bwsalmon/agents#396: lets an operator pick a branch and kick off an
// in-place upgrade -- checkout, containerised build, install, and
// (if this deployment configured one) a restart to bring the new binary
// up. GET /api/upgrade's own "enabled" flag says whether that is wired
// up on this deployment at all; when it isn't, this shows a note instead
// of a form that could only ever 404, the same convention
// SettingsOverlay/SecretsOverlay already use for their own optional
// pieces.
export default function UpgradeOverlay({ onClose, showError }) {
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
    <Overlay onClose={onClose}>
      <h2>Upgrade</h2>
      {!status.enabled && (
        <p className="unconfigured-note">
          Not available: this deployment has no -upgrade-src-dir configured (bwsalmon/agents#396), so there is
          nothing here that can check out, build and install a branch.
        </p>
      )}
      {status.enabled && (
        <>
          <p className="overlay-hint">
            Fetches the branch below into this deployment's own checkout, rebuilds bin/grain with the
            containerised build, installs it, and restarts to run it -- one upgrade at a time, with no
            rollback if it goes wrong.
          </p>
          <form onSubmit={submit}>
            <label>Branch
              <input
                name="branch"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
                placeholder="main"
                autoComplete="off"
                disabled={running}
                required
              />
            </label>
            <div className="form-actions">
              <button type="submit" className="primary" disabled={running}>
                {running ? "Upgrading…" : "Upgrade"}
              </button>
            </div>
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
    </Overlay>
  );
}
