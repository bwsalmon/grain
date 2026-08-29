import { useState } from "react";
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

  const run = (mutate) => onRun((id) => api(`/api/tasks/${id}${mutate}`, { method: "POST" }));

  return (
    <div className="batch-bar">
      <span className="batch-count">{count} selected</span>
      <button className="secondary" onClick={() => run("/approve")}>Approve</button>
      <button className="secondary" onClick={() => run("/submit")}>Submit</button>
      <button
        className="danger secondary"
        onClick={() => {
          if (!confirm(`Close ${count} task(s)?`)) return;
          run("/close");
        }}
      >
        Close
      </button>
      <button className="secondary" onClick={() => run("/reopen")}>Reopen</button>

      <select value={capabilityId} onChange={(e) => setCapabilityId(e.target.value)}>
        <option value="">Capability…</option>
        {(config?.capabilities || []).map((c) => (
          <option key={c.id} value={c.id}>{c.name}</option>
        ))}
      </select>
      <button
        className="secondary"
        disabled={!capabilityId}
        onClick={() => onRun((id) => api(`/api/tasks/${id}/capabilities`, {
          method: "POST",
          body: JSON.stringify({ id: capabilityId, attach: true }),
        }))}
      >
        Attach
      </button>
      <button
        className="secondary"
        disabled={!capabilityId}
        onClick={() => onRun((id) => api(`/api/tasks/${id}/capabilities`, {
          method: "POST",
          body: JSON.stringify({ id: capabilityId, attach: false }),
        }))}
      >
        Detach
      </button>

      <div className="spacer" />
      <button className="secondary" onClick={onClear}>Clear selection</button>
    </div>
  );
}
