package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// An orphaned pull request is one grain has stopped watching: its task
// was closed while the pull request was still open, so nothing here will
// ever merge it, sync it or read it again.
//
// That is a deliberate outcome, not a bug -- nobody wants a closed task's
// work merged, nothing in grain closes a pull request, and the commits on
// the branch are real work whichever way the human went on the task. What
// was missing was anybody being *told*. The pull request sits open on
// GitHub with no sign that grain has looked away, and the task it belongs
// to is closed, so nobody is reading there either; a human who closes a
// task does not necessarily know they have just orphaned a pull request.
//
// The note below is that telling, and it is said in both places, because
// they are two different readers rather than the same one twice: on the
// task, for whoever just closed it, and on the pull request itself, for
// whoever finds it open weeks later and has no reason to look at a closed
// task at all. Store.NoteOrphanedPullRequest writes the task's copy;
// posting the other copy needs a GitHub client, so it belongs to the
// callers that have one (pkg/orchestrator on the finish path,
// cmd/grain/daemon.go's adapter for a close arriving over the UI/API).

// OrphanedPullRequestNote is what grain says about a pull request it has
// stopped watching, in full.
//
// echo carries what happened to the copy posted on the pull request
// itself: nil means it landed, non-nil means it did not and says why.
// Folding that into the words rather than dropping it is what keeps a
// failed GitHub call from costing a close its record -- the task's own
// copy is written last, after the attempt, and a deployment whose UI is
// not wired to a GitHub client at all is one legitimate reason for a
// non-nil echo, not an error worth failing a close over.
//
// Deliberately not a template with the two halves apart: the note posted
// on the pull request is this same text without the echo line, so a
// reader who lands on either copy reads the same explanation.
func OrphanedPullRequestNote(taskID string, ref PullRequestRef, echo error) string {
	var b strings.Builder
	b.WriteString(orphanedNoteLead(ref))
	fmt.Fprintf(&b, "\n\nTask %s is closed, and grain only ever merges a pull request "+
		"belonging to a task that completed -- so it will not merge this one, will not "+
		"update it, and will not look at it again.\n\n"+
		"Nothing has been done to the branch: the commits are real work, and grain never "+
		"closes a pull request. %s is still open for a human to merge or close by hand, "+
		"and reopening task %s is what puts it back under grain's watch.",
		taskID, ref, taskID)
	if echo != nil {
		fmt.Fprintf(&b, "\n\n(grain could not leave this note on the pull request itself: %v)", echo)
	} else {
		b.WriteString("\n\n(grain has left this note on the pull request itself too.)")
	}
	return b.String()
}

// OrphanedPullRequestComment is the copy posted on the pull request --
// the same words, minus the line about posting it there.
func OrphanedPullRequestComment(taskID string, ref PullRequestRef) string {
	body := OrphanedPullRequestNote(taskID, ref, nil)
	return strings.TrimSuffix(body, "\n\n(grain has left this note on the pull request itself too.)")
}

// orphanedNoteLead is the note's own first line, and the key both store
// methods below dedupe on. It names the pull request and nothing that can
// vary between two writes of the same note (no timestamps, no echo
// outcome), so "has this already been said about this pull request?" is a
// prefix comparison rather than a whole-body match -- see
// OrphanedPullRequestNoted.
func orphanedNoteLead(ref PullRequestRef) string {
	return "grain has stopped watching " + ref.String() + "."
}

// OrphanedPullRequestNoted reports whether this note has already been
// left on taskID for ref.
//
// Its purpose is not only to keep the conversation free of duplicates: it
// is also what stops a second copy going onto the pull request on GitHub,
// which is why callers ask this *before* posting there rather than
// relying on NoteOrphanedPullRequest's own write-time check. A task can
// be closed, reopened and closed again, and each of those closes runs the
// same path.
func (s *Store) OrphanedPullRequestNoted(ctx context.Context, taskID string, ref PullRequestRef) (bool, error) {
	noted, err := orphanedNoteExists(ctx, s.db.QueryRowContext, taskID, ref)
	if err != nil {
		return false, fmt.Errorf("reading whether task %s was told about %s: %w", taskID, ref, err)
	}
	return noted, nil
}

// NoteOrphanedPullRequest leaves body on taskID as grain's own comment,
// unless the same note is already there.
//
// The check is repeated inside the write transaction rather than trusted
// from OrphanedPullRequestNoted above: two closes racing each other (a
// human's, and a run's finish path discovering the same close) would
// otherwise both read "not noted" and both write.
//
// Attributed to grain itself, with no OnBehalfOf -- unlike
// orchestrator.relayComment, whose words are an agent's and are only
// carried by grain. These words are grain's own.
func (s *Store) NoteOrphanedPullRequest(ctx context.Context, taskID string,
	ref PullRequestRef, body string, now time.Time) error {

	return s.write(ctx, "note the orphaned pull request on task "+taskID, func(tx *sql.Tx) error {
		noted, err := orphanedNoteExists(ctx, tx.QueryRowContext, taskID, ref)
		if err != nil {
			return fmt.Errorf("reading whether task %s was told about %s: %w", taskID, ref, err)
		}
		if noted {
			return nil
		}
		_, err = addComment(ctx, tx, Comment{
			TaskID:    taskID,
			Author:    Attribution{Actor: Principal{Kind: PrincipalAutomation, ID: "grain"}},
			Body:      body,
			CreatedAt: now,
		})
		return err
	})
}

// orphanedNoteExists is the prefix comparison both of the above make,
// against the database handle or the transaction the caller is already
// holding.
//
// substr rather than LIKE: a GitHub repo name can contain '_', which LIKE
// would read as a wildcard.
func orphanedNoteExists(ctx context.Context,
	queryRow func(context.Context, string, ...any) *sql.Row,
	taskID string, ref PullRequestRef) (bool, error) {

	lead := orphanedNoteLead(ref)
	var one int
	err := queryRow(ctx, "SELECT 1 FROM `task_comment` WHERE `task_id` = ? "+
		"AND substr(`body`, 1, ?) = ? LIMIT 1", taskID, len(lead), lead).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
