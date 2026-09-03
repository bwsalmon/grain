import { Alert, Box, Chip, Stack, Typography } from "@mui/material";

// bwsalmon/agents#611: a read-only view of GET /api/settings' own
// "capabilities" field -- every capability grain ships a provider for,
// and whether this deployment currently has enough configuration for a
// task granted one to actually work. It takes settings straight from
// SettingsOverlay rather than fetching its own copy, unlike SecretsPanel/
// UpgradePanel: the data is already loaded by the time a tab can be
// clicked, and there is nothing here for a human to submit -- the GCP
// project/service account fields that satisfy the gcp-key/gemini-key
// capabilities sit above this panel, on the same Capabilities tab;
// fixing a missing secret means visiting the Secrets tab instead, which
// each capability's own hint below points at.
export default function CapabilitiesPanel({ capabilities }) {
  const list = capabilities || [];
  return (
    <>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Every capability grain ships a provider for, and whether this deployment is currently
        configured for it to work. Secrets live on the Secrets tab. A capability marked "not
        grantable" is one no task can ask for at all, however this deployment is configured.
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
          </Box>
        ))}
      </Stack>
    </>
  );
}
