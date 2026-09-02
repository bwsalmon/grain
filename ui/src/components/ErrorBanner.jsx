import { Alert, Snackbar } from "@mui/material";

export default function ErrorBanner({ message }) {
  return (
    <Snackbar open anchorOrigin={{ vertical: "bottom", horizontal: "center" }} sx={{ zIndex: 6 }}>
      <Alert severity="error" variant="filled" sx={{ maxWidth: "min(90vw, 480px)" }}>
        {message}
      </Alert>
    </Snackbar>
  );
}
