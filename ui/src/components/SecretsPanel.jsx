import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Chip, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// bwsalmon/agents#357: this pane can set and delete secrets on a server
// this UI shares a host with, and report which ones exist, but it never
// asks for -- or gets back -- a value. GET /api/secrets' own "enabled"
// flag says whether that colocation is configured at all; when it isn't,
// the list and form are hidden behind a note instead of showing controls
// that could only ever 404.
//
// bwsalmon/agents#456: lives as a tab inside SettingsOverlay rather than
// its own top-level overlay, so it renders its content only -- the
// shared Overlay/Dialog chrome is SettingsOverlay's.
export default function SecretsPanel({ showError }) {
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
    <>
      {!resp.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this UI was not started with a local secrets directory to write to
          (see -server-data-dir), which only works when the UI runs on the same host as the
          server (bwsalmon/agents#357). You can only set and delete secrets here, never read
          one back.
        </Alert>
      )}
      {resp.enabled && (
        <>
          <ul className="secrets-list">
            {(resp.secrets || []).map((s) => (
              <li className="secret-row" key={s.name}>
                <span className="secret-name">{s.name}</span>
                <Box sx={{ display: "flex", gap: 0.6, flexWrap: "wrap", flex: 1 }}>
                  {s.keys.map((key) => (
                    <Chip
                      key={key}
                      size="small"
                      label={key}
                      onDelete={() => deleteKey(s.name, key)}
                      deleteIcon={<span title={`delete ${s.name}/${key}`}>×</span>}
                    />
                  ))}
                </Box>
                <Button size="small" variant="outlined" onClick={() => deleteSecret(s.name)}>
                  Delete secret
                </Button>
              </li>
            ))}
          </ul>
          {(resp.secrets || []).length === 0 && <p className="empty">No secrets set.</p>}
          <form onSubmit={submit}>
            <TextField name="secret" label="Secret" placeholder="github" autoComplete="off" required InputLabelProps={{ required: false }} fullWidth margin="normal" />
            <TextField name="key" label="Key" placeholder="token" autoComplete="off" required InputLabelProps={{ required: false }} fullWidth margin="normal" />
            <TextField name="value" label="Value" helperText="write-only -- never shown or read back" type="password" autoComplete="off" required InputLabelProps={{ required: false }} fullWidth margin="normal" />
            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
              <Button type="submit" variant="contained">Set</Button>
            </Stack>
          </form>
        </>
      )}
    </>
  );
}
