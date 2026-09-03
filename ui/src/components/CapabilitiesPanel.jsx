import { Alert, Box, Chip, Stack, Typography } from "@mui/material";
import { SecretFields } from "./SecretField.jsx";

// bwsalmon/agents#611: a mostly read-only view of GET /api/settings' own
// "capabilities" field -- every capability grain ships a provider for,
// and whether this deployment currently has enough configuration for a
// task granted one to actually work. It takes settings straight from
// SettingsOverlay rather than fetching its own copy, unlike SecretsPanel/
// UpgradePanel: the data is already loaded by the time a tab can be
// clicked. The GCP project/service account fields that satisfy the
// gcp-key/gemini-key capabilities, and the picker choosing which
// capabilities every new task is filed holding, both sit above this panel
// on the same Capabilities tab (this reports that choice back as
// cap.default).
//
// The one thing here that is not read-only is a capability's own secrets
// (grain/task-110): each row carries a write-only field per credential
// the capability resolves (cap.secrets), so the pane that reports one
// missing is the pane that fills it in. A value written there changes
// nothing this component holds, so it asks SettingsOverlay to re-read
// settings through onSecretsChanged; without a colocated secrets store
// the API reports no secrets at all and no field is offered.
//
// The list is not quite fixed: a deployment's extra named GitHub tokens
// are capabilities too (grain/task-117, "github-credential:<name>"), and
// arrive here in the same shape as everything else -- ready and
// grantable, with no missing config or secrets to report, since a token
// is a file an operator placed rather than something this build gates.
//
// cap.defaultRepos is the second layer of the same choice, made
// somewhere else again -- on the repos page, one repo at a time
// (model.RepoConfig.DefaultCapabilities). It is reported here beside
// cap.default rather than folded into it: with two layers, a single
// "Default" chip would describe a deployment-wide default that only some
// tasks actually get.
export default function CapabilitiesPanel({ capabilities, showError, onSecretsChanged }) {
  const list = capabilities || [];
  return (
    <>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Every capability grain ships a provider for -- plus one per named GitHub token this
        deployment has configured beyond its default one, which a task can be given to push and
        pull through that token instead -- and whether this deployment is currently
        configured for it to work. Each one&apos;s own credentials are set here too, beneath it;
        the agent frameworks&apos; keys live on the Agents tab, beside the choice of framework. A
        capability marked "not
        grantable" is one no task can ask for at all, however this deployment is configured; one
        marked "default" is attached to every new task as it is filed, and one marked "default in"
        only to tasks filed against the repos named (set on the repos page, per repo).
      </Typography>
      {list.length === 0 && <Alert severity="info">No capabilities known.</Alert>}
      <Stack spacing={1.5}>
        {list.map((cap) => (
          <Box key={cap.id} sx={{ border: "1px solid", borderColor: "divider", borderRadius: 1, p: 1.5 }}>
            <Stack direction="row" spacing={1} alignItems="center">
              <Typography variant="subtitle2">{cap.name || cap.id}</Typography>
              <Chip
                size="small"
                label={cap.ready ? "Ready" : "Not ready"}
                color={cap.ready ? "success" : "default"}
                variant={cap.ready ? "filled" : "outlined"}
              />
              {cap.grantable === false && (
                <Chip size="small" label="Not grantable" color="warning" variant="filled" />
              )}
              {/* Which capabilities are defaulted is chosen in the form
                  above; this only reports it, so the readiness of one
                  every task is filed holding is visible in the same
                  line as the fact that it is. */}
              {cap.default && <Chip size="small" label="Default" color="info" variant="outlined" />}
              {/* Shown alongside "Default", not instead of it: a repo can
                  restate one the deployment already gives, and dropping
                  it deployment-wide leaves the repo's own entry standing. */}
              {(cap.defaultRepos || []).length > 0 && (
                <Chip
                  size="small"
                  label={`Default in ${cap.defaultRepos.length} repo${cap.defaultRepos.length === 1 ? "" : "s"}`}
                  color="info"
                  variant="outlined"
                  title={cap.defaultRepos.join(", ")}
                />
              )}
            </Stack>
            <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
              {cap.description}
            </Typography>
            {/* Ahead of the two "missing ..." hints deliberately: this one
                cannot be fixed by anything on this pane, so an operator who
                reads no further than the first line under a capability is
                the one who most needs to see it. */}
            {cap.grantable === false && (
              <Typography variant="body2" className="hint" sx={{ mt: 0.5 }}>
                No task can be granted this: grain registers a provider for it, but the task
                capability picker does not offer it, so attaching it is rejected as an unknown
                capability. Configuration cannot fix this one.
              </Typography>
            )}
            {cap.default && cap.ready === false && (
              <Typography variant="body2" className="hint" sx={{ mt: 0.5 }}>
                Every new task is filed holding this, and it is not ready: each of those tasks will fail to
                dispatch until the gap below is closed, or this capability is dropped from the defaults above.
              </Typography>
            )}
            {(cap.defaultRepos || []).length > 0 && (
              <Typography variant="body2" className="hint" sx={{ mt: 0.5 }}>
                Defaulted on: {cap.defaultRepos.join(", ")} -- every task filed against one of those repos
                starts holding this{cap.ready === false ? ", and will fail to dispatch until the gap below is closed" : ""}.
                Change it on the repos page, under that repo&apos;s Capabilities.
              </Typography>
            )}
            {(cap.missingConfig || []).length > 0 && (
              <Typography variant="body2" className="hint" sx={{ mt: 0.5 }}>
                Needs: {cap.missingConfig.join(", ")}
              </Typography>
            )}
            {(cap.missingSecrets || []).length > 0 && (
              <Typography variant="body2" className="hint" sx={{ mt: 0.5 }}>
                Missing secrets: {cap.missingSecrets.join(", ")}
              </Typography>
            )}
            {/* Last in the row, under everything that says what is
                wrong: the fields are what to do about it, and a
                capability with nothing missing still gets them so a
                credential can be rotated or cleared from the same
                place it was set. */}
            <SecretFields secrets={cap.secrets} showError={showError} onChanged={onSecretsChanged} />
          </Box>
        ))}
      </Stack>
    </>
  );
}
