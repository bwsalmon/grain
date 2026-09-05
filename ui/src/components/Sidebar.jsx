import {
  Box,
  Button,
  Chip,
  Divider,
  List,
  ListItemButton,
  ListItemText,
  Typography,
} from "@mui/material";
import { STATE_LABELS, STATE_ORDER, repoRows } from "../state.js";
import { SIDEBAR_WIDTH } from "../theme.js";
import { useTimeZone } from "../TimeZoneContext.jsx";
import { formatDateTime } from "../time.js";
import GrainMark from "./GrainMark.jsx";
import ItemGlyph from "./ItemGlyph.jsx";

// Sidebar replaces TopBar and Filters with the one nav Plane builds every
// view around: a fixed rail with the workspace identity up top, a state
// list styled like Plane's own status groups (a dot standing in for the
// state's badge color, a count on the right), and the deployment-level
// actions (settings) pinned to the bottom. Schedules and Releases
// live in their own nav pane / the repo pane (bwsalmon/agents#455,
// bwsalmon/agents#398), rather than their own buttons here.
//
// Upgrade lives as a tab inside Settings (bwsalmon/agents#456), as
// Secrets did until grain/task-110 broke it up: every secret is now set
// from whatever uses it -- the agent credentials on Settings' Agents
// tab, a capability's own on its row on the Capabilities tab.
// Logs, Sandbox health and the reboot control (previously their own nav
// entries/a "danger zone" on the general tab -- bwsalmon/agents#457,
// bwsalmon/agents#536, bwsalmon/agents#395) went through Settings' own
// Debug tab for a while too (bwsalmon/agents#623), but moved back out
// to an entry of its own here, under Settings (bwsalmon/agents#640) --
// diagnosing a deployment gone wrong wants faster reach than a tab
// buried inside Settings' configuration form. That entry is called
// "System" (grain/task-12): it is the machine this deployment runs on,
// which an operator looks at to see what it is doing and not only when
// they have already decided something is broken.
//
// Metrics (GET /api/metrics) was a fourth tab in there and is a third
// footer entry now (grain/task-173): the throughput and latency report
// is the standing question "how is this deployment doing", asked when
// nothing is wrong at all, and it was the only thing behind that pane's
// tab strip that was not about something being wrong right now. Two
// clicks and a tab strip is the wrong price for a report somebody reads
// weekly, and burying it under the word "debug" -- what that pane was
// called then -- said the wrong thing about when to open it.
//
// All three footer entries open a full pane beside this rail rather
// than a dialog over the middle of the screen (grain/task-115), which is
// why they are nav entries carrying a selected state now rather than
// plain buttons: the rail stays visible under the pane, so it has to say
// which of the three is covering everything else.
// parseStampTime turns GET /api/config's committedAt into a Date, or
// null for a build whose stamp carried no readable time -- the server
// leaves the field off entirely in that case (pkg/ui/version.go), and a
// value that will not parse is treated the same way rather than printed
// as "Invalid Date".
function parseStampTime(iso) {
  if (!iso) return null;
  const at = new Date(iso);
  return Number.isNaN(at.getTime()) ? null : at;
}

// buildStampLabel is the one line the footer prints: the seven-character
// commit and, after it, when that commit was made.
//
// UTC, to the minute. An operator comparing what a deployment is running
// against `git log` is comparing against commit times, which are the
// same everywhere -- and a stamp rendered in the reader's own zone would
// say something different in every browser looking at the same
// deployment. The tooltip gives the reading on the deployment's own
// clock (grain/task-368) for whoever wants a time they can compare with
// the rest of the UI.
//
// "-dirty" borrows git's own word for it (`git describe --dirty`): the
// build ran against uncommitted changes, so the hash beside it does not
// fully describe what is running. Never true of a CI build.
function buildStampLabel(version) {
  const short = version.commit.slice(0, 7) + (version.modified ? "-dirty" : "");
  const at = parseStampTime(version.committedAt);
  if (!at) return short;
  return `${short} · ${at.toISOString().slice(0, 16).replace("T", " ")}Z`;
}

function buildStampTitle(version, zone) {
  const parts = [`Running commit ${version.commit}`];
  const at = parseStampTime(version.committedAt);
  if (at) parts.push(`committed ${formatDateTime(at, zone)}`);
  if (version.modified)
    parts.push("built from a tree with uncommitted changes");
  return parts.join(", ");
}

export default function Sidebar({
  config,
  tasks,
  schedules = [],
  templates = [],
  suites = [],
  view,
  onSetView,
  stateFilter,
  onSetFilter,
  showSettings = false,
  showSystem = false,
  showMetrics = false,
  onOpenSettings,
  onOpenSystem,
  onOpenMetrics,
  onOpenNewTask,
}) {
  const zone = useTimeZone();
  const counts = {};
  let blocked = 0;
  for (const t of tasks) {
    counts[t.state] = (counts[t.state] || 0) + 1;
    if (t.blocked) blocked += 1;
  }
  const repoCount = repoRows(config, tasks).length;

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
      <span className="nav-icon">
        <span className={`dot ${dotClass}`} />
      </span>
      <ListItemText
        primary={label}
        sx={{ ml: 1 }}
        primaryTypographyProps={{
          noWrap: true,
          fontSize: "0.85rem",
          fontWeight: 500,
        }}
      />
      <Typography
        variant="caption"
        color={active ? "primary" : "text.secondary"}
        sx={{ fontVariantNumeric: "tabular-nums" }}
      >
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
      {/* The mark beside the wordmark is the one place the whole
          deployment's state shows as something other than a number: it
          sits still while nothing is running and its grains scatter and
          re-form while agents are working, so a glance at the sidebar
          says whether the machine is busy without reading the counts
          below it. See docs/brand.md. */}
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.9, px: 0.5 }}>
        <GrainMark size={32} animated={(counts.running || 0) > 0} />
        <Typography
          variant="subtitle1"
          fontWeight={600}
          letterSpacing="-0.01em"
          component="h1"
          sx={{ m: 0 }}
        >
          grain
        </Typography>
        {/* The deployment's own name (grain/task-69), next to the
            wordmark because that is the one piece of chrome on screen in
            every view: an operator with a staging tab and a production
            tab open is one click from approving or merging on the wrong
            one, and the two are otherwise pixel-identical. Warning
            colour rather than the brand accent for the same reason --
            it should not blend into the mark beside it. Absent entirely
            when nothing is configured, which is grain's own shape for a
            single deployment with nothing to be told apart from. App.jsx
            puts the same name in the browser tab's title, for the tab
            strip this box cannot reach. */}
        {config?.environmentName ? (
          <Chip
            label={config.environmentName}
            size="small"
            color="warning"
            title={`Environment: ${config.environmentName}`}
            sx={{ ml: "auto", maxWidth: 110, fontWeight: 600 }}
          />
        ) : null}
      </Box>

      <Button variant="contained" fullWidth onClick={onOpenNewTask}>
        + New task
      </Button>

      <List
        component="nav"
        disablePadding
        sx={{ display: "flex", flexDirection: "column", gap: 0.2 }}
      >
        <NavItem
          id="all"
          label="All tasks"
          dotClass="dot-all"
          count={tasks.length}
          active={view === "tasks" && stateFilter === "all"}
        />
        {STATE_ORDER.filter((s) => counts[s]).map((s) => (
          <NavItem
            key={s}
            id={s}
            label={STATE_LABELS[s]}
            dotClass={`dot-${s}`}
            count={counts[s]}
            active={view === "tasks" && stateFilter === s}
          />
        ))}
        {/* Blocked is not a state (docs/data-model.md) so it gets its own
            nav entry alongside the state ones rather than a slot in
            STATE_ORDER -- a task stays under its own state filter too,
            this is just a faster way to find what dispatch is currently
            skipping over. */}
        {blocked > 0 && (
          <NavItem
            id="blocked"
            label="Blocked"
            dotClass="dot-blocked"
            count={blocked}
            active={view === "tasks" && stateFilter === "blocked"}
          />
        )}
        <Divider sx={{ my: 0.7 }} />
        {/* The board (TaskBoard.jsx, grain/task-287) is the same tasks
            as the entries above it, in columns of the operator's own
            choosing -- so it sits here among the destinations rather
            than in the state list, whose entries each answer "show me
            this one state" and always land on the flat list.
            No count beside it, unlike its neighbours: what a board is
            showing is decided by its own columns, which live in the
            board and not here, and "all tasks" is already counted at
            the top of this rail. The board shows tasks, not a list of
            its own kind of thing, so its marker is the state entries'
            invisible `dot dot-all` rather than one of the four item
            glyphs below -- in the same `nav-icon` column as both, so
            every label in this rail still lines up. */}
        <ListItemButton
          selected={view === "board"}
          onClick={() => onSetView("board")}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <span className="nav-icon">
            <span className="dot dot-all" />
          </span>
          <ListItemText
            primary="Board"
            sx={{ ml: 1 }}
            primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }}
          />
        </ListItemButton>
        {/* The four list entries carry their own glyph where the state
            entries above carry a coloured dot -- each one a Chladni
            figure off the same plate as the mark at the top of this
            rail, in a mode the mark's own cycle does not use
            (src/brand/item-glyphs.js, docs/brand.md). Until now all
            four sat behind an invisible `dot dot-all`, there purely to
            keep their labels in the same column as the states' -- so
            the only thing telling Schedules from Suites at a glance was
            reading the word. The glyph inherits the entry's text
            colour, so it brightens with the label when the entry is
            selected. */}
        <ListItemButton
          selected={view === "repos"}
          onClick={() => onSetView("repos")}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <span className="nav-icon">
            <ItemGlyph kind="repos" />
          </span>
          <ListItemText
            primary="Repos"
            sx={{ ml: 1 }}
            primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }}
          />
          <Typography variant="caption" color="text.secondary">
            {repoCount}
          </Typography>
        </ListItemButton>
        <ListItemButton
          selected={view === "schedules"}
          onClick={() => onSetView("schedules")}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <span className="nav-icon">
            <ItemGlyph kind="schedules" />
          </span>
          <ListItemText
            primary="Schedules"
            sx={{ ml: 1 }}
            primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }}
          />
          <Typography variant="caption" color="text.secondary">
            {schedules.length}
          </Typography>
        </ListItemButton>
        <ListItemButton
          selected={view === "templates"}
          onClick={() => onSetView("templates")}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <span className="nav-icon">
            <ItemGlyph kind="templates" />
          </span>
          <ListItemText
            primary="Templates"
            sx={{ ml: 1 }}
            primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }}
          />
          <Typography variant="caption" color="text.secondary">
            {templates.length}
          </Typography>
        </ListItemButton>
        <ListItemButton
          selected={view === "suites"}
          onClick={() => onSetView("suites")}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <span className="nav-icon">
            <ItemGlyph kind="suites" />
          </span>
          <ListItemText
            primary="Suites"
            sx={{ ml: 1 }}
            primaryTypographyProps={{ fontSize: "0.85rem", fontWeight: 500 }}
          />
          <Typography variant="caption" color="text.secondary">
            {suites.length}
          </Typography>
        </ListItemButton>
      </List>

      {/* No dot beside any of them, unlike every nav entry above: those
          count something, and these three are one destination each. */}
      <List
        component="nav"
        disablePadding
        sx={{ mt: "auto", display: "flex", flexDirection: "column", gap: 0.2 }}
      >
        <Divider sx={{ mb: 0.9 }} />
        <ListItemButton
          selected={showSettings}
          onClick={onOpenSettings}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <ListItemText
            primary="Settings"
            primaryTypographyProps={{
              fontSize: "0.85rem",
              fontWeight: 500,
              color: showSettings ? "text.primary" : "text.secondary",
            }}
          />
        </ListItemButton>
        <ListItemButton
          selected={showSystem}
          onClick={onOpenSystem}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <ListItemText
            primary="System"
            primaryTypographyProps={{
              fontSize: "0.85rem",
              fontWeight: 500,
              color: showSystem ? "text.primary" : "text.secondary",
            }}
          />
        </ListItemButton>
        <ListItemButton
          selected={showMetrics}
          onClick={onOpenMetrics}
          sx={{ borderRadius: 1.5, py: 0.6, px: 0.9 }}
        >
          <ListItemText
            primary="Metrics"
            primaryTypographyProps={{
              fontSize: "0.85rem",
              fontWeight: 500,
              color: showMetrics ? "text.primary" : "text.secondary",
            }}
          />
        </ListItemButton>
      </List>

      {/* Which build of grain is answering: the commit it was built from
          and when that commit was made, out of the binary's own VCS
          stamp (GET /api/config's "version", pkg/version). The question
          "is this deployment running the change I just merged?" is asked
          of a UI far more often than of a shell on the host, and every
          other answer to it -- an image tag, a merge time, the upgrade
          pane's last run -- is a claim about what was deployed rather
          than about what is serving this page.
          Deliberately the smallest thing on the rail, below the nav
          rather than in it: it is worth a glance during a deploy and
          nothing at all on any other day. Absent entirely on a binary
          built without a stamp (`-buildvcs=false`, or a `go test`
          binary), which is the same shape the environment chip above
          takes when there is nothing to say. */}
      {config?.version?.commit ? (
        <Typography
          variant="caption"
          component="p"
          color="text.secondary"
          noWrap
          title={buildStampTitle(config.version, zone)}
          sx={{
            px: 0.9,
            m: 0,
            fontSize: "0.68rem",
            opacity: 0.75,
            fontVariantNumeric: "tabular-nums",
          }}
        >
          {buildStampLabel(config.version)}
        </Typography>
      ) : null}
    </Box>
  );
}
