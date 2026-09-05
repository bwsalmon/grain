import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  MenuItem,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
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
// grain/task-4 added the second half: which repos reach which
// credential, credentials.json itself. That file was the last thing
// standing a deployment up needed a shell on the host for -- and the
// least discoverable, since a UI that had taken the token quite happily
// gave no sign that every clone was still going to fail closed. The
// first token added now becomes the deployment default on its own, and
// the ladder below is there for the narrower entries.
//
// The one thing this pane cannot do for an operator is make a new token
// grantable straight away. The capability set is built once, at daemon
// startup -- so a token added here is on disk, but this process is still
// offering exactly the capabilities it started with. The API answers
// with both facts per token (present, offered), and the disagreement
// between them is what "restart needed" below is. A *ladder* change
// carries no such wait: the git proxy re-reads credentials.json, so a
// repo pointed at a credential here is pushed with it on its next clone.
export default function GitHubTokensSection({ showError }) {
  const [resp, setResp] = useState(null);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [pattern, setPattern] = useState("");
  const [patternCredential, setPatternCredential] = useState("");

  const refresh = useCallback(async () => {
    try {
      setResp(await api("/api/github-tokens"));
    } catch (err) {
      showError(err);
    }
  }, [showError]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const submit = async (evt) => {
    evt.preventDefault();
    try {
      setResp(
        await api(`/api/github-tokens/${encodeURIComponent(name.trim())}`, {
          method: "PUT",
          body: JSON.stringify({ value }),
        }),
      );
      setName("");
      setValue("");
    } catch (err) {
      showError(err);
    }
  };

  const remove = async (token) => {
    try {
      setResp(
        await api(`/api/github-tokens/${encodeURIComponent(token)}`, {
          method: "DELETE",
        }),
      );
    } catch (err) {
      showError(err);
    }
  };

  const submitPattern = async (evt) => {
    evt.preventDefault();
    try {
      setResp(
        await api("/api/github-credential-patterns", {
          method: "PUT",
          body: JSON.stringify({
            pattern: pattern.trim(),
            credential: patternCredential,
          }),
        }),
      );
      setPattern("");
      setPatternCredential("");
    } catch (err) {
      showError(err);
    }
  };

  const removePattern = async (entry) => {
    try {
      setResp(
        await api(
          `/api/github-credential-patterns?pattern=${encodeURIComponent(entry)}`,
          { method: "DELETE" },
        ),
      );
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
  const patterns = resp.patterns || [];
  // Anything a ladder entry may name: a credential that exists, plus the
  // no-credential-at-all case a public repo wants. The same set
  // SetPattern accepts, so the picker cannot offer what the API refuses.
  const credentialChoices = [
    ...tokens.filter((token) => token.present).map((token) => token.name),
    "anonymous",
  ];

  return (
    <Box sx={{ mt: 3 }}>
      <Typography variant="subtitle2">Named tokens</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
        The GitHub credentials this deployment pushes and pulls with. The
        default one serves every repo the ladder covers; each of the others is a
        capability a single task can be given (&quot;GitHub token:
        &lt;name&gt;&quot;), for work that needs a scope or an account the
        default deliberately isn&apos;t. They are files under
        {resp.dir ? ` ${resp.dir}` : " this deployment's secrets directory"},
        the ones the git proxy reads.
      </Typography>
      {!resp.enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this UI has no local GitHub credential directory to
          write to, which only happens when it is not running beside the git
          proxy that reads one.
        </Alert>
      )}
      {resp.enabled && (
        <>
          {tokens.length > 0 && !resp.defaultName && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              No default credential: nothing in the ladder below covers a repo
              with no entry of its own, so every clone and push for one fails
              with &quot;no credential configured&quot;. Point &quot;*&quot; at
              a credential to fix it.
            </Alert>
          )}
          {resp.restartRequired && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              Restart the daemon to finish applying the changes below. The
              credential ladder is read once at startup, so a token added here
              cannot be ticked on a task -- and one removed here is still
              offered -- until then.
            </Alert>
          )}
          <ul className="secrets-list">
            {tokens.map((token) => (
              <li className="secret-row" key={token.name}>
                <span className="secret-name">{token.name}</span>
                <Box
                  sx={{ display: "flex", gap: 0.6, flexWrap: "wrap", flex: 1 }}
                >
                  {token.default && (
                    <Chip
                      size="small"
                      label="deployment default"
                      color="primary"
                    />
                  )}
                  {token.app && <Chip size="small" label="GitHub App" />}
                  {token.capability && (
                    <Chip size="small" label={token.capability} />
                  )}
                  {(token.patterns || []).map((pattern) => (
                    <Chip
                      key={pattern}
                      size="small"
                      variant="outlined"
                      label={pattern}
                    />
                  ))}
                  {!token.present && (
                    <Chip
                      size="small"
                      color="error"
                      label="no credential file"
                    />
                  )}
                  {token.needsRestart && (
                    <Chip size="small" color="warning" label="restart needed" />
                  )}
                </Box>
                <Button
                  size="small"
                  variant="outlined"
                  aria-label={`delete ${token.name}`}
                  disabled={!token.present || (token.patterns || []).length > 0}
                  title={
                    (token.patterns || []).length > 0
                      ? "credentials.json still maps a repo pattern to this credential -- remove that entry on the host first"
                      : `delete ${token.name}`
                  }
                  onClick={() => remove(token.name)}
                >
                  Delete
                </Button>
              </li>
            ))}
          </ul>
          {tokens.length === 0 && (
            <p className="empty">No GitHub credentials configured.</p>
          )}
          <form onSubmit={submit}>
            <TextField
              label="Token name"
              helperText='letters, digits, - and _ -- becomes the capability "GitHub token: &lt;name&gt;". An existing name replaces that token&apos;s value.'
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
              <Button
                type="submit"
                variant="contained"
                disabled={name.trim() === "" || value === ""}
              >
                Save token
              </Button>
            </Stack>
          </form>

          <Typography variant="subtitle2" sx={{ mt: 3 }}>
            Which repos use which credential
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
            credentials.json, the ladder the git proxy resolves every clone and
            push against. The narrowest entry covering a repo wins:
            owner/repo, then owner/*, then &quot;*&quot; -- the deployment
            default, which the first credential added above becomes. Changes
            here need no restart.
          </Typography>
          <ul className="secrets-list">
            {patterns.map((entry) => (
              <li className="secret-row" key={entry.pattern}>
                <span className="secret-name">{entry.pattern}</span>
                <Box
                  sx={{ display: "flex", gap: 0.6, flexWrap: "wrap", flex: 1 }}
                >
                  <Chip
                    size="small"
                    color={entry.pattern === "*" ? "primary" : "default"}
                    label={entry.credential}
                  />
                  {entry.missing && (
                    <Chip
                      size="small"
                      color="error"
                      label="no such credential"
                    />
                  )}
                </Box>
                <Button
                  size="small"
                  variant="outlined"
                  aria-label={`delete ladder entry ${entry.pattern}`}
                  onClick={() => removePattern(entry.pattern)}
                >
                  Delete
                </Button>
              </li>
            ))}
          </ul>
          {patterns.length === 0 && (
            <p className="empty">
              No ladder entries: every repo fails closed until one is added.
            </p>
          )}
          <form onSubmit={submitPattern}>
            <TextField
              label="Repo pattern"
              helperText='"*" for the deployment default, or owner/* or owner/repo for something narrower.'
              value={pattern}
              onChange={(evt) => setPattern(evt.target.value)}
              autoComplete="off"
              required
              InputLabelProps={{ required: false }}
              fullWidth
              margin="normal"
            />
            <TextField
              select
              label="Credential"
              helperText='the credential those repos are pushed and pulled with -- "anonymous" sends no credential at all, which is what a public repo needs.'
              value={patternCredential}
              onChange={(evt) => setPatternCredential(evt.target.value)}
              required
              InputLabelProps={{ required: false }}
              fullWidth
              margin="normal"
            >
              {credentialChoices.map((choice) => (
                <MenuItem key={choice} value={choice}>
                  {choice}
                </MenuItem>
              ))}
            </TextField>
            <Stack direction="row" justifyContent="flex-end" sx={{ mt: 2 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={pattern.trim() === "" || patternCredential === ""}
              >
                Save entry
              </Button>
            </Stack>
          </form>
        </>
      )}
    </Box>
  );
}
