package staterepo

// Every table this package names as a string, checked against the tables
// the schema actually creates.
//
// The lists here -- SettingsTables, churnTables, the names the README
// spells out for whoever opens the repository -- are []string and
// map[string]bool, so the compiler has nothing to say about them, and
// what goes wrong when one drifts is silence rather than an error.
// bwsalmon/grain#672 is the worked example: a branch renamed
// task_template to template while another added SettingsTables naming
// the old one, they merged cleanly, and every template and suite dropped
// out of what Apply imports -- Import skips a table this build does not
// have on purpose, because that is what makes a dump from a newer build
// importable at all. A rename is a normal thing to do to this schema and
// two branches merging is a normal way to do it, so the check belongs
// here rather than in a reviewer's head.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

// openSchema is a freshly Init-ed database: the schema this build
// creates, which is the only authority on what a table name means.
func openSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := model.New(db).Init(context.Background()); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return db
}

// schemaTableSet is what tableNames lists -- the same query Export and
// the whole-database Import walk -- as a set.
func schemaTableSet(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	names, err := tableNames(context.Background(), db)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	if len(set) == 0 {
		t.Fatal("the schema has no tables, so nothing below is checking anything")
	}
	return set
}

// Every name on SettingsTables has to be a table this build actually
// has, or Apply silently imports nothing from a file it was the whole
// point of that list to import. A rename in pkg/model is exactly how
// that goes wrong, and nothing else notices: Import skips a table it
// does not have on purpose, so the daemon comes up, applies nothing,
// and says nothing.
func TestSettingsTablesAllExist(t *testing.T) {
	have := schemaTableSet(t, openSchema(t))
	for _, name := range SettingsTables {
		if !have[name] {
			t.Errorf("SettingsTables names %q, which this build's schema does not have", name)
		}
	}
}

// notSettings is the other half of the same decision: every table the
// schema has that SettingsTables deliberately leaves out.
//
// It is written out rather than derived so that a table added to the
// schema fails this file until somebody says which half it is in. The
// default that costs nothing to get wrong is "not settings" -- a table
// left off SettingsTables is still exported, still imported at start,
// and merely not applied live -- and the default that loses data is the
// other one, so the decision is worth making on purpose once rather than
// inheriting from whichever list a new table happens to fall into.
//
// The reasons come from SettingsTables' own doc comment: a table belongs
// here if it is grain's record of what it did (the database is
// authoritative and a live import would delete rows written since the
// dump), or if nobody proposes a change to it in a pull request.
var notSettings = []string{
	// grain's own record of what it did. Replacing any of these
	// underneath a running reconcile loop deletes the rows that loop is
	// holding ids for.
	"task",
	"task_read",
	"task_grant",
	"task_link",
	"task_tag",
	"task_observation",
	"task_run",
	"task_run_tool",
	"task_run_check_wait",
	"task_sequence",
	"lease",
	"branch",
	// Written by people, and still not applied live: a comment or an
	// attachment is not something a pull request against the state
	// repository proposes, and clearing the tables to reinstate them
	// would race the daemon writing new ones.
	"task_comment",
	"task_attachment",
	// Runs, as against the settings they were run from. A qualification
	// plan and a suite are settings (they are on SettingsTables); a run
	// of one is grain's record of having run it.
	"qualification_run",
	"qualification_task",
	"suite_run",
	"suite_run_item",
	"suite_run_task",
	"release",
	"release_candidate",
	// The schema's own bookkeeping. Nothing proposes a change to the
	// version a database is at; the stamp in the repository
	// (SchemaVersionFile) is what a dump says about that, and importing
	// this table live would tell a running database it is at a different
	// schema than it is.
	"grain_schema",
}

// The converse of TestSettingsTablesAllExist, and the one that catches a
// table added to the schema rather than a table renamed out from under a
// list: every table has to be on exactly one of the two lists. A new
// settings-shaped table that nobody put on SettingsTables is a settings
// change an operator merges and grain never applies -- the #672 failure
// again, arrived at by addition instead of by rename.
func TestEveryTableIsEitherSettingsOrDeliberatelyNot(t *testing.T) {
	have := schemaTableSet(t, openSchema(t))
	classified := map[string]bool{}
	for _, name := range SettingsTables {
		classified[name] = true
	}
	for _, name := range notSettings {
		if classified[name] {
			t.Errorf("%q is on both SettingsTables and notSettings", name)
		}
		classified[name] = true
		if !have[name] {
			t.Errorf("notSettings names %q, which this build's schema does not have: "+
				"drop it, or fix the name it was renamed to", name)
		}
	}
	var missing []string
	for name := range have {
		if !classified[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the schema has tables neither list mentions: %s\n"+
			"Decide for each: settings a merged pull request should apply to a running "+
			"daemon go on staterepo.SettingsTables (bind.go), and everything else on "+
			"notSettings in this file.", strings.Join(missing, ", "))
	}
}

// churnTables is a list of names too, and it drifts the same way -- with
// a worse ending. A churn table whose name no longer exists is not
// merely misclassified: TierOf answers TierState for it by default, so
// Load's import of a merged settings change starts replacing task_run
// and lease from a dump that is up to a churn interval behind the
// database, which is exactly the rollback tier.go exists to prevent.
func TestChurnTablesAllExist(t *testing.T) {
	have := schemaTableSet(t, openSchema(t))
	for name := range churnTables {
		if !have[name] {
			t.Errorf("churnTables names %q, which this build's schema does not have", name)
		}
	}
}

// The whole-database path -- Export, and the Import a fresh clone does
// at startup -- takes its tables from the database rather than from a
// list here, and that is the property worth pinning: it is why nothing
// above has to enumerate the non-settings tables for those two to be
// complete. A dump with a table missing from it restores a database with
// that table empty, and nothing would say so.
func TestExportWritesAFileForEveryTableTheSchemaHas(t *testing.T) {
	db := openSchema(t)
	have := schemaTableSet(t, db)
	dir := t.TempDir()
	if err := Export(context.Background(), db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, TablesDir))
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	written := map[string]bool{}
	for _, e := range entries {
		written[strings.TrimSuffix(e.Name(), ".json")] = true
	}
	for name := range have {
		if !written[name] {
			t.Errorf("the export wrote no file for %q, so a clone of the repository restores it empty", name)
		}
	}
}

// The README is the dump's own documentation, and it names tables in
// prose -- which is a list of table names like any other, read by
// whoever opens the repository and by the agents dispatched to work in
// it. A stale name here sends somebody to edit a file nothing reads
// (#672 left `task_template` in it for exactly as long as it left it in
// SettingsTables).
func TestTheReadmeNamesTablesTheSchemaHas(t *testing.T) {
	have := schemaTableSet(t, openSchema(t))
	// Backticked words that are not tables and are not meant to be. Kept
	// short on purpose: a word in backticks in this README is a table
	// name unless it is one of these.
	notATable := map[string]bool{
		"tables":        true, // TablesDir, the directory the files are in
		"qualification": true, // the family, naming its tables collectively
	}
	for _, quoted := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(readme, -1) {
		// tables/task_run.json and task_run both mean the table; a path
		// with a placeholder in it, a filename that is not a table's and
		// anything with a space in it mean something else and are skipped
		// by the shape test below.
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(quoted[1], "/"), TablesDir+"/"), ".json")
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(name) || notATable[name] {
			continue
		}
		if !have[name] {
			t.Errorf("the README names %q as a table and this build's schema has no such table: "+
				"rename it there too, or add it to notATable in this test if it is not a table name",
				name)
		}
	}
}
