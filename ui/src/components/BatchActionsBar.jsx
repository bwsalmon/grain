import { useState } from "react";
import { Box, Button, Paper, Select, Stack, Typography } from "@mui/material";
import api from "../api.js";

// BatchActionsBar is DetailOverlay's Actions and CapabilityToggles, but
// aimed at every selected task at once instead of the one the overlay
// has open -- the same handful of idempotent endpoints (approve,
// submit, close, reopen, capabilities), each of which already no-ops on
// a task it does not apply to (Client.Approve on an already-approved
// task, Client.SetCapability detaching a capability that is not
// attached, ...), so firing one at every selection member is safe even
// when the selection mixes states.
export default function BatchActionsBar({ count, config, onRun, onClear }) {
  const [capabilityId, setCapabilityId] = useState("");

  if (count === 0) return null;

  const run = (mutate) =>
    onRun((id) => api(`/api/tasks/${id}${mutate}`, { method: "POST" }));

  return (
    <Paper
      elevation={4}
      square
      sx={{
        position: "fixed",
        left: 0,
        right: 0,
        bottom: 0,
        zIndex: 4,
        // Clear of the home indicator on a phone: index.html asks for
        // viewport-fit=cover, so a bar pinned to the bottom edge is
        // partly under it without this. Zero everywhere else.
        pb: "env(safe-area-inset-bottom, 0px)",
      }}
    >
      <Stack
        direction="row"
        alignItems="center"
        spacing={1.2}
        flexWrap="wrap"
        sx={{ px: { xs: 1.5, sm: 3 }, py: 1.3 }}
      >
        <Typography fontWeight={500}>{count} selected</Typography>
        <Button variant="outlined" size="small" onClick={() => run("/approve")}>
          Approve
        </Button>
        <Button variant="outlined" size="small" onClick={() => run("/submit")}>
          Submit
        </Button>
        <Button
          variant="outlined"
          size="small"
          color="error"
          onClick={() => {
            if (!confirm(`Close ${count} task(s)?`)) return;
            run("/close");
          }}
        >
          Close
        </Button>
        <Button variant="outlined" size="small" onClick={() => run("/reopen")}>
          Reopen
        </Button>

        <Select
          native
          size="small"
          value={capabilityId}
          onChange={(e) => setCapabilityId(e.target.value)}
          displayEmpty
        >
          <option value="">Capability…</option>
          {(config?.capabilities || []).map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </Select>
        <Button
          variant="outlined"
          size="small"
          disabled={!capabilityId}
          onClick={() =>
            onRun((id) =>
              api(`/api/tasks/${id}/capabilities`, {
                method: "POST",
                body: JSON.stringify({ id: capabilityId, attach: true }),
              }),
            )
          }
        >
          Attach
        </Button>
        <Button
          variant="outlined"
          size="small"
          disabled={!capabilityId}
          onClick={() =>
            onRun((id) =>
              api(`/api/tasks/${id}/capabilities`, {
                method: "POST",
                body: JSON.stringify({ id: capabilityId, attach: false }),
              }),
            )
          }
        >
          Detach
        </Button>

        <Box sx={{ flex: 1 }} />
        <Button variant="outlined" size="small" onClick={onClear}>
          Clear selection
        </Button>
      </Stack>
    </Paper>
  );
}
