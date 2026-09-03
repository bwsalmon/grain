package main

// `grain state check` end to end: a directory of state files in, a
// verdict out, with no data directory, no daemon and no store of its own
// -- which is the whole shape of the CI step this exists to be.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// exportedState writes a real dump of a real database, the way a running
// daemon's sync does, and returns the directory holding it.
func exportedState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	defer db.Close()
	if err := store.PutTask(ctx, model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "Rename the endpoint",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}},
			Reason:      model.ReasonDirect,
		},
	}); err != nil {
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

func TestStateCheckAcceptsAnExportedRepository(t *testing.T) {
	if err := runState([]string{"check", exportedState(t)}); err != nil {
		t.Fatalf("checking a dump grain wrote: %v", err)
	}
}

// -data-dir is required by every other `grain state` command and must
// not be by this one: the runner checking a pull request has a checkout
// and nothing else.
func TestStateCheckNeedsNoDataDir(t *testing.T) {
	dir := exportedState(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	// No argument at all, which is what `grain state check` in a CI step
	// at the top of the checkout looks like.
	if err := runState([]string{"check"}); err != nil {
		t.Fatalf("checking the working directory: %v", err)
	}
}

func TestStateCheckFailsOnADumpThatWillNotLoad(t *testing.T) {
	dir := exportedState(t)
	path := filepath.Join(dir, staterepo.TablesDir, "task.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	// A column the schema insists on, gone -- the hand edit that used to
	// take a deployment down at its next start.
	delete(rows[0], "title")
	edited, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(edited, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	err = runState([]string{"check", dir})
	if err == nil {
		t.Fatal("a task row with no title passed `grain state check`")
	}
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("error does not name what broke: %v", err)
	}
}

func TestStateCheckRefusesADirectoryThatIsNotAStateRepository(t *testing.T) {
	if err := runState([]string{"check", t.TempDir()}); err == nil {
		t.Fatal("an empty directory passed `grain state check`")
	}
}
