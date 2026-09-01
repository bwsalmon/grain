import { Alert, Box, Chip, Stack, Typography } from "@mui/material";

// bwsalmon/agents#611: a read-only view of GET /api/settings' own
// "capabilities" field -- every capability grain ships a provider for,
// and whether this deployment currently has enough configuration for a
// task granted one to actually work. It takes settings straight from
// SettingsOverlay rather than fetching its own copy, unlike SecretsPanel/
// UpgradePanel: the data is already loaded by the time a tab can be
// clicked, and there is nothing here for a human to submit -- fixing a
// gap means visiting the General or Secrets tab this same overlay
// already has, which each capability's own hint below points at.
export default function CapabilitiesPanel({ capabilities }) {
  const list = capabilities || [];
  return (
    <>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Every capability a task can be granted, and whether this deployment is currently
        configured for it to work. GCP settings live on the General tab; secrets live on the
        Secrets tab.
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
            </Stack>
            <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
              {cap.description}
            </Typography>
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
