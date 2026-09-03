package staterepo

// check.go answers the one question a pull request against a state
// repository cannot answer by being read: will grain be able to load
// this?
//
// Import is already the validator -- it does the whole load in one
// transaction with foreign keys deferred and rolls the lot back on any
// inconsistency -- but until now the only thing that ever ran it was a
// daemon starting up, which is the worst possible place to find out that
// a merged change dropped a NOT NULL column or left a task_link pointing
// at a task that is no longer in the file. By then the change is merged,
// the daemon is the thing that is down, and the person who has to fix it
// is whoever is on call rather than whoever wrote the diff.
//
// So Check runs that same import against a database the caller is
// throwing away afterwards, and reports what broke in terms of the file
// and the row it broke on. It is meant to be a CI step in the state
// repository itself (`grain state check .`), where it fails the pull
// request instead of the deployment.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CheckReport is what a check found: enough to print a summary a human
// can read at the bottom of a CI log, and the warnings that are worth
// saying but not worth failing over.
type CheckReport struct {
	// SchemaVersion is the version the dump is stamped with.
	SchemaVersion int
	// Rows is how many rows each table's file loaded, for the tables that
	// had any. A table with no rows is left out rather than recorded as
	// zero: the interesting number in a CI log is what arrived.
	Rows map[string]int
	// Warnings are things a reviewer should know that do not stop the
	// dump loading -- a file naming a table this build does not have, a
	// row carrying a column it does not have. Both are what a dump from a
	// different build looks like, which is legitimate (Import skips them
	// deliberately), and both are also what a typo in a hand-edited file
	// looks like, which is not.
	Warnings []string
}

// Total is how many rows the whole dump loaded.
func (r CheckReport) Total() int {
	n := 0
	for _, count := range r.Rows {
		n += count
	}
	return n
}

// Tables names the tables that loaded rows, sorted.
func (r CheckReport) Tables() []string {
	names := make([]string, 0, len(r.Rows))
	for name := range r.Rows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Check loads dir into db and reports whether it worked.
//
// db must be an empty, throwaway database at this build's own schema:
// Import replaces every row in whatever it is given, so handing it the
// database a deployment is running on would be a check that costs the
// deployment its state. The caller opens it because opening a database is
// pkg/model/sqlite's business and this package deliberately depends on no
// driver (see that package's own doc comment).
//
// version is the schema this build knows -- model.SchemaVersion at every
// real call site. A dump stamped with anything else is refused here for
// the same reason Load refuses it: the rows are shaped for a schema this
// binary does not have, so "it imported" would not mean what a reader of
// a green check would take it to mean.
func Check(ctx context.Context, db *sql.DB, dir string, version int) (CheckReport, error) {
	report := CheckReport{Rows: map[string]int{}}
	// One connection, with foreign keys on it, so that the import runs
	// under the constraints a daemon's would. pkg/model/sqlite enables
	// the pragma with a single Exec, which reaches one pooled connection
	// rather than every connection database/sql may later open -- so a
	// check that happened to import on another one would be quietly
	// laxer than the load it stands in for. The schema declares no
	// foreign keys today (Import defers them anyway, for the day it
	// does), which is precisely why this is worth pinning here rather
	// than left to be noticed when the first one is added.
	//
	// Mutating the caller's *sql.DB is fair here for the reason the doc
	// comment above gives: it is a database the caller is throwing away.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return report, fmt.Errorf("staterepo: enabling foreign keys for the check: %w", err)
	}
	if !HasDump(dir) {
		return report, fmt.Errorf("staterepo: %s holds no dump: there is no %s/ directory with table files in it",
			dir, TablesDir)
	}
	found, err := ReadSchemaVersion(dir)
	if err != nil {
		return report, err
	}
	report.SchemaVersion = found
	switch {
	case found > version:
		return report, fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooNew, found, version)
	case found < version:
		return report, fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooOld, found, version)
	}
	tables, err := tableNames(ctx, db)
	if err != nil {
		return report, err
	}
	warnings, err := describeDump(ctx, db, dir, tables)
	if err != nil {
		return report, err
	}
	report.Warnings = warnings

	if err := Import(ctx, db, dir); err != nil {
		return report, err
	}
	for _, t := range tables {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(t)).Scan(&n); err != nil {
			return report, fmt.Errorf("staterepo: counting %s: %w", t, err)
		}
		if n > 0 {
			report.Rows[t] = n
		}
	}
	return report, nil
}

// describeDump reads the dump the way Import will and says what Import
// would silently pass over. Import is right to skip a file or a column
// this build does not have -- that is a dump written by a build one
// version along, and refusing it would make every upgrade a flag day --
// but a check that said nothing about it would let a misspelt table name
// through as a change that "loads fine" and does nothing at all.
func describeDump(ctx context.Context, db *sql.DB, dir string, tables []string) ([]string, error) {
	known := map[string]bool{}
	for _, t := range tables {
		known[t] = true
	}
	entries, err := os.ReadDir(filepath.Join(dir, TablesDir))
	if err != nil {
		return nil, fmt.Errorf("staterepo: reading %s: %w", filepath.Join(dir, TablesDir), err)
	}
	var warnings []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		table := strings.TrimSuffix(e.Name(), ".json")
		path := filepath.Join(TablesDir, e.Name())
		if !known[table] {
			warnings = append(warnings, fmt.Sprintf(
				"%s names no table this build has; every row in it would be ignored", path))
			continue
		}
		cols, err := columns(ctx, db, table)
		if err != nil {
			return nil, err
		}
		hasColumn := map[string]bool{}
		for _, c := range cols {
			hasColumn[c.name] = true
		}
		rows, err := readRows(filepath.Join(dir, TablesDir, e.Name()))
		if err != nil {
			return nil, err
		}
		// Reported once per column rather than once per row: a column
		// name that is wrong is wrong in every row of the file, and a
		// warning per row would bury the one line that matters.
		seen := map[string]bool{}
		for i, row := range rows {
			for name := range row {
				if hasColumn[name] || seen[name] {
					continue
				}
				seen[name] = true
				warnings = append(warnings, fmt.Sprintf(
					"%s row %d has a column %q that %s does not; it would be ignored", path, i, name, table))
			}
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

// readRows parses one table file, with the same decoder Import uses so
// that a file this rejects is exactly a file Import would reject.
func readRows(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("staterepo: reading %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("staterepo: parsing %s: %w", path, err)
	}
	return rows, nil
}
