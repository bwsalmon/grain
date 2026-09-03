package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetRepoConfig reads repo's own configuration, or nil (with no error)
// when the repo has none -- the same "nothing has configured one yet"
// nil GetQualificationPlan returns, and the same reading
// ui.(*Client).defaultCapabilities takes: a repo with no row adds
// nothing to what the deployment already defaults.
func (s *Store) GetRepoConfig(ctx context.Context, repo RepoRef) (*RepoConfig, error) {
	var defaultCapabilities string
	err := s.db.QueryRowContext(ctx,
		"SELECT `default_capabilities` FROM `repo_config` WHERE `owner` = ? AND `name` = ?",
		repo.Owner, repo.Name).Scan(&defaultCapabilities)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repo config for %s: %w", repo, err)
	}
	return &RepoConfig{Repo: repo, DefaultCapabilities: splitCSV(defaultCapabilities)}, nil
}

// ListRepoConfigs returns every repo that has a configuration of its
// own, ordered by repo so a listing is stable rather than dependent on
// insertion order. Every entry is non-Empty: PutRepoConfig deletes a row
// that would say nothing, so this is exactly the set of repos that add
// something to the deployment-wide answer -- which is what
// ui.capabilityStatuses reports as CapabilityStatus.DefaultRepos and
// what GET /api/config hands the new-task form.
func (s *Store) ListRepoConfigs(ctx context.Context) ([]RepoConfig, error) {
	var out []RepoConfig
	err := each(ctx, s.db,
		"SELECT `owner`,`name`,`default_capabilities` FROM `repo_config` ORDER BY `owner`,`name`",
		nil,
		func(rows *sql.Rows) error {
			var c RepoConfig
			var defaultCapabilities string
			if err := rows.Scan(&c.Repo.Owner, &c.Repo.Name, &defaultCapabilities); err != nil {
				return err
			}
			c.DefaultCapabilities = splitCSV(defaultCapabilities)
			out = append(out, c)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing repo configs: %w", err)
	}
	return out, nil
}

// PutRepoConfig replaces repo's own configuration wholesale -- one row
// per repo, replaced rather than diffed, the same shape PutConfig and
// PutQualificationPlan already have.
//
// A config that says nothing (RepoConfig.Empty) deletes the row instead
// of writing an empty one, so a repo whose last default capability is
// unticked leaves no trace behind to be listed, reported, or wondered
// about later.
func (s *Store) PutRepoConfig(ctx context.Context, c RepoConfig) error {
	return s.write(ctx, "update repo config for "+c.Repo.String(), func(tx *sql.Tx) error {
		if c.Empty() {
			_, err := tx.ExecContext(ctx,
				"DELETE FROM `repo_config` WHERE `owner` = ? AND `name` = ?",
				c.Repo.Owner, c.Repo.Name)
			return err
		}
		_, err := tx.ExecContext(ctx,
			"REPLACE INTO `repo_config` (`owner`,`name`,`default_capabilities`) VALUES (?,?,?)",
			c.Repo.Owner, c.Repo.Name, joinCSV(c.DefaultCapabilities))
		return err
	})
}
