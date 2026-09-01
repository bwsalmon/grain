package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const branchColumns = "`id`,`owner`,`repo`,`name`,`status`,`created_at`,`last_error`"

func scanBranch(scan func(...any) error) (Branch, error) {
	var b Branch
	var status string
	var lastError sql.NullString
	if err := scan(&b.ID, &b.Repo.Owner, &b.Repo.Name, &b.Name, &status, &b.CreatedAt, &lastError); err != nil {
		return Branch{}, err
	}
	b.Status = BranchStatus(status)
	b.LastError = lastError.String
	return b, nil
}

// CreateBranch records a fresh request to create name on repo -- the
// issue's own "create a new branch on a repo... This should just create
// the given branch in github." It only ever writes a row, declaring the
// intent; the branches reconciler (pkg/orchestrator.SyncBranches) is what
// actually creates it on GitHub, at the repo's current default branch
// tip, and advances Status to BranchCreated.
//
// name must satisfy ValidBranchName (ErrInvalidBranchName). Unlike
// CreateRelease, there is no in-store uniqueness check against name:
// whether it collides with something already on GitHub is a fact only
// GitHub itself holds, so a collision surfaces as LastError once the
// reconciler tries and GitHub refuses it, the same way any other
// GitHub-side failure here does.
func (s *Store) CreateBranch(ctx context.Context, repo RepoRef, name string, now time.Time) (Branch, error) {
	if !ValidBranchName(name) {
		return Branch{}, ErrInvalidBranchName
	}
	var out Branch
	err := s.write(ctx, "create branch "+name+" for "+repo.String(), func(tx *sql.Tx) error {
		b := Branch{Repo: repo, Name: name, Status: BranchPending, CreatedAt: now}
		id, err := insertBranch(ctx, tx, b)
		if err != nil {
			return err
		}
		b.ID = id
		out = b
		return nil
	})
	return out, err
}

func insertBranch(ctx context.Context, tx *sql.Tx, b Branch) (int64, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `branch` (`owner`,`repo`,`name`,`status`,`created_at`,`last_error`) VALUES (?,?,?,?,?,?)",
		b.Repo.Owner, b.Repo.Name, b.Name, string(b.Status), b.CreatedAt.UTC(), nullable(b.LastError))
	if err != nil {
		return 0, fmt.Errorf("creating branch %q for %s: %w", b.Name, b.Repo, err)
	}
	return res.LastInsertId()
}

// ListBranches returns every branch ever requested for repo, newest
// first -- what the repo page reads to show a requested branch's own
// status and, once the reconciler has tried, any error it hit.
func (s *Store) ListBranches(ctx context.Context, repo RepoRef) ([]Branch, error) {
	var out []Branch
	err := each(ctx, s.db,
		"SELECT "+branchColumns+" FROM `branch` WHERE `owner` = ? AND `repo` = ? ORDER BY `id` DESC",
		[]any{repo.Owner, repo.Name},
		func(rows *sql.Rows) error {
			b, err := scanBranch(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, b)
			return nil
		})
	return out, err
}

// PendingBranches returns every requested branch, across every repo,
// still BranchPending -- oldest first -- what the branches reconciler
// still owes a GitHub-side create.
func (s *Store) PendingBranches(ctx context.Context) ([]Branch, error) {
	var out []Branch
	err := each(ctx, s.db,
		"SELECT "+branchColumns+" FROM `branch` WHERE `status` = ? ORDER BY `id`",
		[]any{string(BranchPending)},
		func(rows *sql.Rows) error {
			b, err := scanBranch(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, b)
			return nil
		})
	return out, err
}

// MarkBranchCreated advances a BranchPending row to BranchCreated,
// clearing LastError -- the branches reconciler's report that Name is
// live on GitHub.
func (s *Store) MarkBranchCreated(ctx context.Context, id int64) error {
	return s.write(ctx, "mark branch created", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `branch` SET `status` = ?, `last_error` = NULL WHERE `id` = ?",
			string(BranchCreated), id)
		return err
	})
}

// MarkBranchError records why the branches reconciler's create has not
// landed yet, leaving Status unchanged (BranchPending) -- the next cycle
// retries the same create.
func (s *Store) MarkBranchError(ctx context.Context, id int64, message string) error {
	return s.write(ctx, "record branch error", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE `branch` SET `last_error` = ? WHERE `id` = ?", message, id)
		return err
	})
}
