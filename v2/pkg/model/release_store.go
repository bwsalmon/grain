package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetReleaseConfig reads repo's release settings, or nil (with no error)
// if nothing has configured them yet -- the same "nil means unconfigured"
// convention GetConfig holds to for the deployment-wide row.
func (s *Store) GetReleaseConfig(ctx context.Context, repo RepoRef) (*ReleaseConfig, error) {
	return getReleaseConfig(ctx, s.db, repo)
}

const releaseConfigColumns = "`prod_branch`,`rc_branch`,`release_branch_prefix`,`major_version`"

func getReleaseConfig(ctx context.Context, q querier, repo RepoRef) (*ReleaseConfig, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+releaseConfigColumns+" FROM `release_config` WHERE `owner` = ? AND `name` = ?",
		repo.Owner, repo.Name)
	cfg := ReleaseConfig{Repo: repo}
	if err := row.Scan(&cfg.ProdBranch, &cfg.RCBranch, &cfg.ReleaseBranchPrefix, &cfg.MajorVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading release config for %s: %w", repo, err)
	}
	return &cfg, nil
}

// ListReleaseConfigs returns every repo with release settings configured,
// ordered by repo -- what a UI with no repo already in hand lists to let
// a human pick one.
func (s *Store) ListReleaseConfigs(ctx context.Context) ([]ReleaseConfig, error) {
	var out []ReleaseConfig
	err := each(ctx, s.db,
		"SELECT `owner`,`name`,"+releaseConfigColumns+" FROM `release_config` ORDER BY `owner`,`name`", nil,
		func(rows *sql.Rows) error {
			var cfg ReleaseConfig
			if err := rows.Scan(&cfg.Repo.Owner, &cfg.Repo.Name,
				&cfg.ProdBranch, &cfg.RCBranch, &cfg.ReleaseBranchPrefix, &cfg.MajorVersion); err != nil {
				return err
			}
			out = append(out, cfg)
			return nil
		})
	return out, err
}

// PutReleaseConfig replaces repo's release settings wholesale -- there is
// one row per repo, so a caller changing one field reads the current
// ReleaseConfig first the same way UpdateSettings does for the
// deployment-wide one.
func (s *Store) PutReleaseConfig(ctx context.Context, cfg ReleaseConfig) error {
	return s.write(ctx, "put release config for "+cfg.Repo.String(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"REPLACE INTO `release_config` (`owner`,`name`,"+releaseConfigColumns+") VALUES (?,?,?,?,?,?)",
			cfg.Repo.Owner, cfg.Repo.Name, cfg.ProdBranch, cfg.RCBranch, cfg.ReleaseBranchPrefix, cfg.MajorVersion)
		return err
	})
}

const candidateColumns = "`id`,`owner`,`name`,`major_version`,`number`,`version`," +
	"`branch`,`release_branch`,`status`,`created_at`,`promoted_at`,`last_error`"

func scanCandidate(scan func(...any) error) (Candidate, error) {
	var c Candidate
	var status string
	var releaseBranch, lastError sql.NullString
	var promotedAt sql.NullTime
	if err := scan(&c.ID, &c.Repo.Owner, &c.Repo.Name, &c.MajorVersion, &c.Number, &c.Version,
		&c.Branch, &releaseBranch, &status, &c.CreatedAt, &promotedAt, &lastError); err != nil {
		return Candidate{}, err
	}
	c.Status = CandidateStatus(status)
	c.ReleaseBranch = releaseBranch.String
	c.LastError = lastError.String
	c.PromotedAt = timePtr(promotedAt)
	return c, nil
}

// CurrentCandidate returns repo's most recently cut candidate, whatever
// its status, or nil if none has ever been cut -- what a UI reads to
// render "the current rc" (still cutting, active, promoting, or already
// promoted) alongside the button to act on it.
func (s *Store) CurrentCandidate(ctx context.Context, repo RepoRef) (*Candidate, error) {
	return currentCandidate(ctx, s.db, repo)
}

func currentCandidate(ctx context.Context, q querier, repo RepoRef) (*Candidate, error) {
	c, err := scanCandidate(q.QueryRowContext(ctx,
		"SELECT "+candidateColumns+" FROM `release_candidate` "+
			"WHERE `owner` = ? AND `name` = ? ORDER BY `id` DESC LIMIT 1",
		repo.Owner, repo.Name).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading current candidate for %s: %w", repo, err)
	}
	return &c, nil
}

// ListCandidates returns every candidate ever cut for repo, newest first
// -- a release history, not just the current one.
func (s *Store) ListCandidates(ctx context.Context, repo RepoRef) ([]Candidate, error) {
	var out []Candidate
	err := each(ctx, s.db,
		"SELECT "+candidateColumns+" FROM `release_candidate` "+
			"WHERE `owner` = ? AND `name` = ? ORDER BY `id` DESC",
		[]any{repo.Owner, repo.Name},
		func(rows *sql.Rows) error {
			c, err := scanCandidate(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, c)
			return nil
		})
	return out, err
}

// CutCandidate allocates and records a fresh release candidate for repo:
// the issue's own "create a new rc". MajorVersion comes from repo's
// current ReleaseConfig; Number is one past the highest ever allocated
// for repo (so it never repeats, even across a MajorVersion edit);
// Version is always 1, for now. Status starts at CandidateCutting -- the
// releases reconciler is what actually creates Branch on GitHub and
// advances it to CandidateActive.
//
// It is an error to cut a fresh candidate while repo already has one that
// has not been promoted (ErrCandidateActive): the issue's own "the
// current rc" is singular, and promoting is what retires one.
func (s *Store) CutCandidate(ctx context.Context, repo RepoRef, now time.Time) (Candidate, error) {
	var out Candidate
	err := s.write(ctx, "cut release candidate for "+repo.String(), func(tx *sql.Tx) error {
		cfg, err := getReleaseConfig(ctx, tx, repo)
		if err != nil {
			return err
		}
		if cfg == nil {
			return ErrNoReleaseConfig
		}
		current, err := currentCandidate(ctx, tx, repo)
		if err != nil {
			return err
		}
		if current != nil && current.Status != CandidatePromoted {
			return ErrCandidateActive
		}
		number := 1
		if current != nil {
			number = current.Number + 1
		}
		c := Candidate{
			Repo: repo, MajorVersion: cfg.MajorVersion, Number: number, Version: 1,
			Status: CandidateCutting, CreatedAt: now,
		}
		c.Branch = cfg.ReleaseBranchPrefix + CandidateLabel(c.MajorVersion, c.Number, c.Version)
		id, err := insertCandidate(ctx, tx, c)
		if err != nil {
			return err
		}
		c.ID = id
		out = c
		return nil
	})
	return out, err
}

func insertCandidate(ctx context.Context, tx *sql.Tx, c Candidate) (int64, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `release_candidate` "+
			"(`owner`,`name`,`major_version`,`number`,`version`,`branch`,`release_branch`,`status`,`created_at`,`promoted_at`,`last_error`) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		c.Repo.Owner, c.Repo.Name, c.MajorVersion, c.Number, c.Version,
		c.Branch, nullable(c.ReleaseBranch), string(c.Status), c.CreatedAt.UTC(), timeOf(c.PromotedAt), nullable(c.LastError))
	if err != nil {
		return 0, fmt.Errorf("cutting a release candidate for %s: %w", c.Repo, err)
	}
	return res.LastInsertId()
}

// PromoteCandidate requests promotion of repo's current candidate to its
// ReleaseConfig's ProdBranch -- the issue's own "promote the current rc".
// The candidate must already be CandidateActive (its own branch cut and
// live on GitHub); PromoteCandidate only records ReleaseBranch (named
// from ReleaseConfig's own prefix) and advances Status to
// CandidatePromoting, the same declarative-intent handoff CutCandidate
// makes to the releases reconciler -- which is what actually moves
// ProdBranch and creates ReleaseBranch on GitHub, then advances Status to
// CandidatePromoted.
//
// ErrNoCandidate: repo has never had one cut. ErrCandidateNotReady: the
// current one is still cutting, or already mid-promotion. ErrAlreadyPromoted:
// the issue's own "it cannot be promoted twice."
func (s *Store) PromoteCandidate(ctx context.Context, repo RepoRef) (Candidate, error) {
	var out Candidate
	err := s.write(ctx, "promote release candidate for "+repo.String(), func(tx *sql.Tx) error {
		cfg, err := getReleaseConfig(ctx, tx, repo)
		if err != nil {
			return err
		}
		if cfg == nil {
			return ErrNoReleaseConfig
		}
		current, err := currentCandidate(ctx, tx, repo)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrNoCandidate
		}
		switch current.Status {
		case CandidatePromoted:
			return ErrAlreadyPromoted
		case CandidateCutting, CandidatePromoting:
			return ErrCandidateNotReady
		}
		current.ReleaseBranch = cfg.ReleaseBranchPrefix + ReleaseLabel(current.MajorVersion, current.Number)
		current.Status = CandidatePromoting
		if err := updateCandidate(ctx, tx, *current); err != nil {
			return err
		}
		out = *current
		return nil
	})
	return out, err
}

func updateCandidate(ctx context.Context, tx *sql.Tx, c Candidate) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE `release_candidate` SET `release_branch` = ?, `status` = ?, `promoted_at` = ?, `last_error` = ? "+
			"WHERE `id` = ?",
		nullable(c.ReleaseBranch), string(c.Status), timeOf(c.PromotedAt), nullable(c.LastError), c.ID)
	if err != nil {
		return fmt.Errorf("updating release candidate %d: %w", c.ID, err)
	}
	return nil
}

// PendingCandidates returns every candidate, across every repo, whose
// status names a GitHub-side effect the releases reconciler still owes
// it -- CandidateCutting or CandidatePromoting -- oldest first, so a
// reconciler working through a backlog clears the longest-waiting one
// first.
func (s *Store) PendingCandidates(ctx context.Context) ([]Candidate, error) {
	var out []Candidate
	err := each(ctx, s.db,
		"SELECT "+candidateColumns+" FROM `release_candidate` WHERE `status` IN (?,?) ORDER BY `id`",
		[]any{string(CandidateCutting), string(CandidatePromoting)},
		func(rows *sql.Rows) error {
			c, err := scanCandidate(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, c)
			return nil
		})
	return out, err
}

// MarkCandidateCut advances a CandidateCutting row to CandidateActive,
// clearing LastError -- the releases reconciler's report that the
// candidate's own branch (and the repo's moving rc branch) are live on
// GitHub.
func (s *Store) MarkCandidateCut(ctx context.Context, id int64) error {
	return s.write(ctx, "mark release candidate cut", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `release_candidate` SET `status` = ?, `last_error` = NULL WHERE `id` = ?",
			string(CandidateActive), id)
		return err
	})
}

// MarkCandidatePromoted advances a CandidatePromoting row to
// CandidatePromoted, stamping PromotedAt and clearing LastError -- the
// releases reconciler's report that ProdBranch and ReleaseBranch both
// landed on GitHub. This is the retirement the issue's own "it cannot be
// promoted twice" depends on: PromoteCandidate refuses every status but
// CandidateActive, so a candidate that reaches this can never be promoted
// again.
func (s *Store) MarkCandidatePromoted(ctx context.Context, id int64, now time.Time) error {
	return s.write(ctx, "mark release candidate promoted", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `release_candidate` SET `status` = ?, `promoted_at` = ?, `last_error` = NULL WHERE `id` = ?",
			string(CandidatePromoted), now.UTC(), id)
		return err
	})
}

// MarkCandidateError records why the releases reconciler's current step
// for a candidate has not landed yet, leaving Status unchanged -- the
// next cycle retries the same step, the same level-triggered discipline
// every other reconciler in pkg/orchestrator holds to.
func (s *Store) MarkCandidateError(ctx context.Context, id int64, message string) error {
	return s.write(ctx, "record release candidate error", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `release_candidate` SET `last_error` = ? WHERE `id` = ?", message, id)
		return err
	})
}
