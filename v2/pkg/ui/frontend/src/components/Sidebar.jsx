import { Box, Button, Divider, List, ListItemButton, ListItemText, Typography } from "@mui/material";
import { STATE_LABELS, STATE_ORDER, reposFromTasks } from "../state.js";

const SIDEBAR_WIDTH = 232;

// Sidebar replaces TopBar and Filters with the one nav Plane builds every
// view around: a fixed rail with the workspace identity up top, a state
// list styled like Plane's own status groups (a dot standing in for the
// state's badge color, a count on the right), and the deployment-level
// actions (secrets, settings) pinned to the bottom.
export default function Sidebar({ config, tasks, schedules = [], view, onSetView, stateFilter, onSetFilter, onOpenSecrets, onOpenSettings, onOpenUpgrade, onOpenNewTask }) {
  const repoName = config ? (config.defaultTarget ? config.defaultTarget : `as ${config.actor}`) : "";

  const counts = {};
  let blocked = 0;
  for (const t of tasks) {
    counts[t.state] = (counts[t.state] || 0) + 1;
    if (t.blocked) blocked += 1;
  }
  const repoCount = reposFromTasks(tasks).length;

  // Picking a state filter always lands on the task list -- there is
  // nothing for it to scope on the repo page.
  const selectState = (id) => {
    onSetFilter(id);
    onSetView("tasks");
  };

  const NavItem = ({ id, label, dotClass, count, active }) => (
    <ListItemButton
      selected={active}
      onClick={() => selectState(id)}
      sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
    >
      <span className={`dot ${dotClass}`} />
      <ListItemText
        primary={label}
        sx={{ ml: 1 }}
        primaryTypographyProps={{ noWrap: true, fontSize: "0.85rem", fontWeight: 500 }}
      />
      <Typography variant="caption" color={active ? "primary" : "text.secondary"} sx={{ fontVariantNumeric: "tabular-nums" }}>
        {count}
      </Typography>
    </ListItemButton>
  );

  return (
    <Box
      component="aside"
      sx={{
        width: SIDEBAR_WIDTH,
        flex: "none",
        display: "flex",
        flexDirection: "column",
        gap: 1.4,
        p: "0.9rem 0.7rem",
        bgcolor: "background.paper",
        borderRight: 1,
        borderColor: "divider",
      }}
    >
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.9, px: 0.5 }}>
        <span className="brand-mark" />
        <Typography variant="subtitle1" fontWeight={600} letterSpacing="-0.01em" component="h1" sx={{ m: 0 }}>
          grain
        </Typography>
      </Box>
      {repoName && (
        <Typography variant="caption" color="text.secondary" noWrap title={repoName} sx={{ px: 0.5, mt: -1 }}>
          {repoName}
        </Typography>
      )}

      <Button variant="contained" fullWidth onClick={onOpenNewTask}>+ New task</Button>

      <List component="nav" disablePadding sx={{ display: "flex", flexDirection: "column", gap: 0.2 }}>
        <ListItemButton selected={view === "repos"} onClick={() => onSetView("repos")} sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}>
          <span className="dot dot-all" />
          <ListItemText primary="Repos" sx={{ ml: 1 }} primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }} />
          <Typography variant="caption" color="text.secondary">{repoCount}</Typography>
        </ListItemButton>
        <ListItemButton selected={view === "schedules"} onClick={() => onSetView("schedules")} sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}>
          <span className="dot dot-all" />
          <ListItemText primary="Scheduled tasks" sx={{ ml: 1 }} primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }} />
          <Typography variant="caption" color="text.secondary">{schedules.length}</Typography>
        </ListItemButton>
        <ListItemButton selected={view === "logs"} onClick={() => onSetView("logs")} sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}>
          <span className="dot dot-all" />
          <ListItemText primary="Logs" sx={{ ml: 1 }} primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }} />
        </ListItemButton>
        <NavItem id="all" label="All issues" dotClass="dot-all" count={tasks.length} active={view === "tasks" && stateFilter === "all"} />
        {STATE_ORDER.filter((s) => counts[s]).map((s) => (
          <NavItem key={s} id={s} label={STATE_LABELS[s]} dotClass={`dot-${s}`} count={counts[s]} active={view === "tasks" && stateFilter === s} />
        ))}
        {/* Blocked is not a state (docs/data-model.md) so it gets its own
            nav entry alongside the state ones rather than a slot in
            STATE_ORDER -- a task stays under its own state filter too,
            this is just a faster way to find what dispatch is currently
            skipping over. */}
        {blocked > 0 && (
          <NavItem id="blocked" label="Blocked" dotClass="dot-blocked" count={blocked} active={view === "tasks" && stateFilter === "blocked"} />
        )}
      </List>

      <Box sx={{ mt: "auto", display: "flex", flexDirection: "column", gap: 0.2 }}>
        <Divider sx={{ mb: 0.9 }} />
        <Button color="inherit" sx={{ justifyContent: "flex-start", px: 0.9, py: 0.6, fontSize: "0.85rem", fontWeight: 500, color: "text.secondary" }} onClick={onOpenSecrets}>Secrets</Button>
        <Button color="inherit" sx={{ justifyContent: "flex-start", px: 0.9, py: 0.6, fontSize: "0.85rem", fontWeight: 500, color: "text.secondary" }} onClick={onOpenSettings}>Settings</Button>
        <Button color="inherit" sx={{ justifyContent: "flex-start", px: 0.9, py: 0.6, fontSize: "0.85rem", fontWeight: 500, color: "text.secondary" }} onClick={onOpenUpgrade}>Upgrade</Button>
      </Box>
    </Box>
  );
}
