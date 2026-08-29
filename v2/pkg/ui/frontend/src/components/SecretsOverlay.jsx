import { useCallback, useEffect, useState } from "react";
import api from "../api.js";
import Overlay from "./Overlay.jsx";

// bwsalmon/agents#357: this pane can set and delete secrets on a server
// this UI shares a host with, and report which ones exist, but it never
// asks for -- or gets back -- a value. GET /api/secrets' own "enabled"
// flag says whether that colocation is configured at all; when it isn't,
// the list and form are hidden behind a note instead of showing controls
// that could only ever 404.
export default function SecretsOverlay({ onClose, showError }) {
  const [resp, setResp] = useState(null);

  const refresh = useCallback(async () => {
    try {
      setResp(await api("/api/secrets"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const deleteKey = async (secret, key) => {
    try {
      await api(`/api/secrets/${encodeURIComponent(secret)}/${encodeURIComponent(key)}`, { method: "DELETE" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const deleteSecret = async (secret) => {
    try {
      await api(`/api/secrets/${encodeURIComponent(secret)}`, { method: "DELETE" });
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  const submit = async (evt) => {
    evt.preventDefault();
    const form = evt.target;
    const secret = form.elements.secret.value.trim();
    const key = form.elements.key.value.trim();
    const value = form.elements.value.value;
    try {
      await api(`/api/secrets/${encodeURIComponent(secret)}/${encodeURIComponent(key)}`, {
        method: "PUT",
        body: JSON.stringify({ value }),
      });
      form.reset();
      await refresh();
    } catch (err) {
      showError(err);
    }
  };

  if (resp === null) return null;

  return (
    <Overlay onClose={onClose}>
      <h2>Secrets</h2>
      {!resp.enabled && (
        <p className="unconfigured-note">
          Not available: this UI was not started with a local secrets directory to write to
          (see -server-data-dir), which only works when the UI runs on the same host as the
          server (bwsalmon/agents#357). You can only set and delete secrets here, never read
          one back.
        </p>
      )}
      {resp.enabled && (
        <>
          <ul className="secrets-list">
            {(resp.secrets || []).map((s) => (
              <li className="secret-row" key={s.name}>
                <span className="secret-name">{s.name}</span>
                <span className="secret-keys">
                  {s.keys.map((key) => (
                    <span className="secret-key" key={key}>
                      {key}
                      <button
                        className="secret-key-delete"
                        type="button"
                        title={`delete ${s.name}/${key}`}
                        onClick={() => deleteKey(s.name, key)}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                </span>
                <button className="secondary secret-delete" type="button" onClick={() => deleteSecret(s.name)}>
                  Delete secret
                </button>
              </li>
            ))}
          </ul>
          {(resp.secrets || []).length === 0 && <p className="empty">No secrets set.</p>}
          <form onSubmit={submit}>
            <label>Secret
              <input name="secret" placeholder="github" autoComplete="off" required />
            </label>
            <label>Key
              <input name="key" placeholder="token" autoComplete="off" required />
            </label>
            <label>Value <span className="hint">write-only -- never shown or read back</span>
              <input name="value" type="password" autoComplete="off" required />
            </label>
            <div className="form-actions">
              <button type="submit" className="primary">Set</button>
            </div>
          </form>
        </>
      )}
    </Overlay>
  );
}
