package model_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
)

func orphanTask(t *testing.T, store *model.Store, ctx context.Context, id string) {
	t.Helper()
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "t",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Binding:  model.BindingDirective,
		Approval: &model.Attribution{Actor: human},
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task: %v", err)
	}
}

func orphanNotes(t *testing.T, store *model.Store, ctx context.Context, id string) []string {
	t.Helper()
	comments, err := store.Comments(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range comments {
		out = append(out, c.Body)
	}
	return out
}

// The note is said once per pull request, however many times the close
// that provokes it is discovered -- by the human who closed the task, by
// the run finishing underneath them, or by both at once (see
// ui.Client.noteOrphanedPullRequests and orchestrator's own copy). The
// second write is dropped even though its body differs, because what
// varies between two writes is only how the copy on GitHub went.
func TestNoteOrphanedPullRequestSaysItOncePerPullRequest(t *testing.T) {
	store, ctx := open(t)
	orphanTask(t, store, ctx, "t1")
	ref := model.PullRequestRef{Repo: model.RepoRef{Owner: "acme", Name: "widgets"}, Number: 3}

	if noted, err := store.OrphanedPullRequestNoted(ctx, "t1", ref); err != nil || noted {
		t.Fatalf("noted = %v (%v), want false before anything is said", noted, err)
	}
	first := model.OrphanedPullRequestNote("t1", ref, nil)
	if err := store.NoteOrphanedPullRequest(ctx, "t1", ref, first, now); err != nil {
		t.Fatal(err)
	}
	if noted, err := store.OrphanedPullRequestNoted(ctx, "t1", ref); err != nil || !noted {
		t.Fatalf("noted = %v (%v), want true once it has been said", noted, err)
	}

	second := model.OrphanedPullRequestNote("t1", ref, errors.New("403 from GitHub"))
	if err := store.NoteOrphanedPullRequest(ctx, "t1", ref, second, now); err != nil {
		t.Fatal(err)
	}
	notes := orphanNotes(t, store, ctx, "t1")
	if len(notes) != 1 || notes[0] != first {
		t.Fatalf("comments = %+v, want only the first note", notes)
	}
}

// Two pull requests on one task are two different things to say. A task
// can carry more than one fixes-link -- a run that opened its own pull
// request against a base that later moved, a link written by hand -- and
// each names a pull request nobody is going to merge.
func TestNoteOrphanedPullRequestSpeaksForEachPullRequest(t *testing.T) {
	store, ctx := open(t)
	orphanTask(t, store, ctx, "t1")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	first := model.PullRequestRef{Repo: repo, Number: 3}
	second := model.PullRequestRef{Repo: repo, Number: 4}

	for _, ref := range []model.PullRequestRef{first, second} {
		if err := store.NoteOrphanedPullRequest(ctx, "t1", ref,
			model.OrphanedPullRequestNote("t1", ref, nil), now); err != nil {
			t.Fatal(err)
		}
	}
	notes := orphanNotes(t, store, ctx, "t1")
	if len(notes) != 2 {
		t.Fatalf("comments = %+v, want one per pull request", notes)
	}
	if !strings.Contains(notes[0], first.String()) || !strings.Contains(notes[1], second.String()) {
		t.Fatalf("comments = %+v, want each to name its own pull request", notes)
	}
}

// '_' is a legal character in a GitHub repo name and a single-character
// wildcard in SQL's LIKE, so a dedupe written with LIKE would read a note
// about acme/wid_ets#3 as covering acme/widgets#3 and say nothing about
// the second pull request at all. The comparison is substr for exactly
// this reason.
func TestNoteOrphanedPullRequestTreatsUnderscoresAsLiteral(t *testing.T) {
	store, ctx := open(t)
	orphanTask(t, store, ctx, "t1")
	underscored := model.PullRequestRef{Repo: model.RepoRef{Owner: "acme", Name: "wid_ets"}, Number: 3}
	plain := model.PullRequestRef{Repo: model.RepoRef{Owner: "acme", Name: "widgets"}, Number: 3}

	if err := store.NoteOrphanedPullRequest(ctx, "t1", underscored,
		model.OrphanedPullRequestNote("t1", underscored, nil), now); err != nil {
		t.Fatal(err)
	}
	noted, err := store.OrphanedPullRequestNoted(ctx, "t1", plain)
	if err != nil {
		t.Fatal(err)
	}
	if noted {
		t.Fatalf("a note about %s must not cover %s", underscored, plain)
	}
}

// What the two copies say. The one left on the pull request is the same
// explanation as the one on the task, minus the line about having left it
// there -- which is the only part that would read as nonsense on the pull
// request itself.
func TestOrphanedPullRequestNoteSaysWhatHappenedToTheOtherCopy(t *testing.T) {
	ref := model.PullRequestRef{Repo: model.RepoRef{Owner: "acme", Name: "widgets"}, Number: 3}

	landed := model.OrphanedPullRequestNote("t1", ref, nil)
	if !strings.HasPrefix(landed, "grain has stopped watching acme/widgets#3.") {
		t.Fatalf("note = %q, want it to open by naming the pull request", landed)
	}
	if !strings.Contains(landed, "task t1") && !strings.Contains(landed, "Task t1") {
		t.Fatalf("note = %q, want it to name the task", landed)
	}
	if !strings.Contains(landed, "left this note on the pull request itself too") {
		t.Fatalf("note = %q, want it to say the pull request was told", landed)
	}

	failed := model.OrphanedPullRequestNote("t1", ref, errors.New("403 from GitHub"))
	if !strings.Contains(failed, "could not leave this note on the pull request itself: 403 from GitHub") {
		t.Fatalf("note = %q, want it to say why the pull request was not told", failed)
	}

	posted := model.OrphanedPullRequestComment("t1", ref)
	if strings.Contains(posted, "pull request itself") {
		t.Fatalf("comment = %q, want it without the line about the other copy", posted)
	}
	if !strings.HasPrefix(posted, "grain has stopped watching acme/widgets#3.") {
		t.Fatalf("comment = %q, want the same explanation", posted)
	}
}
