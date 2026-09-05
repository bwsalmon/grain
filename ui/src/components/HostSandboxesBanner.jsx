import { Alert, AlertTitle } from "@mui/material";

// HostSandboxesBanner is what config.hostSandboxes (ui.Config.
// HostSandboxes' own doc comment, cmd/grain/daemon.go) becomes on
// screen: this deployment dispatches into plain directories on the
// daemon's own machine, so a run has that machine's filesystem, network
// and credentials and there is nothing between an agent and the host
// grain itself runs on.
//
// It says so because that used to be invisible. Host sandboxing was what
// a deployment got by leaving a flag off -- scripts/setup.sh's own
// default until grain/task-15 -- and no screen here distinguished it
// from a deployment giving every run a VM of its own, so "is this thing
// isolated?" could only be answered by reading a unit file.
//
// A standing banner rather than a five-second toast, for the reason
// ReconcilerDownBanner is one: it describes the whole deployment for as
// long as it is true, which here is until the daemon is restarted onto
// the other backend. Unlike that one, though, nothing here is broken --
// it is a deliberate configuration on any machine one operator owns, and
// wrong only where a task somebody else filed can reach it. Hence
// severity="warning" and a sentence naming the fix rather than an error
// telling an operator to restart something.
export default function HostSandboxesBanner() {
  return (
    <Alert severity="warning" variant="filled" sx={{ borderRadius: 0 }}>
      <AlertTitle sx={{ fontWeight: 700 }}>
        Host mode: agents run unsandboxed on this machine
      </AlertTitle>
      Every dispatched run executes directly on the host this daemon is on, with
      its filesystem, its network and its credentials, and nothing it changes is
      thrown away with the run. Do not run a production or shared deployment
      this way -- reinstall with GRAIN_KONTUR_ENABLE=1 (scripts/setup.sh) to
      give every run a VM of its own.
    </Alert>
  );
}
