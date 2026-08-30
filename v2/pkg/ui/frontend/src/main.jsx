import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App.jsx";
import AppThemeProvider from "./AppThemeProvider.jsx";
import "./style.css";

createRoot(document.getElementById("root")).render(
  <StrictMode>
    <AppThemeProvider>
      <App />
    </AppThemeProvider>
  </StrictMode>,
);
