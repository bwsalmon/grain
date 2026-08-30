import { useEffect, useMemo } from "react";
import { CssBaseline, ThemeProvider, useMediaQuery } from "@mui/material";
import { darkTheme, lightTheme } from "./theme.js";
import { ThemeModeProvider, useThemeMode } from "./ThemeModeContext.jsx";

// Split out from AppThemeProvider so useThemeMode's context is available:
// the resolved light/dark choice depends on both the user's stored mode
// and (when that mode is "auto") the OS-level prefers-color-scheme signal.
function ResolvedThemeProvider({ children }) {
  const { mode } = useThemeMode();
  const prefersDark = useMediaQuery("(prefers-color-scheme: dark)");
  const resolvedMode = mode === "auto" ? (prefersDark ? "dark" : "light") : mode;
  const theme = useMemo(() => (resolvedMode === "dark" ? darkTheme : lightTheme), [resolvedMode]);

  // style.css keys its own custom properties (state dots, badges, the
  // task list layout -- pieces MUI has no primitive for) off this
  // attribute rather than duplicating MUI's palette choice in JS.
  useEffect(() => {
    document.documentElement.dataset.theme = resolvedMode;
  }, [resolvedMode]);

  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      {children}
    </ThemeProvider>
  );
}

export default function AppThemeProvider({ children }) {
  return (
    <ThemeModeProvider>
      <ResolvedThemeProvider>{children}</ResolvedThemeProvider>
    </ThemeModeProvider>
  );
}
