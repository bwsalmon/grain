import { useCallback, useEffect, useState } from "react";
import { Alert, Box, Button, Chip, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// grain/task-137: the named GitHub tokens this deployment holds, on the
// same tab as the rest of its GitHub configuration.
//
// grain/task-117 made every token beyond the deployment default a
// capability of its own ("GitHub token: <name>" in the per-task picker),
// but adding one still meant shell access to the host: write
// <name>.token into $GRAIN_DATA_DIR/secrets/github by hand. This is that
// step, moved here -- writing the same files, in the same directory,
// that the git proxy has always read (pkg/gitproxy/credentials.go's own
// doc comment on why they are files and not rows in the secrets
// database).
//
// Write-only like every other credential control in this pane: a value
// goes in, names and presence come back, nothing is ever read out.
//
// The one thing this pane cannot do for an operator is make a new token
// usable straight away. The credential ladder is deliberately loaded
// once, at daemon startup -- so a token added here is on disk, but this
// process is still offering exactly the capabilities it started with.
// The API answers with both facts per token (present, offered), and the
// disagreement between them is what "restart needed" below is.
export default function GitHubTokensSection({ showError }) {
  const [resp, setResp] = useState(null);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");

  const refresh = useCallback(async () => {
    try {
      setResp(await api("/api/github-tokens"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => { refresh(); }, [refresh]);

  const submit = async (evt) => {
    evt.preventDefault();
    try {
      setResp(await api(`/api/github-tokens/${encodeURIComponent(name.trim())}`, {
        method: "PUT",
        body: JSON.stringify({ value }),
      }));
      setName("");
      setValue("");
    } catch (err) {
      showError(err);
    }
  };

  const remove = async (token) => {
    try {
      setResp(await api(`/api/github-tokens/${encodeURIComponent(token)}`, { method: "DELETE" }));
    } catch (err) {
      showError(err);
    }
  };

  // Nothing until the listing is in hand -- and a response with no body
  // at all (an older daemon that has never heard of this endpoint,
  // answering 204) is read the same way rather than rendered as an empty
  // ladder that isn't one.
  if (!resp) return null;

  const tokens = resp.tokens || [];

  return (
    <Box sx={{ mt: 3 }}>
      <Typography variant="subtitle2">Named tokens</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
        The GitHub credentials this deployment pushes and pulls with. The default one serves every
        repo the ladder covers; each of the others is a capability a single task can be given
        (&quot;GitHub token: &lt;name&gt;&quot;), for work that needs a scope or an account the
        default deliberately isn&apos;t. Which repos fall back to which credential is still
        credentials.json on the host{resp.dir ? ` (${resp.dir})` : ""}, not editable from here.
      </Typography>
      {!resp.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this UI has no local GitHub credential directory to write to, which only
          happens when it is not running beside the git proxy that reads one.
        </Alert>
      )}
      {resp.enabled && (
        <>
          {resp.restartRequired && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              Restart the daemon to finish applying the changes below. The credential ladder is read
              once at startup, so a token added here cannot be ticked on a task -- and one removed
              here is still offered -- until then.
            </Alert>
          )}
          <ul className="secrets-list">
            {tokens.map((token) => (
              <li className="secret-row" key={token.name}>
                <span className="secret-name">{token.name}</span>
                <Box sx={{ display: "flex", gap: 0.6, flexWrap: "wrap", flex: 1 }}>
                  {token.default && <Chip size="small" label="deployment default" color="primary" />}
                  {token.app && <Chip size="small" label="GitHub App" />}
                  {token.capability && <Chip size="small" label={token.capability} />}
                  {(token.patterns || []).map((pattern) => (
                    <Chip key={pattern} size="small" variant="outlined" label={pattern} />
                  ))}
                  {!token.present && <Chip size="small" color="error" label="no credential file" />}
                  {token.needsRestart && <Chip size="small" color="warning" label="restart needed" />}
                </Box>
                <Button
                  size="small"
                  variant="outlined"
                  aria-label={`delete ${token.name}`}
                  disabled={!token.present || (token.patterns || []).length > 0}
                  title={(token.patterns || []).length > 0
                    ? "credentials.json still maps a repo pattern to this credential -- remove that entry on the host first"
                    : `delete ${token.name}`}
                  onClick={() => remove(token.name)}
                >
                  Delete
                </Button>
              </li>
            ))}
          </ul>
          {tokens.length === 0 && <p className="empty">No GitHub credentials configured.</p>}
          <form onSubmit={submit}>
            <TextField
              label="Token name"
              helperText="letters, digits, - and _ -- becomes the capability &quot;GitHub token: &lt;name&gt;&quot;. An existing name replaces that token's value."
              value={name}
              onChange={(evt) => setName(evt.target.value)}
              autoComplete="off"
              required
              InputLabelProps={{ required: false }}
              fullWidth
              margin="normal"
            />
            <TextField
              label="Token"
              helperText="a GitHub PAT -- write-only, never shown or read back. A GitHub App credential is three values in a file and is still placed on the host."
              type="password"
              value={value}
              onChange={(evt) => setValue(evt.target.value)}
              autoComplete="off"
              required
              InputLabelProps={{ required: false }}
              fullWidth
              margin="normal"
            />
            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
              <Button type="submit" variant="contained" disabled={name.trim() === "" || value === ""}>
                Save token
              </Button>
            </Stack>
          </form>
        </>
      )}
    </Box>
  );
}
