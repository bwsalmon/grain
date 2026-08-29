// model.State's own vocabulary, in model.StateOf's precedence order.
export const STATE_ORDER = ["proposed", "queued", "running", "awaiting_reply", "failed", "completed", "closed"];

export const STATE_LABELS = {
  proposed: "Proposed",
  queued: "Queued",
  running: "Running",
  awaiting_reply: "Awaiting reply",
  // A task stops here, not "queued", once it has failed
  // model.MaxConsecutiveFailures runs in a row -- see StateFailed's own
  // doc comment. "Retry" (DetailOverlay's own Actions) is the only way
  // out.
  failed: "Failed",
  completed: "Completed",
  closed: "Closed",
};

export function capabilityName(config, id) {
  const c = (config?.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}
