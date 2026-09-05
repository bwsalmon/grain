import { Box } from "@mui/material";

// BannerStrip is the pinned strip at the top of the page that every
// standing, deployment-wide banner lives in -- ReconcilerDownBanner,
// AgentPauseBanner, HostSandboxesBanner.
//
// Each of those used to position itself (position: fixed, top: 0), which
// worked only for as long as exactly one of them could ever be on screen:
// two pinned to the same coordinates draw on top of each other. Host
// sandboxing broke that assumption (grain/task-15) -- it is true for the
// life of the deployment rather than for the length of an incident, so it
// coincides with whatever else is wrong, and it is not a warning any of
// those should be allowed to hide. So the position moved out here, once,
// and the banners stack inside it.
//
// The layout is what ReconcilerDownBanner carried before, unchanged:
// position: fixed (rather than a row in .app-shell's own flex layout,
// style.css) keeps this from touching that layout at all -- .app-shell is
// locked to height: 100vh precisely so panes scroll internally instead of
// the whole document doing so (bwsalmon/agents#544), which a banner
// pushing everything else down would fight. zIndex 5 sits between
// BatchActionsBar's 4 and ErrorBanner's 6 (both style.css) -- above the
// normal page, below a toast, and below an open Dialog's own 1300
// (Overlay.jsx) so it never blocks a modal actually in front of it.
export default function BannerStrip({ children }) {
  return (
    <Box
      sx={{
        position: "fixed",
        top: 0,
        left: 0,
        right: 0,
        zIndex: 5,
        display: "flex",
        flexDirection: "column",
      }}
    >
      {children}
    </Box>
  );
}
