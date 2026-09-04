import { createTheme } from "@mui/material/styles";

// The palettes below carry over the accent/danger/surface values that
// style.css's own :root and prefers-color-scheme:dark blocks already
// defined (bwsalmon/agents#364's Plane-inspired tokens) so adopting MUI's
// primitives (Button, TextField, Dialog, ...) does not also reset the
// app's look. style.css keeps those custom properties for the pieces MUI
// has no primitive for -- state dots, badges, the task list layout.
//
// Every value here has a twin in style.css and the two have to be
// changed together. The accent is the brand mark's own grain colour --
// bronze (#8A6A2E) on the light ground, wheat (#D9A441) on the dark one,
// which is the split the mark itself is drawn with because the gold
// disappears against white; the greys are warmed to match. See
// docs/brand.md.
// SIDEBAR_WIDTH is the one layout number two pieces of chrome have to
// agree on: the sidebar itself (Sidebar.jsx) and the pane anything you
// open fills (Overlay.jsx's own `pane`), which starts exactly where the
// sidebar ends. It lives here rather than in either of them so neither
// owns the other's width.
export const SIDEBAR_WIDTH = 232;

const shared = {
  shape: { borderRadius: 6 },
  typography: {
    fontFamily:
      '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
    fontSize: 14,
    button: { textTransform: "none", fontWeight: 500 },
  },
  components: {
    MuiButton: {
      styleOverrides: {
        root: { borderRadius: 6 },
      },
    },
  },
};

export const lightTheme = createTheme({
  ...shared,
  palette: {
    mode: "light",
    primary: { main: "#8a6a2e", dark: "#6d5324", contrastText: "#ffffff" },
    error: { main: "#d0453a" },
    // success mirrors --state-completed; warning is pulled off MUI's own
    // bright orange to a burnt one that reads as the accent's louder
    // cousin rather than fighting it; info is the one cool tone left in
    // the app, gold's complement, which is what an Alert that is only
    // telling you something should be next to all this warmth.
    success: { main: "#1a9c53" },
    warning: { main: "#a8621c" },
    info: { main: "#3f6b8a" },
    background: { default: "#f7f6f3", paper: "#ffffff" },
    text: { primary: "#24221d", secondary: "#6e6a60" },
    divider: "#e6e3dc",
  },
});

export const darkTheme = createTheme({
  ...shared,
  palette: {
    mode: "dark",
    // primary.dark is MUI's hover shade, and on this ground the hover has
    // to be *lighter* than the resting colour -- the same inversion the
    // light palette above does not need.
    primary: { main: "#d9a441", dark: "#e8b962", contrastText: "#17130a" },
    error: { main: "#ec7469" },
    success: { main: "#3ec573" },
    warning: { main: "#e29a4e" },
    info: { main: "#7fa8c7" },
    background: { default: "#0f1013", paper: "#14161a" },
    text: { primary: "#e9e7e1", secondary: "#9b988f" },
    divider: "#292b31",
  },
});
