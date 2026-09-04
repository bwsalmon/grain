import { useEffect, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import api from "../api.js";

// Where this installation's state lives.
//
// grain's database is a git repository now (pkg/staterepo): every table
// exported as JSON, so a template, a suite or a repo's configuration can
// be changed through a pull request like anything else, with grain
// pulling the merged result back in on its own timer -- "Sync now" below
// is that same cycle, both directions, for an operator who does not want
// to wait for the tick. This pane is the bootstrap for that, and it is
// deliberately only the bootstrap -- there are three
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
  // The private key this host's secrets file is sealed to. It is the
  // third input "point grain at an existing repository" needs, for a
  // deployment moved here from somewhere else: the tables arrive through
  // the clone, the sealed file through a restore of the data directory,
  // and the key by hand or not at all.
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
      .then((s) => {
        if (live) setStatus(s);
      })
      .catch((e) => showError(e.message));
    return () => {
      live = false;
    };
  }, [showError]);

  const act = async (path, body) => {
    setBusy(true);
    try {
      setStatus(
        await api(path, { method: "POST", body: JSON.stringify(body || {}) }),
      );
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
        This UI is not running inside a daemon that owns a state repository, so
        there is nothing to configure here.
      </Typography>
    );
  }

  const schemaMismatch =
    status.schemaVersion > 0 &&
    status.schemaVersion !== status.buildSchemaVersion;

  return (
    <Box>
      <Typography variant="subtitle2">State repository</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        grain exports its whole database to a git repository &mdash; one JSON
        file per table &mdash; so settings, tasks and metrics can be read and
        changed through pull requests. Secrets go there too, in one file
        encrypted to the key below; nothing else in the repository is encrypted,
        and no agent ever reads that file.
      </Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        A merged change to the settings tables &mdash; templates, suites,
        schedules, repo and deployment configuration &mdash; is pulled in and
        live within half a minute, with no restart. Tasks, runs and metrics are
        grain&apos;s own record of what it did and are only replaced when grain
        starts, so nothing here rewrites rows a run in flight is holding.
      </Typography>

      <Stack
        direction="row"
        spacing={1}
        alignItems="center"
        sx={{ mb: 1 }}
        flexWrap="wrap"
      >
        <Chip
          size="small"
          color={status.mode === "remote" ? "primary" : "default"}
          label={status.mode === "remote" ? status.remote : "local only"}
        />
        {status.branch && (
          <Chip size="small" variant="outlined" label={status.branch} />
        )}
        {status.head && (
          <Chip
            size="small"
            variant="outlined"
            label={status.head.slice(0, 8)}
          />
        )}
      </Stack>
      <Typography variant="caption" color="text.secondary" component="div">
        working tree: {status.dir}
      </Typography>

      {status.remoteAhead && (
        <Alert severity="info" sx={{ mt: 2 }}>
          This repository holds a commit grain has not been able to take up
          &mdash; somebody merged a change that a tick could not apply on its
          own. grain loads what is waiting when it starts, so restart it to load
          this one. Until then it stops exporting rather than committing over
          the merge, so the database keeps running as it is and nothing here is
          lost.
        </Alert>
      )}
      {status.diverged && (
        <Alert severity="error" sx={{ mt: 2 }}>
          This deployment has diverged from its remote and is not syncing. Its
          working tree and the remote branch have both moved past the same
          commit, so nothing merged here is reaching grain and nothing grain
          exports is reaching the remote. grain resolves this by itself when the
          only commits in the way are its own exports; this one holds a commit
          it will not throw away, so it needs somebody at{" "}
          <code>{status.dir}</code> to decide what happens to it.
        </Alert>
      )}
      {status.workflowRefused && (
        <Alert severity="warning" sx={{ mt: 2 }}>
          Pull requests against this repository are not being checked. grain
          installs a workflow that runs <code>grain state check</code> on every
          one of them, and this deployment&apos;s credential may not push files
          under <code>.github/workflows</code>, so the file was never committed
          {status.workflowRefusedAt && (
            <>
              {" "}
              (last tried {new Date(status.workflowRefusedAt).toLocaleString()})
            </>
          )}
          . grain keeps syncing and tries again daily. To install it yourself,
          run <code>grain state ci</code> in a clone and commit{" "}
          <code>{status.workflowFile}</code> with a credential that may write
          workflows &mdash; or set <code>&quot;noWorkflow&quot;: true</code> in
          this host&apos;s state settings to stop grain offering it.
        </Alert>
      )}
      {status.error && (
        <Alert severity={status.diverged ? "info" : "warning"} sx={{ mt: 2 }}>
          {status.error}
        </Alert>
      )}
      {schemaMismatch && (
        <Alert severity="error" sx={{ mt: 2 }}>
          The repository holds schema {status.schemaVersion} and this build
          knows {status.buildSchemaVersion}. grain does not migrate a dump
          between schemas; the repository has to be re-seeded.
        </Alert>
      )}

      <Stack direction="row" spacing={1} sx={{ mt: 2 }}>
        <Button
          type="button"
          size="small"
          variant="outlined"
          disabled={busy}
          onClick={() => act("/api/state-repo/sync")}
        >
          Sync now
        </Button>
        {status.mode === "remote" && (
          <Button
            type="button"
            size="small"
            disabled={busy}
            onClick={() => act("/api/state-repo", { mode: "local" })}
          >
            Stop using the remote
          </Button>
        )}
      </Stack>

      <Typography variant="subtitle2" sx={{ mt: 3 }}>
        Point grain at a repository
      </Typography>
      <Typography variant="body2" color="text.secondary">
        An existing grain state repository replaces this installation&apos;s
        database with its contents. To start from scratch, create an empty
        repository on GitHub and paste its URL: grain seeds that one from what
        it has now. Either way the previous working tree is kept on disk, not
        deleted, and this host&apos;s secrets stay where they are &mdash; they
        live beside their key under the data directory, not in the repository.
      </Typography>
      <TextField
        label="Repository URL"
        value={remote}
        onChange={(e) => setRemote(e.target.value)}
        placeholder="https://github.com/owner/grain-state.git"
        autoComplete="off"
        fullWidth
        margin="normal"
        size="small"
      />
      <TextField
        label="Branch"
        value={branch}
        onChange={(e) => setBranch(e.target.value)}
        autoComplete="off"
        fullWidth
        margin="normal"
        size="small"
      />
      <TextField
        label="Push token (optional)"
        value={token}
        onChange={(e) => setToken(e.target.value)}
        helperText="Leave empty to push with this deployment's own GitHub credential. Stored on the host, never shown again."
        type="password"
        autoComplete="off"
        fullWidth
        margin="normal"
        size="small"
      />
      <TextField
        label="Secrets key (optional)"
        value={secretsKey}
        onChange={(e) => setSecretsKey(e.target.value)}
        helperText={
          "The private key this host's secrets file is encrypted to. Needed only when the data " +
          "directory was restored from an installation this host has not run before; otherwise the key below stays."
        }
        type="password"
        autoComplete="off"
        fullWidth
        margin="normal"
        size="small"
      />
      <Button
        type="button"
        variant="contained"
        size="small"
        disabled={busy || !remote.trim()}
        onClick={() =>
          act("/api/state-repo", {
            mode: "remote",
            remote: remote.trim(),
            branch: branch.trim(),
            token,
            secretsKey,
          })
        }
      >
        Adopt repository
      </Button>

      <Typography variant="subtitle2" sx={{ mt: 3 }}>
        Secrets key
      </Typography>
      <Typography variant="body2" color="text.secondary">
        The secrets file in the repository is encrypted to this public key. Its
        private half lives at <code>{status.secretsKeyFile}</code> and is never
        committed anywhere &mdash; back that file up, because without it no one,
        grain included, can read those secrets again.
      </Typography>
      <Typography
        variant="caption"
        component="pre"
        sx={{ mt: 1, overflowX: "auto" }}
      >
        {status.secretsPublicKey || "(no key yet)"}
      </Typography>

      {status.secretsError && (
        <Alert severity="error" sx={{ mt: 2 }}>
          grain cannot read this host&apos;s secrets file: {status.secretsError}
          {status.secretsFileRecipient && (
            <>
              {" "}
              They are encrypted to <code>{status.secretsFileRecipient}</code>;
              paste that key&apos;s private half below to install it.
            </>
          )}
        </Alert>
      )}
      <TextField
        label="Import a private key"
        value={importKey}
        onChange={(e) => setImportKey(e.target.value)}
        helperText={
          "Installs a key you already hold, so a secrets file sealed by another installation becomes " +
          "readable here. A key that cannot open the file is refused, and the key it replaces is kept on disk."
        }
        type="password"
        autoComplete="off"
        fullWidth
        margin="normal"
        size="small"
      />
      <Button
        type="button"
        size="small"
        variant="outlined"
        disabled={busy || !importKey.trim()}
        onClick={() =>
          act("/api/state-repo/secrets-key", { key: importKey.trim() })
        }
      >
        Import key
      </Button>
    </Box>
  );
}
