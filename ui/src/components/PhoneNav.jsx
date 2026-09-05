import { useState } from "react";
import AddIcon from "@mui/icons-material/Add";
import MenuIcon from "@mui/icons-material/Menu";
import {
  AppBar,
  Box,
  Chip,
  Drawer,
  IconButton,
  Toolbar,
  Typography,
} from "@mui/material";
import GrainMark from "./GrainMark.jsx";

// PhoneNav is what the sidebar becomes on a phone (phone.js): the rail
// itself is unchanged and moves, whole, into a drawer that slides in
// over the page, and what stays on screen is one bar across the top --
// the mark, the wordmark, the deployment's name, and the two controls
// worth a permanent tap target.
//
// The rail is not rebuilt for the small screen, it is *hidden* behind a
// button: every entry in it (the state filters, the four lists, the
// board, Settings/System/Metrics) is still one tap away, and there is
// only one nav in the tree to keep correct. A bottom tab bar would have
// meant choosing four of a dozen destinations and leaving the rest
// somewhere else.
//
// The two controls that do not go behind the button are the menu itself
// and "+" -- filing a task is the one thing somebody does on a phone
// without navigating anywhere first, and it is the sidebar's own primary
// button, so making it cost two taps would be the wrong trade.
//
// Which build is running (the sidebar's footer stamp) is deliberately
// left in the drawer rather than promoted here: it is worth a glance
// during a deploy and nothing on any other day, and this bar has one row
// to spend. The environment chip is the opposite case and rides along --
// the sidebar's own argument for it is that it is on screen in every
// view, which on a phone means here or nowhere.
export default function PhoneNav({ config, running, onOpenNewTask, children }) {
  const [open, setOpen] = useState(false);

  return (
    <>
      {/* position="static", not "fixed": the shell is a flex column on a
          phone (style.css), so this bar takes its own row at the top and
          the page scrolls beneath it without needing an offset that
          would have to be kept in step with the bar's height. */}
      <AppBar
        position="static"
        elevation={0}
        color="default"
        sx={{
          flex: "none",
          bgcolor: "background.paper",
          borderBottom: 1,
          borderColor: "divider",
          // The status bar's own strip on a notched phone: index.html
          // asks for viewport-fit=cover, so without this the bar's
          // contents sit under the clock.
          pt: "env(safe-area-inset-top, 0px)",
        }}
      >
        <Toolbar variant="dense" sx={{ gap: 1, minHeight: 52 }}>
          <IconButton
            edge="start"
            aria-label="Open navigation"
            onClick={() => setOpen(true)}
          >
            <MenuIcon />
          </IconButton>
          {/* Animated while anything is running, exactly as in the rail:
              on a phone this is the only piece of the deployment's live
              state that is always on screen. */}
          <GrainMark size={24} animated={running} />
          <Typography
            component="h1"
            variant="subtitle1"
            fontWeight={600}
            letterSpacing="-0.01em"
            noWrap
            sx={{ m: 0, flex: 1, minWidth: 0 }}
          >
            grain
          </Typography>
          {config?.environmentName ? (
            <Chip
              label={config.environmentName}
              size="small"
              color="warning"
              title={`Environment: ${config.environmentName}`}
              sx={{ maxWidth: 110, fontWeight: 600 }}
            />
          ) : null}
          <IconButton edge="end" aria-label="New task" onClick={onOpenNewTask}>
            <AddIcon />
          </IconButton>
        </Toolbar>
      </AppBar>

      <Drawer
        open={open}
        onClose={() => setOpen(false)}
        aria-label="Navigation"
        // The rail is 232px of a ~390px screen, so the page it is
        // covering stays visible down the right-hand edge -- which is
        // what makes it read as a drawer over the page rather than as a
        // new screen.
        PaperProps={{ sx: { pt: "env(safe-area-inset-top, 0px)" } }}
      >
        {/* Every control in the rail is a navigation -- a state filter,
            a list, a pane -- and a phone drawer that stays open over the
            page it just navigated is in the way. So one handler here
            closes it on any tap that lands inside, rather than the rail
            growing an onClose-aware copy of each of its dozen
            callbacks. */}
        <Box
          onClick={() => setOpen(false)}
          sx={{ display: "flex", height: "100%" }}
        >
          {children}
        </Box>
      </Drawer>
    </>
  );
}
