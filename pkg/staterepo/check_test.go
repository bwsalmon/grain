package staterepo_test

// Check against real dumps and a real database: what a state
// repository's CI step would run, given the files a pull request
// proposes.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// dumpOf writes a real export of a database holding one task, which is
// the shape every test here starts from and then breaks.
func dumpOf(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting a task: %v", err)
	}
	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	if err := staterepo.WriteSchemaVersion(dir, model.SchemaVersion); err != nil {
		t.Fatalf("stamping: %v", err)
	}
	return dir
}

// readTable/writeTable are how these tests play the part of somebody
// hand-editing a file in a pull request.
func readTable(t *testing.T, dir, table string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, staterepo.TablesDir, table+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func writeTable(t *testing.T, dir, table string, rows any) {
	t.Helper()
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, staterepo.TablesDir, table+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAcceptsADumpGrainWrote(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	_, db := openDB(t)

	report, err := staterepo.Check(ctx, db, dir, model.SchemaVersion)
	if err != nil {
		t.Fatalf("checking a dump grain itself wrote: %v", err)
	}
	if report.SchemaVersion != model.SchemaVersion {
		t.Errorf("report says schema %d, want %d", report.SchemaVersion, model.SchemaVersion)
	}
	if report.Rows["task"] != 1 {
		t.Errorf("report counted %d tasks, want 1 (rows: %v)", report.Rows["task"], report.Rows)
	}
	if report.Total() < 1 {
		t.Errorf("report totals %d rows", report.Total())
	}
	if len(report.Warnings) != 0 {
		t.Errorf("grain's own dump warned: %v", report.Warnings)
	}
}

// The case this whole command exists for: a row that lost a column the
// schema insists on. Before `grain state check`, this failed on the next
// daemon start, after the merge.
func TestCheckRejectsARowMissingARequiredColumn(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	rows := readTable(t, dir, "task")
	delete(rows[0], "title")
	writeTable(t, dir, "task", rows)
	_, db := openDB(t)

	_, err := staterepo.Check(ctx, db, dir, model.SchemaVersion)
	if err == nil {
		t.Fatal("a task row with no title passed the check")
	}
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("error does not name the table: %v", err)
	}
}

// A hand-edited file that duplicates a row -- the shape of a copy-paste
// that forgot to change the id -- is refused by the primary key, which
// is exactly the sort of thing nobody sees by reading the diff.
func TestCheckRejectsTwoRowsWithTheSameKey(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	rows := readTable(t, dir, "task")
	writeTable(t, dir, "task", append(rows, rows[0]))
	_, db := openDB(t)

	if _, err := staterepo.Check(ctx, db, dir, model.SchemaVersion); err == nil {
		t.Fatal("two task rows with the same id passed the check")
	}
}

func TestCheckRejectsAMalformedFile(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	path := filepath.Join(dir, staterepo.TablesDir, "task.json")
	if err := os.WriteFile(path, []byte("[{\"id\": \"a1b2\",,}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, db := openDB(t)

	_, err := staterepo.Check(ctx, db, dir, model.SchemaVersion)
	if err == nil {
		t.Fatal("a file that is not JSON passed the check")
	}
	if !strings.Contains(err.Error(), "task.json") {
		t.Errorf("error does not name the file that broke: %v", err)
	}
}

// Import deliberately skips a file or a column this build does not have
// -- that is what a dump from a newer build looks like -- so the check
// cannot fail on either. It says so instead, because it is also what a
// typo looks like.
func TestCheckWarnsAboutWhatImportWouldIgnore(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	writeTable(t, dir, "task_templates", []map[string]any{{"id": "x"}})
	rows := readTable(t, dir, "task")
	rows[0]["titel"] = "a typo"
	writeTable(t, dir, "task", rows)
	_, db := openDB(t)

	report, err := staterepo.Check(ctx, db, dir, model.SchemaVersion)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	joined := strings.Join(report.Warnings, "\n")
	if !strings.Contains(joined, "task_templates.json") {
		t.Errorf("no warning about a file naming no table: %v", report.Warnings)
	}
	if !strings.Contains(joined, "titel") {
		t.Errorf("no warning about a column no table has: %v", report.Warnings)
	}
}

func TestCheckRefusesADumpFromAnotherSchema(t *testing.T) {
	ctx := context.Background()
	dir := dumpOf(t)
	if err := staterepo.WriteSchemaVersion(dir, model.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	_, db := openDB(t)

	if _, err := staterepo.Check(ctx, db, dir, model.SchemaVersion); !errors.Is(err, staterepo.ErrSchemaTooNew) {
		t.Fatalf("check of a newer dump = %v, want ErrSchemaTooNew", err)
	}
}

func TestCheckRefusesADirectoryThatIsNotAStateRepository(t *testing.T) {
	ctx := context.Background()
	_, db := openDB(t)
	if _, err := staterepo.Check(ctx, db, t.TempDir(), model.SchemaVersion); err == nil {
		t.Fatal("an empty directory passed the check")
	}
}
