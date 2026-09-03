import CloseIcon from "@mui/icons-material/Close";
import { Dialog, IconButton } from "@mui/material";
import { SIDEBAR_WIDTH } from "../theme.js";

// Shared overlay chrome, now MUI's own Dialog: it already closes on a
// backdrop click and Escape, and traps focus the way the old hand-rolled
// backdrop div never did. maxWidth/fullWidth stand in for panel/
// panel-detail's two widths from style.css.
//
// `pane` is the third width, and the one anything you *open* uses
// (grain/task-94): a task, a schedule, a template or a suite fills the
// whole content area beside the sidebar, top to bottom, instead of a
// box floating in the middle of it. An agent's own answer inside a task
// runs to headings, lists and fenced diffs, and reading that through a
// 900px porthole was the complaint. The three lists whose "+" button and
// whose rows open the same component (schedules, templates, suites) get
// it for both, so opening one item is the same gesture with the same
// result everywhere. Dialogs that are an *action* rather than a thing --
// New task, Run a suite, Settings, Debug, an attempt's transcript --
// stay centered boxes.
export default function Overlay({ onClose, wide = false, pane = false, children }) {
  if (pane) {
    return (
      <Dialog
        open
        onClose={onClose}
        fullScreen
        scroll="paper"
        sx={{
          // The container is a flex row; pushing the paper to the end and
          // sizing it to "everything but the sidebar" is what leaves the
          // sidebar showing beside it rather than covered by it. Below
          // the sidebar's own breakpoint there is no room to spare, so
          // the pane takes the width outright.
          "& .MuiDialog-container": { justifyContent: "flex-end" },
          "& .MuiDialog-paper": {
            width: { xs: "100%", sm: `calc(100% - ${SIDEBAR_WIDTH}px)` },
            borderLeft: 1,
            borderColor: "divider",
          },
        }}
      >
        <IconButton
          aria-label="Close dialog"
          onClick={onClose}
          size="small"
          sx={{ position: "absolute", top: 8, right: 8, color: "text.secondary", zIndex: 1 }}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
        {/* The pane's own body scrolls, not the document: the paper is
            the viewport's full height and its content (a long timeline,
            a long form) is what overflows. */}
        <div className="overlay-pane">{children}</div>
      </Dialog>
    );
  }
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
