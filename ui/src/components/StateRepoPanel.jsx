import { useEffect, useState } from "react";
import { Alert, Box, Button, Chip, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// Where this installation's state lives.
//
// grain's database is a git repository now (pkg/staterepo): every table
// exported as JSON, so a template, a suite or a repo's configuration can
// be changed through a pull request like anything else, with grain
// pulling the merged result back in. This pane is the bootstrap for
// that, and it is deliberately only the bootstrap -- there are three
// answers to "where does the repository live" and nothing here edits
// what is *in* it, because the repository is the place to do that.
//
// The three:
//
//   - local only, which needs nothing from anybody: grain inits a
//     repository under its data directory and commits to it forever.
//     This is what a fresh install already has, so this pane is never a
//     wall an operator has to get past before grain works.
//   - an existing repository, whose contents replace this installation's
//     database when it is adopted.
//   - an empty repository, which grain seeds from the database it has.
//
// The last two are one form, because adopting cannot tell them apart up
// front and does not need to: empty is seeded, populated is imported.
export default function StateRepoPanel({ showError }) {
  const [status, setStatus] = useState(null);
  const [remote, setRemote] = useState("");
  const [branch, setBranch] = useState("main");
  const [token, setToken] = useState("");
  // The private key of the installation being adopted. It is the third
  // input "point grain at an existing repository" needs and the one the
  // repository deliberately cannot carry: the clone brings the sealed
  // secrets file, and nothing here can open it until its key arrives by
  // some other route than the repository itself.
  const [secretsKey, setSecretsKey] = useState("");
  // The same key, arriving later: a repository can be adopted before
  // whoever runs it has fetched their key out of wherever they keep it,
  // so importing one is its own action and its own field rather than
  // something only an adopt can carry.
  const [importKey, setImportKey] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let live = true;
    api("/api/state-repo")
      .then((s) => { if (live) setStatus(s); })
      .catch((e) => showError(e.message));
    return () => { live = false; };
  }, [showError]);

  const act = async (path, body) => {
    setBusy(true);
    try {
      setStatus(await api(path, { method: "POST", body: JSON.stringify(body || {}) }));
      // Both credentials have reached the daemon and are written to files
      // only it reads; keeping either in a form field afterwards serves
      // nobody.
      setToken("");
      setSecretsKey("");
      setImportKey("");
    } catch (e) {
      showError(e.message);
    } finally {
      setBusy(false);
    }
  };

  if (!status) return null;
  if (!status.available) {
    return (
      <Typography variant="body2" color="text.secondary">
        This UI is not running inside a daemon that owns a state repository, so there is nothing to configure here.
      </Typography>
    );
  }

  const schemaMismatch =
    status.schemaVersion > 0 && status.schemaVersion !== status.buildSchemaVersion;

  return (
    <Box>
      <Typography variant="subtitle2">State repository</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        grain exports its whole database to a git repository &mdash; one JSON file per table &mdash; so settings,
        tasks and metrics can be read and changed through pull requests. Secrets go there too, in one file encrypted
        to the key below; nothing else in the repository is encrypted, and no agent ever reads that file.
      </Typography>

      <Stack direction="row" spacing={1} alignItems="center" sx={{ mb: 1 }} flexWrap="wrap">
        <Chip
          size="small"
          color={status.mode === "remote" ? "primary" : "default"}
          label={status.mode === "remote" ? status.remote : "local only"}
        />
        {status.branch && <Chip size="small" variant="outlined" label={status.branch} />}
        {status.head && <Chip size="small" variant="outlined" label={status.head.slice(0, 8)} />}
      </Stack>
      <Typography variant="caption" color="text.secondary" component="div">
        working tree: {status.dir}
      </Typography>

      {status.remoteAhead && (
        <Alert severity="info" sx={{ mt: 2 }}>
          Somebody merged a change into this repository. grain applies what is waiting when it starts &mdash;
          restart it to load this one. Until then it stops exporting rather than committing over the merge, so
          the database keeps running as it is and nothing here is lost.
        </Alert>
      )}
      {status.error && <Alert severity="warning" sx={{ mt: 2 }}>{status.error}</Alert>}
      {schemaMismatch && (
        <Alert severity="error" sx={{ mt: 2 }}>
          The repository holds schema {status.schemaVersion} and this build knows {status.buildSchemaVersion}. grain
          does not migrate a dump between schemas; the repository has to be re-seeded.
        </Alert>
      )}

      <Stack direction="row" spacing={1} sx={{ mt: 2 }}>
        <Button type="button" size="small" variant="outlined" disabled={busy}
          onClick={() => act("/api/state-repo/sync")}>
          Sync now
        </Button>
        {status.mode === "remote" && (
          <Button type="button" size="small" disabled={busy}
            onClick={() => act("/api/state-repo", { mode: "local" })}>
            Stop using the remote
          </Button>
        )}
      </Stack>

      <Typography variant="subtitle2" sx={{ mt: 3 }}>Point grain at a repository</Typography>
      <Typography variant="body2" color="text.secondary">
        An existing grain state repository replaces this installation&apos;s database with its contents &mdash; give it
        the secrets key below as well, or its secrets stay unreadable here. To start from scratch, create an empty
        repository on GitHub and paste its URL: grain seeds that one from what it has now, secrets and all. Either
        way the previous working tree is kept on disk, not deleted.
      </Typography>
      <TextField label="Repository URL" value={remote} onChange={(e) => setRemote(e.target.value)}
        placeholder="https://github.com/owner/grain-state.git" autoComplete="off" fullWidth margin="normal" size="small" />
      <TextField label="Branch" value={branch} onChange={(e) => setBranch(e.target.value)}
        autoComplete="off" fullWidth margin="normal" size="small" />
      <TextField label="Push token (optional)" value={token} onChange={(e) => setToken(e.target.value)}
        helperText="Leave empty to push with this deployment's own GitHub credential. Stored on the host, never shown again."
        type="password" autoComplete="off" fullWidth margin="normal" size="small" />
      <TextField label="Secrets key (optional)" value={secretsKey} onChange={(e) => setSecretsKey(e.target.value)}
        helperText={"The private key that repository's secrets are encrypted to. Needed only when adopting an " +
          "installation this host has not run before; an empty repository keeps the key below."}
        type="password" autoComplete="off" fullWidth margin="normal" size="small" />
      <Button type="button" variant="contained" size="small" disabled={busy || !remote.trim()}
        onClick={() => act("/api/state-repo", {
          mode: "remote", remote: remote.trim(), branch: branch.trim(), token, secretsKey,
        })}>
        Adopt repository
      </Button>

      <Typography variant="subtitle2" sx={{ mt: 3 }}>Secrets key</Typography>
      <Typography variant="body2" color="text.secondary">
        The secrets file in the repository is encrypted to this public key. Its private half lives at{" "}
        <code>{status.secretsKeyFile}</code> and is never committed anywhere &mdash; back that file up, because
        without it no one, grain included, can read those secrets again.
      </Typography>
      <Typography variant="caption" component="pre" sx={{ mt: 1, overflowX: "auto" }}>
        {status.secretsPublicKey || "(no key yet)"}
      </Typography>

      {status.secretsError && (
        <Alert severity="error" sx={{ mt: 2 }}>
          grain cannot read the secrets in this repository: {status.secretsError}
          {status.secretsFileRecipient && (
            <>
              {" "}They are encrypted to <code>{status.secretsFileRecipient}</code>; paste that key&apos;s private
              half below to install it.
            </>
          )}
        </Alert>
      )}
      <TextField label="Import a private key" value={importKey} onChange={(e) => setImportKey(e.target.value)}
        helperText={"Installs a key you already hold, so a repository sealed to another installation becomes " +
          "readable here. A key that cannot open the file is refused, and the key it replaces is kept on disk."}
        type="password" autoComplete="off" fullWidth margin="normal" size="small" />
      <Button type="button" size="small" variant="outlined" disabled={busy || !importKey.trim()}
        onClick={() => act("/api/state-repo/secrets-key", { key: importKey.trim() })}>
        Import key
      </Button>
    </Box>
  );
}
