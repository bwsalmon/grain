import CloseIcon from "@mui/icons-material/Close";
import { Dialog, IconButton } from "@mui/material";

// Shared overlay chrome, now MUI's own Dialog: it already closes on a
// backdrop click and Escape, and traps focus the way the old hand-rolled
// backdrop div never did. maxWidth/fullWidth stand in for panel/
// panel-detail's two widths from style.css.
export default function Overlay({ onClose, wide = false, children }) {
  return (
    <Dialog open onClose={onClose} maxWidth={wide ? "md" : "sm"} fullWidth scroll="body">
      <IconButton
        aria-label="Close dialog"
        onClick={onClose}
        size="small"
        sx={{ position: "absolute", top: 8, right: 8, color: "text.secondary" }}
      >
        <CloseIcon fontSize="small" />
      </IconButton>
      <div style={{ padding: "1.5rem" }}>{children}</div>
    </Dialog>
  );
}
