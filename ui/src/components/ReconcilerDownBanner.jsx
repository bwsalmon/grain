import { Alert } from "@mui/material";

// ReconcilerDownBanner is what config.reconcilerDown (ui.Config.
// ReconcilerDown's own doc comment, cmd/grain/daemon.go) becomes on
// screen: a standing banner, not a five-second ErrorBanner toast, since
// unlike a single failed request this describes the whole deployment --
// nothing is being dispatched or reconciled -- for as long as it stays
// true, which is until the process is restarted (bwsalmon/agents#576).
// Pinned to the top of the page, above the sidebar and every view it
// switches between, so it stays visible regardless of which page a
// still-online-looking UI happens to be showing.
// position: fixed (rather than taking up space in .app-shell's own flex
// row, style.css) keeps this from having to touch that layout at all --
// .app-shell is locked to height: 100vh precisely so panes scroll
// internally instead of the whole document doing so (bwsalmon/agents
// #544), which a banner pushing everything else down would fight. zIndex
// 5 sits between BatchActionsBar's 4 and ErrorBanner's 6 (both style.css)
// -- above the normal page, below a toast, and below an open Dialog's own
// 1300 (Overlay.jsx) so it never blocks a modal actually in front of it.
export default function ReconcilerDownBanner() {
  return (
    <Alert
      severity="error"
      variant="filled"
      sx={{
        position: "fixed",
        top: 0,
        left: 0,
        right: 0,
        zIndex: 5,
        borderRadius: 0,
        justifyContent: "center",
      }}
    >
      This deployment's reconcile loop has stopped -- no tasks are being
      dispatched or checked. Restart the daemon to recover.
    </Alert>
  );
}
