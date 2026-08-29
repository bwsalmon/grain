// model.State's own vocabulary, in model.StateOf's precedence order.
export const STATE_ORDER = ["proposed", "queued", "running", "awaiting_reply", "completed", "closed"];

export const STATE_LABELS = {
  proposed: "Proposed",
  queued: "Queued",
  running: "Running",
  awaiting_reply: "Awaiting reply",
  completed: "Completed",
  closed: "Closed",
};

export function capabilityName(config, id) {
  const c = (config?.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}
