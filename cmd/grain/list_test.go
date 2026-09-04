package main

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// grain/task-288: `grain list` narrows and orders on the same task
// attributes the UI's own toolbar does. These exercise taskQuery, which
// is all of that question -- cmdList around it only turns flags into one
// of these and prints what comes back.

func at(day int) *time.Time {
	t := time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC)
	return &t
}

// listing is one of everything worth asking about: two repos and a task
// with none, two authors, a scheduled task, a merge fix, a live chat, a
// blocked task, one task that merges itself.
func listing() []ui.Task {
	return []ui.Task{
		{
			ID: "1", Title: "Widget work", State: model.StateQueued, Repo: "acme/widgets", Base: "main",
			Author: "ada", Capabilities: []string{"gh"}, AutoMerge: true, CreatedAt: at(3),
		},
		{
			ID: "2", Title: "Gadget work", State: model.StateRunning, Repo: "acme/gadgets",
			Author: "grace", Capabilities: []string{"gcp"}, Scheduled: true, CreatedAt: at(1),
		},
		{
			ID: "3", Title: "Loose chat", State: model.StateProposed,
			Author: "ada", Interactive: true, CreatedAt: at(2),
		},
		{
			ID: "4", Title: "Repair the widget branch", State: model.StateQueued, Repo: "acme/widgets",
			Author: "grain", Stacked: true, GeneratedFrom: "1", Blocked: true, BlockedBy: []string{"1"},
			CreatedAt: at(4),
		},
	}
}

func ids(tasks []ui.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTaskQueryFilters(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		q    taskQuery
		want []string
	}{
		{name: "no question at all keeps everything", q: taskQuery{}, want: []string{"1", "2", "3", "4"}},
		{name: "state", q: taskQuery{state: "queued"}, want: []string{"1", "4"}},
		{name: "repo", q: taskQuery{repo: "acme/widgets"}, want: []string{"1", "4"}},
		{name: "no repo at all", q: taskQuery{repo: "none"}, want: []string{"3"}},
		{name: "base branch", q: taskQuery{base: "main"}, want: []string{"1"}},
		{name: "the repo's default base", q: taskQuery{base: "none"}, want: []string{"2", "3", "4"}},
		{name: "capability", q: taskQuery{capability: "gcp"}, want: []string{"2"}},
		{name: "no capabilities", q: taskQuery{capability: "none"}, want: []string{"3", "4"}},
		{name: "author", q: taskQuery{author: "ada"}, want: []string{"1", "3"}},
		{name: "filed by hand", q: taskQuery{origin: "hand"}, want: []string{"1", "3"}},
		{name: "filed by a schedule", q: taskQuery{origin: "schedule"}, want: []string{"2"}},
		// A merge fix is generated from another task, so it answers the
		// more specific of the two origins it could claim.
		{name: "filed by the merge queue", q: taskQuery{origin: "fix"}, want: []string{"4"}},
		{name: "nothing was proposed here", q: taskQuery{origin: "proposed"}, want: []string{}},
		{name: "blocked", q: taskQuery{blocked: &yes}, want: []string{"4"}},
		{name: "not blocked", q: taskQuery{blocked: &no}, want: []string{"1", "2", "3"}},
		{name: "merges itself", q: taskQuery{autoMerge: &yes}, want: []string{"1"}},
		{name: "needs a human", q: taskQuery{autoMerge: &no}, want: []string{"2", "3", "4"}},
		{name: "a live chat", q: taskQuery{interactive: &yes}, want: []string{"3"}},
		{name: "search matches a title, case-insensitively", q: taskQuery{search: "WIDGET"}, want: []string{"1", "4"}},
		{name: "search matches an id", q: taskQuery{search: "2"}, want: []string{"2"}},
		{name: "filters combine", q: taskQuery{repo: "acme/widgets", author: "ada"}, want: []string{"1"}},
		{name: "filters that cannot both hold keep nothing", q: taskQuery{repo: "acme/widgets", author: "grace"}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.q.apply(listing())
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !equal(ids(got), tc.want) {
				t.Fatalf("apply() = %v, want %v", ids(got), tc.want)
			}
		})
	}
}

func TestTaskQueryOrders(t *testing.T) {
	for _, tc := range []struct {
		name string
		sort string
		want []string
	}{
		// The backlog's own order is what the daemon served, so nothing
		// here reorders it.
		{name: "backlog leaves the served order alone", sort: "backlog", want: []string{"1", "2", "3", "4"}},
		{name: "unset leaves it alone too", sort: "", want: []string{"1", "2", "3", "4"}},
		{name: "newest", sort: "newest", want: []string{"4", "1", "3", "2"}},
		{name: "oldest", sort: "oldest", want: []string{"2", "3", "1", "4"}},
		{name: "title", sort: "title", want: []string{"2", "3", "4", "1"}},
		{name: "state, in the order a task lives through them", sort: "state", want: []string{"3", "1", "4", "2"}},
		// The task with no repo sorts last rather than first.
		{name: "repo, with the repo-less last", sort: "repo", want: []string{"2", "1", "4", "3"}},
		{name: "author", sort: "author", want: []string{"1", "3", "2", "4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := taskQuery{sortBy: tc.sort}.apply(listing())
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !equal(ids(got), tc.want) {
				t.Fatalf("apply(-sort %q) = %v, want %v", tc.sort, ids(got), tc.want)
			}
		})
	}
}

// Ties keep the order the daemon served, so `-sort state` is the backlog
// grouped by state rather than reshuffled inside each group.
func TestTaskQuerySortIsStable(t *testing.T) {
	got, err := taskQuery{sortBy: "state"}.apply(listing())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 1 and 4 are both queued, and 1 came first off the wire.
	if want := []string{"3", "1", "4", "2"}; !equal(ids(got), want) {
		t.Fatalf("apply(-sort state) = %v, want %v", ids(got), want)
	}
}

// grain/task-284: a review is stacked on another task's branch the way
// the merge queue's own fix is, so both would answer to "fix" if
// taskOrigin read Stacked alone. -origin has to tell them apart -- one
// is a second agent reading a change, the other is a repair of a red
// build.
func TestTaskOriginTellsAReviewFromAMergeFix(t *testing.T) {
	tasks := []ui.Task{
		{ID: "1", Title: "Widget work", State: model.StateQueued},
		{ID: "2", Title: "Repair it", State: model.StateQueued, Stacked: true, GeneratedFrom: "1"},
		{ID: "3", Title: "Review it", State: model.StateQueued, Stacked: true, Review: true, GeneratedFrom: "1"},
	}
	for _, tc := range []struct {
		origin string
		want   []string
	}{
		{origin: "fix", want: []string{"2"}},
		{origin: "review", want: []string{"3"}},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			got, err := taskQuery{origin: tc.origin}.apply(tasks)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !equal(ids(got), tc.want) {
				t.Fatalf("apply(-origin %s) = %v, want %v", tc.origin, ids(got), tc.want)
			}
		})
	}
}

// A word nobody can act on is answered with the list of words that work,
// rather than with an empty listing that reads like a deployment with no
// such tasks.
func TestTaskQueryRejectsUnknownValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    taskQuery
	}{
		{name: "state", q: taskQuery{state: "in_progress"}},
		{name: "origin", q: taskQuery{origin: "robot"}},
		{name: "sort", q: taskQuery{sortBy: "oldest-first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.q.apply(listing()); err == nil {
				t.Fatalf("apply(%+v) succeeded, want an error naming the values that work", tc.q)
			}
		})
	}
}

// A state this build has never heard of sorts after the ones it knows,
// so a newer daemon's own vocabulary cannot take over the top of a
// listing an older CLI prints.
func TestUnknownStateSortsLast(t *testing.T) {
	tasks := []ui.Task{
		{ID: "1", Title: "From the future", State: model.State("hibernating")},
		{ID: "2", Title: "Ordinary", State: model.StateCompleted},
	}
	got, err := taskQuery{sortBy: "state"}.apply(tasks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := []string{"2", "1"}; !equal(ids(got), want) {
		t.Fatalf("apply(-sort state) = %v, want %v", ids(got), want)
	}
}
