import CloseIcon from "@mui/icons-material/Close";
import { Button, Dialog, IconButton } from "@mui/material";
import { useIsPhone } from "../phone.js";
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
// result everywhere. Settings and System are panes too now
// (grain/task-115): both are a whole destination behind a tab strip
// rather than one form to fill in and dismiss, and a centered box was
// making a log tail, a sandbox table and a six-tab settings form share
// the same 600-900px porthole. What is left in a centered box is what
// really is a single *action*: New task, Run a suite, an attempt's
// transcript.
//
// `header` is the pane's own fixed chrome, above the part that scrolls:
// a title and a tab strip that stay put while the tab's content moves
// under them, so a pane the height of the screen never scrolls its own
// tabs off the top. Panes without one (a task, a schedule) pass nothing
// and scroll their whole body as before.
//
// A pane leaves by a back button in its top-left corner, not by an X in
// its top-right (grain/task-177). A pane is a destination -- opening a
// task or a schedule fills the whole content area beside the sidebar and
// puts its own URL in the address bar -- and the other destinations of
// that kind, a repo's page (RepoPage) and a repo's releases
// (RepoReleases), have always left by "← Repos" on the left. Two
// gestures for the same "go back where I came from", in opposite corners
// of the screen, was the inconsistency. `backLabel` names where that is
// for the panes that only ever open from one list; the default says just
// "Back", for the panes (a task, Settings, System, Metrics) that can be
// opened from more than one place and so cannot honestly name one.
//
// The centered shape keeps its X: New task, Run a suite and an attempt's
// transcript are single actions taken over the page you are already on,
// not somewhere you navigated to, and there is nothing to go "back" to.
export default function Overlay({
  onClose,
  wide = false,
  pane = false,
  backLabel = "Back",
  header = null,
  children,
}) {
  // On a phone the rail is not beside the pane at all -- it is in
  // PhoneNav's drawer -- so there is nothing to leave room for and the
  // pane takes the whole width. Asked of phone.js rather than of the
  // `sm` breakpoint below, which a phone held sideways is wide enough to
  // clear while having no rail on screen to spare the width for.
  const isPhone = useIsPhone();
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
            width: isPhone
              ? "100%"
              : { xs: "100%", sm: `calc(100% - ${SIDEBAR_WIDTH}px)` },
            borderLeft: 1,
            borderColor: "divider",
          },
        }}
      >
        {/* In the flow above the header rather than floating over the
            corner, so it never lands on top of a title or a tab strip --
            the same place, and the same negative margin pulling the
            label out to the pane's own left edge, as RepoPage's. */}
        <div className="overlay-pane-back">
          <Button onClick={onClose} sx={{ ml: -0.9 }}>
            &larr; {backLabel}
          </Button>
        </div>
        {header !== null && <div className="overlay-pane-header">{header}</div>}
        {/* The pane's own body scrolls, not the document: the paper is
            the viewport's full height and its content (a long timeline,
            a long form) is what overflows. With a header above it, this
            is the only part that moves. */}
        <div className="overlay-pane">{children}</div>
      </Dialog>
    );
  }
  // The centered box is a box on anything with room for one, and the
  // whole screen on a phone: New task is a form of a dozen fields, and
  // MUI's own centered shape would leave it a 32px margin on each side
  // and the rest of the page showing uselessly behind it. Full screen it
  // is the paper, not the document, that scrolls (scroll="paper"), so
  // the body below has to be the part that moves.
  const body = isPhone
    ? {
        padding: "1.25rem 1rem",
        paddingBottom: "calc(1.25rem + env(safe-area-inset-bottom, 0px))",
        flex: 1,
        minHeight: 0,
        overflowY: "auto",
      }
    : { padding: "1.5rem" };
  return (
    <Dialog
      open
      onClose={onClose}
      maxWidth={wide ? "md" : "sm"}
      fullWidth
      fullScreen={isPhone}
      scroll={isPhone ? "paper" : "body"}
      sx={
        isPhone
          ? { "& .MuiDialog-paper": { pt: "env(safe-area-inset-top, 0px)" } }
          : undefined
      }
    >
      <IconButton
        aria-label="Close dialog"
        onClick={onClose}
        size="small"
        sx={{
          position: "absolute",
          // Absolute positioning ignores the paper's own padding, so the
          // safe-area inset the paper takes above has to be added here
          // too or the X lands under a notched phone's status bar.
          top: isPhone ? "calc(8px + env(safe-area-inset-top, 0px))" : 8,
          right: 8,
          color: "text.secondary",
        }}
      >
        <CloseIcon fontSize="small" />
      </IconButton>
      <div style={body}>{children}</div>
    </Dialog>
  );
}
