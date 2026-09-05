import { useState } from "react";
import {
  Alert,
  Button,
  Chip,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import api from "../api.js";
import {
  ACTIVITY_RANK,
  STATE_LABELS,
  STATE_ORDER,
  repoActivity,
  repoRows,
} from "../state.js";
import { useListOrder } from "../listOrder.js";
import {
  ListEmpty,
  ListHeader,
  ListSearchField,
  ListSortSelect,
  ListToolbar,
  ReorderableList,
} from "./ListPrimitives.jsx";
import ItemGlyph from "./ItemGlyph.jsx";

// SORTS is this page's own toolbar Select, TaskList's own shape
// (taskFilters.js) with "custom" standing where the task list's
// "Backlog order" does: a repo has no backlog position, so the order a
// drag stores here is this browser's own display order and nothing more
// (listOrder.js). It is still the default, because a list nobody has
// dragged is in the alphabetical order it always was -- applyOrder falls
// back to exactly the comparator "Name (A–Z)" uses -- so the only thing
// the default costs is the handles that make the order draggable.
const byName = (a, b) => a.repo.localeCompare(b.repo);
const SORTS = {
  custom: { label: "Custom order", cmp: byName },
  name: { label: "Name (A–Z)", cmp: byName },
  active: {
    label: "Active first",
    cmp: (a, b) =>
      ACTIVITY_RANK[repoActivity(a).key] - ACTIVITY_RANK[repoActivity(b).key] ||
      byName(a, b),
  },
  tasks: {
    label: "Most tasks",
    cmp: (a, b) => b.total - a.total || byName(a, b),
  },
};

// RepoList is the repo index: one row per known repo -- every
// config.targetRepos entry, plus any repo tasks target that isn't one,
// plus any repo carrying configuration of its own, which today is a
// default capability set, standing instructions, or both (repoRows has
// why all three sources) -- each showing how many tasks sit in every state so a
// repo with something stuck (awaiting_reply, or a pile of blocked work)
// stands out before anyone opens it. Clicking a row opens that repo's
// own page (RepoPage, grain/task-111), which is where everything about
// one repo now lives.
//
// A row is a name and its counts, and nothing else. It used to carry a
// chevron that folded the repo's tasks out in place plus four buttons --
// New branch, Capabilities, Releases, Remove -- and a "+" for filing a
// task, each with its own stopPropagation so it wouldn't also open the
// row, and two of them folding a form out between rows (bwsalmon/agents
// #459, #473, #474, #638). All of it moved onto the repo page: a list of
// repos is for finding a repo, and each of those controls is about one
// repo, which is a thing that now has a page of its own to be on.
//
// Adding a target repo (bwsalmon/agents#473) stays here, since it is the
// one control that isn't about a repo this page is already listing --
// Remove, its counterpart, lives on the repo page beside everything else
// about that repo.
//
// "Add repo" reads backwards the first time (grain/task-45). An empty
// config.targetRepos means *unrestricted* everywhere it appears --
// CreateTask enforces nothing, `grain settings` prints "unrestricted",
// every row here is `configured: false` -- so adding the first repo does
// not widen anything, it narrows the deployment to exactly that repo,
// and the next task filed against any other repo parks off the allowlist
// instead of dispatching. Two things say so, one before the click and
// one at it: the note below the header stands on the page for as long
// as the allowlist is empty (and, once it holds one entry, says that
// too -- keyed on the list rather than on the transition, the way `grain
// repo add`'s own one-line note is), and addRepo confirms the first add
// naming the repos that would fall off. The confirmation is the one that
// can be specific, since only it knows which repo is being added; the
// standing note is what somebody reads while deciding to click at all.
//
// The header/toolbar/row shape below mirrors TaskList's own
// (bwsalmon/agents#561): a .content-header with title, count, and this
// page's own primary action (the add-repo form, in place of TaskList's
// filter title or TemplatesList/SchedulesList's "+ New X" button) above
// a .task-list-toolbar search box, then flat divider rows -- so the four
// list pages read as one design instead of four.
export default function RepoList({
  tasks,
  config,
  onOpenRepo,
  onRefreshConfig,
  showError,
}) {
  const [newRepo, setNewRepo] = useState("");
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("custom");
  const repos = repoRows(config, tasks);
  // Every repo, not just the ones the search leaves showing -- what a
  // drag stores has to know where the hidden rows sit too (listOrder.js).
  const [ordered, move] = useListOrder("repos", repos, (r) => r.repo, byName);

  const q = search.trim().toLowerCase();
  const sorted =
    sortBy === "custom" ? ordered : [...repos].sort(SORTS[sortBy].cmp);
  const visible = sorted.filter(
    (r) => q === "" || r.repo.toLowerCase().includes(q),
  );
  const targetRepos = config?.targetRepos || [];

  // firstAddWarning is what the confirmation says before the first repo
  // is added to an empty (== unrestricted) allowlist, or null when this
  // add can only ever widen an allowlist that already restricts.
  //
  // The repos it names are the rows this page is already showing. With
  // targetRepos empty none of them is `configured`, so every one is here
  // because a task targets it or because it carries default capabilities
  // of its own -- repoRows' other two sources -- and either way the
  // allowlist this click creates leaves it off, so the next task filed
  // against it parks. Worth listing, since that is the cost of the click
  // and nothing else on the page states it.
  //
  // Compared as typed rather than as the server will store it
  // (AddTargetRepo runs the string through model.ParseRepo first), so an
  // oddly-spelled repo may list itself among the fallers; the sentence
  // is still true, and the alternative is a second parse here that could
  // drift from that one.
  const firstAddWarning = (repo) => {
    if (targetRepos.length > 0) return null;
    const falling = repos.map((r) => r.repo).filter((r) => r !== repo);
    let msg =
      `Add ${repo} as the only repo this deployment allows?\n\n` +
      "This deployment doesn't restrict target repos today -- an empty allowlist is what means " +
      `unrestricted -- so adding ${repo} narrows it to that one repo rather than widening anything.`;
    if (falling.length > 0) {
      msg +=
        `\n\nOff the allowlist as of this click: ${falling.join(", ")}. ` +
        "Tasks already filed against them are unaffected, but the next one is parked instead of " +
        "dispatched until the repo is added back here.";
    }
    return msg;
  };

  const addRepo = async (evt) => {
    evt.preventDefault();
    const repo = newRepo.trim();
    if (repo === "") return;
    const warning = firstAddWarning(repo);
    if (warning !== null && !confirm(warning)) return;
    try {
      await api("/api/repos", {
        method: "POST",
        body: JSON.stringify({ repo }),
      });
      setNewRepo("");
      await onRefreshConfig();
    } catch (err) {
      showError(err);
    }
  };

  return (
    <main>
      <ListHeader
        title="Repos"
        icon={<ItemGlyph kind="repos" size={20} />}
        count={visible.length}
        style={{ alignItems: "center" }}
        action={
          <Stack
            component="form"
            direction="row"
            spacing={1}
            onSubmit={addRepo}
            sx={{ ml: "auto" }}
          >
            <TextField
              value={newRepo}
              onChange={(evt) => setNewRepo(evt.target.value)}
              placeholder="owner/name"
              size="small"
              autoComplete="off"
            />
            <Button type="submit" variant="outlined" size="small">
              Add repo
            </Button>
          </Stack>
        }
      />
      {/* The standing half of grain/task-45's answer, keyed on the
          allowlist as it stands rather than on any transition: empty is
          the state in which "Add repo" is about to restrict rather than
          widen, and one entry is the state that leaves. Neither is an
          error or a warning -- both are working deployments -- so both
          are severity="info". */}
      {targetRepos.length === 0 && (
        <Alert severity="info" sx={{ mx: "1.75rem", mt: 2 }}>
          This deployment allows a task to target any repo: an empty allowlist
          is what means unrestricted. Adding the first repo here restricts it to
          that one repo rather than widening anything, and a task filed against
          any other repo is parked instead of dispatched until that repo is
          added too.
        </Alert>
      )}
      {targetRepos.length === 1 && (
        <Alert severity="info" sx={{ mx: "1.75rem", mt: 2 }}>
          This deployment allows only {targetRepos[0]}. A task filed against any
          other repo is parked instead of dispatched until that repo is added
          here; removing this one allows any repo again.
        </Alert>
      )}
      {repos.length > 0 && (
        <ListToolbar>
          <ListSearchField
            placeholder="Search repos…"
            value={search}
            onChange={setSearch}
          />
          <ListSortSelect
            id="repo-sort"
            value={sortBy}
            onChange={setSortBy}
            options={SORTS}
          />
        </ListToolbar>
      )}
      <ReorderableList
        className="repo-list"
        items={visible}
        idOf={(r) => r.repo}
        nameOf={(r) => r.repo}
        noun="repo"
        reorder={sortBy === "custom" ? move : null}
      >
        {(r, { handle, dragging }) => {
          // A row with no tasks, off the allowlist, is on this page only
          // because the repo carries configuration of its own -- a
          // default capability set, standing instructions, or both
          // (repoRows' third source). It has no counts to show, so
          // without saying so it would read as an empty row nobody asked
          // for -- and its page is the only way to reach what put it
          // here.
          const defaultsOnly = r.defaults && !r.configured && r.total === 0;
          const activity = repoActivity(r);
          return (
            <div
              className={`repo-list-row${dragging ? " task-row-dragging" : ""}`}
              onClick={() => onOpenRepo(r.repo)}
            >
              {handle}
              {/* The row's own status, ahead of the name, where a task
                  row carries its state badge -- so the two lists' rows
                  start the same way and a repo with something happening
                  in it is visible without reading its counts
                  (grain/task-327). The same .badge figure the count
                  chips after it use, in the icon-only column width
                  .badge-icon fixes, and the same argument as those
                  chips for why "running" is the plain CSS spin rather
                  than StateDot's grain mark: this is an aggregate, not
                  one task's live status. */}
              <span
                className={`badge badge-icon badge-${activity.state} repo-status repo-status-${activity.key}`}
                title={activity.title}
                aria-label={activity.title}
                role="img"
              />
              <span className="repo-list-name">{r.repo}</span>
              <span className="chips">
                {/* Each chip here counts a repo's tasks in one state, not one
                      task's own status, so "running" keeps the plain CSS spin
                      (style.css's .badge-running) rather than StateDot's grain
                      mark (bwsalmon/agents#586) -- there is no single task for
                      the mark to represent. */}
                {STATE_ORDER.filter((s) => r.counts[s]).map((s) => (
                  <Chip
                    key={s}
                    size="small"
                    className={`badge badge-${s}`}
                    label={`${STATE_LABELS[s]} ${r.counts[s]}`}
                  />
                ))}
                {r.blocked > 0 && (
                  <Chip
                    size="small"
                    color="error"
                    label={`Blocked ${r.blocked}`}
                  />
                )}
                {defaultsOnly && (
                  <Chip
                    size="small"
                    variant="outlined"
                    label="Defaults only"
                    title="No tasks, and not on this deployment's target repos -- listed here because it has configuration of its own, which its own page edits."
                  />
                )}
              </span>
              <Typography
                variant="caption"
                color="text.secondary"
                whiteSpace="nowrap"
              >
                {r.total} task{r.total === 1 ? "" : "s"}
              </Typography>
            </div>
          );
        }}
      </ReorderableList>
      {repos.length === 0 && (
        <ListEmpty>
          No repos yet -- add one above, or file a task with a target repo.
        </ListEmpty>
      )}
      {repos.length > 0 && visible.length === 0 && (
        <ListEmpty>No repos match your search.</ListEmpty>
      )}
    </main>
  );
}
