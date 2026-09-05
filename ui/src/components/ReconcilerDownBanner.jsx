import { Alert } from "@mui/material";

// ReconcilerDownBanner is what config.reconcilerDown (ui.Config.
// ReconcilerDown's own doc comment, cmd/grain/daemon.go) becomes on
// screen: a standing banner, not a five-second ErrorBanner toast, since
// unlike a single failed request this describes the whole deployment --
// nothing is being dispatched or reconciled -- for as long as it stays
// true, which is until the process is restarted (bwsalmon/agents#576).
// Pinned to the top of the page, above the sidebar and every view it
// switches between, so it stays visible regardless of which page a
// still-online-looking UI happens to be showing. The pinning itself
// lives in BannerStrip.jsx now, shared with every other standing banner
// -- see its own doc comment for the layout and why it moved out of
// here.
export default function ReconcilerDownBanner() {
  return (
    <Alert
      severity="error"
      variant="filled"
      sx={{
        borderRadius: 0,
        justifyContent: "center",
      }}
    >
      This deployment's reconcile loop has stopped -- no tasks are being
      dispatched or checked. Restart the daemon to recover.
    </Alert>
  );
}
