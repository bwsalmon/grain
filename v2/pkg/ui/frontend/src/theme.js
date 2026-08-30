import { createTheme } from "@mui/material/styles";

// The palettes below carry over the accent/danger/surface values that
// style.css's own :root and prefers-color-scheme:dark blocks already
// defined (bwsalmon/agents#364's Plane-inspired tokens) so adopting MUI's
// primitives (Button, TextField, Dialog, ...) does not also reset the
// app's look. style.css keeps those custom properties for the pieces MUI
// has no primitive for -- state dots, badges, the task list layout.
const shared = {
  shape: { borderRadius: 6 },
  typography: {
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
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
    primary: { main: "#3f76ff", dark: "#3163e0", contrastText: "#ffffff" },
    error: { main: "#d0453a" },
    background: { default: "#f6f6f8", paper: "#ffffff" },
    text: { primary: "#23242a", secondary: "#6d6f78" },
    divider: "#e5e5ea",
  },
});

export const darkTheme = createTheme({
  ...shared,
  palette: {
    mode: "dark",
    primary: { main: "#6f95ff", dark: "#85a5ff", contrastText: "#0c1730" },
    error: { main: "#ec7469" },
    background: { default: "#101115", paper: "#18191e" },
    text: { primary: "#e8e8ec", secondary: "#9799a3" },
    divider: "#2b2c33",
  },
});
