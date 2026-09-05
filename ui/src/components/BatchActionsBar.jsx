import { useState } from "react";
import { Box, Button, Paper, Select, Stack, Typography } from "@mui/material";
import api from "../api.js";

// BatchActionsBar is DetailOverlay's Actions and CapabilityToggles, but
// aimed at every selected task at once instead of the one the overlay
// has open -- the same handful of idempotent endpoints (approve,
// submit, defer, undefer, close, reopen, capabilities), most of which
// already no-op on a task they do not apply to (Client.Approve on an
// already-approved task, Client.SetCapability detaching a capability
// that is not attached, ...), so firing one at every selection member is
// safe even when the selection mixes states.
//
// Defer is the one that refuses rather than no-ops -- a task that has
// already run cannot be put aside (Client.Defer) -- and it is offered
// here anyway, because putting a stack of proposals aside in one go is
// the thing deferring is most often for. A mixed selection defers what
// it can and says how many it could not: App's actBatch collects
// failures rather than aborting the rest, and leaves the selection
// standing so whoever ran it can see what is left.
export default function BatchActionsBar({ count, config, onRun, onClear }) {
  const [capabilityId, setCapabilityId] = useState("");

  if (count === 0) return null;

  const run = (mutate) =>
    onRun((id) => api(`/api/tasks/${id}${mutate}`, { method: "POST" }));

  return (
    <Paper
      elevation={4}
      square
      sx={{ position: "fixed", left: 0, right: 0, bottom: 0, zIndex: 4 }}
    >
      <Stack
        direction="row"
        alignItems="center"
        spacing={1.2}
        flexWrap="wrap"
        sx={{ px: 3, py: 1.3 }}
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
          title="Put the selected tasks aside. They stop waiting to run and drop out of the lists until you ask for them."
          onClick={() => run("/defer")}
        >
          Defer
        </Button>
        <Button variant="outlined" size="small" onClick={() => run("/undefer")}>
          Undefer
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
