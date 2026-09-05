package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// `grain list` asks the same question the UI's own task toolbar does
// (ui/src/components/TaskList.jsx): which of the deployment's tasks, in
// what order. The vocabulary is deliberately the same on both sides --
// the same attributes, the same names for the ways a task can have been
// filed -- so "the scheduled ones, oldest first" means one thing whether
// it is asked with a pair of menus or with `-origin schedule -sort
// oldest`.
//
// It narrows here rather than on the server because GET /api/tasks
// answers with the whole task list either way, and the UI narrows that
// same answer in the browser. A query parameter would move this loop
// across the wire without removing it from the frontend, and would leave
// two places to look for what a word like "origin" covers.

// taskStates is every state a task can be in, in the order the UI's own
// sidebar and `-sort state` put them (ui/src/state.js's STATE_ORDER):
// roughly a task's life from proposal to close. It is also what -state
// is checked against, so a typo is answered with the list rather than
// with an empty listing that looks like a deployment with no such tasks.
var taskStates = []model.State{
	model.StateProposed,
	model.StateQueued,
	model.StateRunning,
	model.StateAwaitingReply,
	model.StateFailed,
	model.StateAwaitingSubmit,
	model.StateCompleted,
	model.StateDeferred,
	model.StateClosed,
}

// taskOrigins is every way a task can come to exist, and what -origin
// calls each -- the same set the UI's own Origin menu offers, read off
// the same fields. A task carrying more than one marker is reported as
// the most specific of them, which is why the order below is the order
// taskOrigin tests them in: a merge fix is always generated from another
// task, so calling it "proposed" would say the less specific of two true
// things.
var taskOrigins = []string{"review", "fix", "schedule", "suite", "proposed", "hand"}

func taskOrigin(t ui.Task) string {
	switch {
	// Review before fix: both are stacked on another task's own branch,
	// and only one of them is a repair of a red build (ui.Task.Review).
	case t.Review:
		return "review"
	case t.Stacked:
		return "fix"
	case t.Scheduled:
		return "schedule"
	case t.SuiteRun:
		return "suite"
	case t.GeneratedFrom != "":
		return "proposed"
	}
	return "hand"
}

// noneValue is what -repo/-base/-capability take to mean "the tasks that
// have none at all" -- the UI's own "No repo"/"Repo default"/"No
// capabilities" menu entries, which a flag holding one string has no
// other way to express. A repo is owner/name and a capability id is a
// bare word from ui.OfferedCapabilities, so neither can collide with it;
// a branch literally named "none" is the one thing this spelling cannot
// ask about.
const noneValue = "none"

// taskQuery is one invocation's worth of narrowing and ordering.
//
// The three tri-state fields are pointers because "unset" and "false"
// are different questions: -auto-merge asks for the tasks that merge
// themselves, -auto-merge=false for the ones that need a human, and
// leaving it off asks nothing at all. cmdList fills them in from
// FlagSet.Visit, which is the only thing that can tell a flag left at
// its default from one explicitly set to it.
type taskQuery struct {
	state      string
	repo       string
	base       string
	capability string
	author     string
	origin     string
	search     string

	blocked     *bool
	autoMerge   *bool
	interactive *bool

	sortBy string
}

// taskOrders is every -sort value. "backlog" carries no comparison
// because it is the order the daemon served -- the backlog's own, which
// is the one order this command cannot compute for itself and the one a
// task's position in the queue is actually read off.
//
// A sort is stable throughout, so tasks that compare equal stay in that
// backlog order: `-sort state` is the backlog grouped by state rather
// than reshuffled inside each group.
var taskOrders = []struct {
	name string
	less func(a, b ui.Task) bool
}{
	{name: "backlog"},
	{name: "newest", less: func(a, b ui.Task) bool { return createdAfter(a, b) }},
	{name: "oldest", less: func(a, b ui.Task) bool { return createdAfter(b, a) }},
	{name: "title", less: func(a, b ui.Task) bool {
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	}},
	{name: "state", less: func(a, b ui.Task) bool { return stateRank(a.State) < stateRank(b.State) }},
	{name: "repo", less: func(a, b ui.Task) bool { return missingLast(a.Repo, b.Repo) }},
	{name: "author", less: func(a, b ui.Task) bool { return missingLast(a.Author, b.Author) }},
}

// createdAfter is "a is newer than b", with a task carrying no creation
// time -- one filed by a build that did not record one -- treated as the
// oldest thing there is rather than as the newest.
func createdAfter(a, b ui.Task) bool {
	if a.CreatedAt == nil {
		return false
	}
	if b.CreatedAt == nil {
		return true
	}
	return a.CreatedAt.After(*b.CreatedAt)
}

// stateRank is where taskStates says a state goes. A state this build
// has never heard of sorts after all of them rather than before, so a
// newer daemon's unknown state does not quietly take over the top of the
// listing.
func stateRank(s model.State) int {
	for i, known := range taskStates {
		if s == known {
			return i
		}
	}
	return len(taskStates)
}

// missingLast orders two of a task's text attributes alphabetically,
// with the tasks that have no value for it last: a run of tasks with no
// target repo at the top of `-sort repo` would bury what the order was
// picked to find.
func missingLast(a, b string) bool {
	if (a == "") != (b == "") {
		return b == ""
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

func (q taskQuery) validate() error {
	if q.state != "" && stateRank(model.State(q.state)) == len(taskStates) {
		return fmt.Errorf("unknown -state %q: one of %s", q.state, joinStates())
	}
	if q.origin != "" && !contains(taskOrigins, q.origin) {
		return fmt.Errorf("unknown -origin %q: one of %s", q.origin, strings.Join(taskOrigins, ", "))
	}
	if q.sortBy != "" {
		if _, ok := taskOrder(q.sortBy); !ok {
			return fmt.Errorf("unknown -sort %q: one of %s", q.sortBy, joinOrders())
		}
	}
	return nil
}

func (q taskQuery) match(t ui.Task) bool {
	switch {
	case q.state != "" && string(t.State) != q.state:
		return false
	case q.repo != "" && !matchesValue(q.repo, t.Repo):
		return false
	case q.base != "" && !matchesValue(q.base, t.Base):
		return false
	case q.capability != "" && !matchesSet(q.capability, t.Capabilities):
		return false
	case q.author != "" && t.Author != q.author:
		return false
	case q.origin != "" && taskOrigin(t) != q.origin:
		return false
	case q.blocked != nil && t.Blocked != *q.blocked:
		return false
	case q.autoMerge != nil && t.AutoMerge != *q.autoMerge:
		return false
	case q.interactive != nil && t.Interactive != *q.interactive:
		return false
	}
	if q.search == "" {
		return true
	}
	// Title or id, the two things the UI's own search box looks at, so
	// the same words find the same tasks in both places.
	needle := strings.ToLower(q.search)
	return strings.Contains(strings.ToLower(t.Title), needle) || strings.Contains(strings.ToLower(t.ID), needle)
}

// apply is the whole of what -state and friends do to a listing: narrow,
// then order. It returns a new slice rather than sorting the caller's.
func (q taskQuery) apply(tasks []ui.Task) ([]ui.Task, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	kept := make([]ui.Task, 0, len(tasks))
	for _, t := range tasks {
		if q.match(t) {
			kept = append(kept, t)
		}
	}
	if q.sortBy != "" {
		less, _ := taskOrder(q.sortBy)
		if less != nil {
			sort.SliceStable(kept, func(i, j int) bool { return less(kept[i], kept[j]) })
		}
	}
	return kept, nil
}

func matchesValue(want, have string) bool {
	if want == noneValue {
		return have == ""
	}
	return have == want
}

func matchesSet(want string, have []string) bool {
	if want == noneValue {
		return len(have) == 0
	}
	return contains(have, want)
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func taskOrder(name string) (func(a, b ui.Task) bool, bool) {
	for _, o := range taskOrders {
		if o.name == name {
			return o.less, true
		}
	}
	return nil, false
}

func joinStates() string {
	names := make([]string, 0, len(taskStates))
	for _, s := range taskStates {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

func joinOrders() string {
	names := make([]string, 0, len(taskOrders))
	for _, o := range taskOrders {
		names = append(names, o.name)
	}
	return strings.Join(names, ", ")
}

func cmdList(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain list", flag.ContinueOnError)
	var q taskQuery
	fs.StringVar(&q.state, "state", "", "only tasks in this state: "+joinStates())
	fs.StringVar(&q.repo, "repo", "", `only tasks targeting this repo (owner/name), or "none" for those with no target repo`)
	fs.StringVar(&q.base, "base", "", `only tasks on this base branch, or "none" for those taking the repo's default`)
	fs.StringVar(&q.capability, "capability", "", `only tasks carrying this capability id, or "none" for those carrying none`)
	fs.StringVar(&q.author, "author", "", "only tasks filed by this author")
	fs.StringVar(&q.origin, "origin", "", "only tasks filed this way: "+strings.Join(taskOrigins, ", "))
	fs.StringVar(&q.search, "search", "", "only tasks whose title or id contains this text, case-insensitively")
	fs.StringVar(&q.sortBy, "sort", "", "print in this order: "+joinOrders())
	// The three tri-state flags. Each asks for the tasks that have the
	// attribute; "=false" asks for the ones that don't; leaving it off
	// asks nothing -- which is why Visit, rather than the values
	// themselves, is what decides whether the question was asked.
	blocked := fs.Bool("blocked", false, "only tasks waiting on another task (-blocked=false for only the unblocked ones)")
	autoMerge := fs.Bool("auto-merge", false, "only tasks that merge themselves (-auto-merge=false for only those needing a human)")
	interactive := fs.Bool("interactive", false, "only live chats (-interactive=false for only background tasks)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "blocked":
			q.blocked = blocked
		case "auto-merge":
			q.autoMerge = autoMerge
		case "interactive":
			q.interactive = interactive
		}
	})
	// Checked before the request, so a misspelt -state is answered
	// straight away rather than after a round trip that was never going
	// to be printed.
	if err := q.validate(); err != nil {
		return err
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return err
	}
	kept, err := q.apply(tasks)
	if err != nil {
		return err
	}
	out.tasks(kept)
	return nil
}
