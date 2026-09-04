// taskFilters.js is the question "which tasks show up, in what order" --
// the sort orders, the attribute filters and the code that turns a set
// of tasks into the menus offering them -- kept apart from any one view
// that asks it.
//
// It lived inside TaskList.jsx until the board (TaskBoard.jsx) needed
// the same toolbar over the same tasks (grain/task-287). Two views
// asking that question two different ways would mean a repo filter that
// spells "no repo" differently on each of them, and a new attribute
// getting a menu on one view and not on the other; there is one FILTERS
// list here instead, and both views build their toolbar out of it.
//
// What is deliberately *not* here: the state filter. The list takes it
// from the sidebar as a standing question about which tasks it is
// about, and the board answers the same question with its columns
// (board.js) -- so it is each view's own, not shared.
import { STATE_ORDER, capabilityName } from "./state.js";

// byText orders on one of a task's own text attributes, with the tasks
// that have no value for it last rather than first -- "Repo (A–Z)"
// opening on a run of tasks with no target repo would bury the thing the
// order was picked to find.
//
// Nothing here breaks a tie: Array#sort is stable, so tasks that compare
// equal keep the backlog order they arrived in, which makes every sort
// below "the backlog, grouped by X" rather than a reshuffle inside each
// group.
function byText(pick) {
  return (a, b) => {
    const x = pick(a) || "";
    const y = pick(b) || "";
    if ((x === "") !== (y === "")) return x === "" ? 1 : -1;
    return x.localeCompare(y);
  };
}

// stateRank puts a task where STATE_ORDER -- the sidebar's own order,
// proposed through closed -- says it goes. A state this build has never
// heard of sorts after all of them instead of before, so a newer daemon's
// unknown state doesn't quietly take over the top of the list.
export function stateRank(t) {
  const i = STATE_ORDER.indexOf(t.state);
  return i === -1 ? STATE_ORDER.length : i;
}

// SORTS is every order a task view's own toolbar Select offers, keyed by
// what it stores in sortBy. "manual" is the backlog order the store
// itself keeps (Store.Reorder, bwsalmon/agents#476) and the only one
// dragging a row makes sense against -- picking any other one is a
// display-only reordering of the same tasks, so drag-and-drop is
// disabled outside it rather than let a drop silently reorder the
// backlog underneath a view that no longer matches it.
export const SORTS = {
  manual: { label: "Backlog order", cmp: null },
  newest: { label: "Newest first", cmp: (a, b) => new Date(b.createdAt || 0) - new Date(a.createdAt || 0) },
  oldest: { label: "Oldest first", cmp: (a, b) => new Date(a.createdAt || 0) - new Date(b.createdAt || 0) },
  title: { label: "Title (A–Z)", cmp: (a, b) => a.title.localeCompare(b.title) },
  state: { label: "State", cmp: (a, b) => stateRank(a) - stateRank(b) },
  repo: { label: "Repo (A–Z)", cmp: byText((t) => t.repo) },
  author: { label: "Author (A–Z)", cmp: byText((t) => t.author) },
};

// sortTasks is SORTS applied without copying the array when it doesn't
// have to be: "manual" is the order the store already handed over.
export function sortTasks(tasks, sortBy) {
  const cmp = SORTS[sortBy]?.cmp;
  return cmp ? [...tasks].sort(cmp) : tasks;
}

// NONE is the option value standing for "this task has no value for the
// attribute at all" -- no target repo, no capabilities. It is a string a
// real value can't be: a repo is owner/name and a capability id is a
// bare word, so neither can be "__none__", and it is never displayed
// (noneLabel is).
export const NONE = "__none__";

const ORIGIN_LABELS = {
  hand: "Filed by hand",
  proposed: "Proposed by an agent",
  schedule: "Scheduled",
  suite: "Suite run",
  fix: "Merge fix",
  review: "Review",
};

// originOf is the one thing that put this task in the list -- what the
// row's own scheduled/suite/merge-fix chips say, collapsed to a single
// value so it can be asked about as one attribute. The order matters
// where a task carries more than one marker: a merge fix is always
// generated from another task, so answering "proposed" for it would say
// the less specific of two true things.
export function originOf(t) {
  // Review before fix: both are stacked, and only one of them is a
  // repair of a red build.
  if (t.review) return "review";
  if (t.stacked) return "fix";
  if (t.scheduled) return "schedule";
  if (t.suiteRun) return "suite";
  if (t.generatedFrom) return "proposed";
  return "hand";
}

// FILTERS is every attribute a task view's toolbar can narrow by, beyond
// the states the view itself is about. Each entry says only what is
// particular to that attribute:
//
//   values    every value this task has for it -- none for a task the
//             attribute doesn't apply to, more than one for capabilities
//   labelOf   how a value is written on the menu, when it isn't the
//             value itself
//   order     the order to offer values in, for the attributes whose
//             values are a fixed vocabulary rather than whatever the
//             tasks happen to hold (the rest are offered alphabetically)
//   noneLabel what to call "has none of these", for the attributes where
//             having none of them is a real answer worth filtering on
//
// Everything else -- collecting the options, hiding a Select that could
// not narrow anything, forgetting a choice that has gone out of range,
// deciding whether a task matches -- is the same for every attribute and
// happens once, in filterViews below.
export const FILTERS = [
  {
    id: "repo",
    label: "Repo",
    anyLabel: "Any repo",
    // The write target alone, which is the same repo a repo's own page
    // scopes its copy of this list by (App.jsx): a task that only reads
    // a repo is not one of that repo's tasks.
    values: (t) => (t.repo ? [t.repo] : []),
    noneLabel: "No repo",
  },
  {
    id: "base",
    label: "Base",
    anyLabel: "Any base",
    values: (t) => (t.base ? [t.base] : []),
    noneLabel: "Repo default",
  },
  {
    id: "capability",
    label: "Capability",
    anyLabel: "Any capability",
    values: (t) => t.capabilities || [],
    labelOf: (v, config) => capabilityName(config, v),
    noneLabel: "No capabilities",
  },
  {
    id: "author",
    label: "Author",
    anyLabel: "Any author",
    values: (t) => (t.author ? [t.author] : []),
  },
  {
    id: "origin",
    label: "Origin",
    anyLabel: "Any origin",
    values: (t) => [originOf(t)],
    order: ["hand", "proposed", "schedule", "suite", "fix", "review"],
    labelOf: (v) => ORIGIN_LABELS[v],
  },
  {
    id: "kind",
    label: "Kind",
    anyLabel: "Any kind",
    values: (t) => [t.interactive ? "interactive" : "background"],
    order: ["background", "interactive"],
    labelOf: (v) => (v === "interactive" ? "Interactive" : "Background"),
  },
  {
    id: "autoMerge",
    label: "Auto-merge",
    anyLabel: "Either way",
    values: (t) => [t.autoMerge ? "on" : "off"],
    order: ["on", "off"],
    labelOf: (v) => (v === "on" ? "On" : "Off"),
  },
];

// optionsFor is every value of one attribute the given tasks actually
// carry -- so a menu never offers a choice that would come back empty.
// A fixed-vocabulary attribute keeps its own order; the rest are
// alphabetical by what the menu shows rather than by the raw value,
// since a capability's id and its name are not the same string.
export function optionsFor(f, tasks, config) {
  const seen = new Set();
  let none = false;
  for (const t of tasks) {
    const values = f.values(t);
    if (values.length === 0) none = true;
    for (const v of values) seen.add(v);
  }
  const label = (v) => (f.labelOf ? f.labelOf(v, config) : v);
  const options = f.order
    ? f.order.filter((v) => seen.has(v)).map((v) => ({ value: v, label: label(v) }))
    : [...seen].map((v) => ({ value: v, label: label(v) })).sort((a, b) => a.label.localeCompare(b.label));
  if (none && f.noneLabel) options.push({ value: NONE, label: f.noneLabel });
  return options;
}

export function filterMatches(f, t, value) {
  const values = f.values(t);
  return value === NONE ? values.length === 0 : values.includes(value);
}

// filterViews is one entry per attribute: the values there are to pick
// from right now, and which one is picked.
//
// A Select is only worth showing when it offers more than one option,
// since an attribute every task in view shares -- the repo, on a repo's
// own page -- can narrow nothing. A choice whose option has since gone
// (the sidebar moved to a state where no task carries it) reads as "any"
// again rather than as a filter that matches nothing, which also keeps
// MUI from warning about a Select whose value is not one of its items.
//
// `tasks` is what the view is about before the toolbar has its say --
// the list's state filter applied, the board's columns applied -- so a
// menu never offers a repo whose every task is already off screen.
export function filterViews(tasks, filters, config) {
  return FILTERS.map((f) => {
    const options = optionsFor(f, tasks, config);
    const shown = options.length > 1;
    const value = shown && options.some((o) => o.value === filters[f.id]) ? filters[f.id] : "";
    return { f, options, shown, value };
  });
}

// matchesFilters is whether one task survives every filter currently
// picked plus the search box -- the second half of every view's own
// `matches`, whose first half is that view's own question about state.
// The search is over the title and the id, which is what somebody
// typing "287" or "kanban" is reaching for.
export function matchesFilters(t, activeFilters, q) {
  if (!activeFilters.every(({ f, value }) => filterMatches(f, t, value))) return false;
  return q === "" || t.title.toLowerCase().includes(q) || String(t.id).toLowerCase().includes(q);
}
