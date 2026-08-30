import { useMemo } from "react";
import { CssBaseline, ThemeProvider, useMediaQuery } from "@mui/material";
import { darkTheme, lightTheme } from "./theme.js";

// The app previously followed the OS light/dark setting through a plain
// prefers-color-scheme media query in style.css; useMediaQuery here
// subscribes to the same signal so MUI's theme -- and CssBaseline's
// background/text reset -- track it the same way.
export default function AppThemeProvider({ children }) {
  const prefersDark = useMediaQuery("(prefers-color-scheme: dark)");
  const theme = useMemo(() => (prefersDark ? darkTheme : lightTheme), [prefersDark]);

  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      {children}
    </ThemeProvider>
  );
}
