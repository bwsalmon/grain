package staterepo_test

// What a dump has to be able to carry.
//
// The rest of the suite checks that the right rows move in the right
// direction at the right moment. This file checks the thing underneath
// all of it: that a value written into the database and read back out of
// a clone of the repository is the same value -- the same bytes, in the
// same SQLite storage class, NULL where it was NULL and empty where it
// was empty.
//
// It is worth its own file because the failures here are silent. A row
// that does not import at all fails loudly, at COMMIT, in front of
// `grain state check`; a row that imports with different bytes in it
// looks exactly like a successful restore.

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// A run's transcript is whatever came off somebody else's stdout, and
// SQLite does not police what goes into a TEXT column, so bytes that are
// not valid UTF-8 do reach the export. encoding/json cannot carry one --
// it substitutes U+FFFD and reports no error -- so a dump that handed
// them to it held different bytes than the database and every restore
// from it was quietly corrupt.
func TestADumpCarriesTextThatIsNotValidUTF8(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	// A truncated multi-byte sequence and a lone continuation byte:
	// what a transcript captured mid-character looks like.
	transcript := "make test\n\xe2\x82quipped\xff\xfe\n"
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "a1b2-1", TaskID: "a1b2", Sandbox: "grain-a1b2-1", Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := store.SetRunTranscript(ctx, "a1b2-1", transcript); err != nil {
		t.Fatalf("writing the transcript: %v", err)
	}

	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	_, restored := openDB(t)
	if err := staterepo.Import(ctx, restored, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	var got, class string
	if err := restored.QueryRowContext(ctx,
		"SELECT `transcript`, typeof(`transcript`) FROM `task_run` WHERE `id` = 'a1b2-1'").
		Scan(&got, &class); err != nil {
		t.Fatalf("reading the restored transcript: %v", err)
	}
	if got != transcript {
		t.Fatalf("the round trip changed the transcript:\n got %q\nwant %q", got, transcript)
	}
	// And it is still text. Base64 is how those bytes cross the file, but
	// they have to land back in the storage class they left: a transcript
	// that came back a blob would compare equal on the bytes and be a
	// different database.
	if class != "text" {
		t.Fatalf("the transcript came back as %s, not text", class)
	}
}

// The other side of the same coin: a column that really is binary stays
// binary, and every one of the 256 byte values survives.
func TestADumpCarriesABlobWholeAndStillABlob(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	content := make([]byte, 256)
	for i := range content {
		content[i] = byte(i)
	}
	if _, err := store.AddAttachment(ctx, model.Attachment{
		TaskID: "a1b2", Filename: "screenshot.png", ContentType: "image/png",
		Size: int64(len(content)), Content: content, CreatedAt: now,
	}); err != nil {
		t.Fatalf("attaching: %v", err)
	}

	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	_, restored := openDB(t)
	if err := staterepo.Import(ctx, restored, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	var got []byte
	var class string
	if err := restored.QueryRowContext(ctx,
		"SELECT `content`, typeof(`content`) FROM `task_attachment` WHERE `task_id` = 'a1b2'").
		Scan(&got, &class); err != nil {
		t.Fatalf("reading the restored attachment: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("the round trip changed the attachment: %d bytes, want %d", len(got), len(content))
	}
	if class != "blob" {
		t.Fatalf("the attachment came back as %s, not blob", class)
	}
}

// Everything else, all at once: a database with a row in every shape
// this schema can produce, exported, imported into an empty database and
// compared column by column with typeof() alongside every value.
//
// Written this way rather than as a list of fields to check because the
// list would be the thing that went stale. A column a later schema adds
// is covered by this the day it is added, and a column whose values stop
// surviving the round trip is named by the failure.
func TestARestoredDatabaseIsTheSameDatabase(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	fill(t, ctx, store, db)

	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	_, restored := openDB(t)
	if err := staterepo.Import(ctx, restored, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	sameDatabase(t, ctx, db, restored, "a database restored from its own dump")
}

// And the fixed point that makes an export safe to run on a timer: a
// database restored from a dump exports to byte-identical files. If it
// did not, a deployment restored from a clone would commit a diff of its
// own restore on its very first sync -- and every host that restored
// would produce a different one.
func TestARestoreExportsToTheSameBytes(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	fill(t, ctx, store, db)

	first := t.TempDir()
	if err := staterepo.Export(ctx, db, first); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	_, restored := openDB(t)
	if err := staterepo.Import(ctx, restored, first); err != nil {
		t.Fatalf("importing: %v", err)
	}
	second := t.TempDir()
	if err := staterepo.Export(ctx, restored, second); err != nil {
		t.Fatalf("re-exporting: %v", err)
	}
	sameDump(t, first, second)
}

// sameDump fails unless two exported directories hold the same files
// with the same bytes.
func sameDump(t *testing.T, a, b string) {
	t.Helper()
	names := func(dir string) []string {
		entries, err := os.ReadDir(filepath.Join(dir, staterepo.TablesDir))
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	left, right := names(a), names(b)
	if !equalStrings(left, right) {
		t.Fatalf("the two dumps hold different files:\n%v\n%v", left, right)
	}
	for _, name := range left {
		x := read(t, filepath.Join(a, staterepo.TablesDir, name))
		y := read(t, filepath.Join(b, staterepo.TablesDir, name))
		if x != y {
			t.Fatalf("%s differs between the two dumps:\n%s\n---\n%s", name, x, y)
		}
	}
}

// fill writes a row in every shape the schema can hold: NULL and empty
// string side by side, text with newlines and control characters and
// astral-plane runes in it, a REAL sort key, a 64-bit id, a blob, a
// timestamp with sub-second precision -- and rows in both tiers and on
// both sides of the settings/observation line, so the tests above cover
// every table the export writes rather than the two everything else
// happens to touch.
func fill(t *testing.T, ctx context.Context, store *model.Store, db *sql.DB) {
	t.Helper()
	// Text that has caused trouble in one system or another: an emoji
	// (outside the basic plane), a right-to-left mark, a NUL, a tab, CRLF,
	// a quote, a backslash, and the HTML-ish characters encoding/json
	// escapes.
	awkward := "quote \" backslash \\ tab\t crlf\r\n nul\x00 emoji \U0001F600 rtl ‏ " +
		"html <&> and a very long line " + strings.Repeat("x", 4000)

	tk := task("a1b2")
	tk.Title = awkward
	tk.Body = ""
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatalf("putting a task: %v", err)
	}
	// A second task with no target at all, which is a NULL where the
	// first one has a value.
	plain := task("c3d4")
	plain.Target = nil
	sub := now.Add(1234567 * time.Nanosecond)
	plain.CreatedAt = &sub
	if err := store.PutTask(ctx, plain); err != nil {
		t.Fatalf("putting a task: %v", err)
	}

	commentID, err := store.AddComment(ctx, model.Comment{
		TaskID: "a1b2", CreatedAt: sub, Body: awkward,
		Author: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}},
	})
	if err != nil {
		t.Fatalf("commenting: %v", err)
	}
	// One attachment on the task itself (a NULL comment_id) and one on
	// the comment (an integer one).
	blob := make([]byte, 512)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	for _, att := range []model.Attachment{
		{TaskID: "a1b2", Filename: "body.bin", ContentType: "application/octet-stream",
			Size: int64(len(blob)), Content: blob, CreatedAt: sub},
		{TaskID: "a1b2", CommentID: &commentID, Filename: "", ContentType: "",
			Size: 0, Content: []byte{}, CreatedAt: sub},
	} {
		if _, err := store.AddAttachment(ctx, att); err != nil {
			t.Fatalf("attaching: %v", err)
		}
	}

	if err := store.StartRun(ctx, model.Run{
		ID: "a1b2-1", TaskID: "a1b2", Sandbox: "grain-a1b2-1", Attempt: 1, StartedAt: sub,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := store.SetRunTranscript(ctx, "a1b2-1", awkward+"\n\xff\xfe raw bytes"); err != nil {
		t.Fatalf("writing a transcript: %v", err)
	}
	if err := store.FinishRun(ctx, "a1b2-1", sub.Add(time.Minute), "succeeded", awkward); err != nil {
		t.Fatalf("finishing a run: %v", err)
	}
	// A run still live, so the finished columns are NULL rather than set.
	if err := store.StartRun(ctx, model.Run{
		ID: "c3d4-1", TaskID: "c3d4", Sandbox: "grain-c3d4-1", Attempt: 1, StartedAt: sub,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	// An observation, which is what a reconcile cycle stamps.
	at := sub.Add(2 * time.Minute)
	if err := store.ObserveField(ctx, "a1b2", at, func(o *model.Observation) {
		o.PrOpenedAt = &at
	}); err != nil {
		t.Fatalf("observing: %v", err)
	}

	// The settings half.
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "nightly", Title: awkward, Body: awkward,
		Reads:     []model.RepoRef{{Owner: "owner", Name: "payments-api"}},
		CreatedAt: sub,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if err := store.PutSuite(ctx, model.Suite{
		ID: "suite-1", Name: awkward, Mode: model.SuiteCount, Count: 3,
		Items:     []model.SuiteItem{{TemplateID: "tpl-1"}, {TemplateID: "tpl-1"}},
		AutoMerge: true, CreatedAt: sub,
	}); err != nil {
		t.Fatalf("putting a suite: %v", err)
	}
	if err := store.PutRepoConfig(ctx, model.RepoConfig{
		Repo:                model.RepoRef{Owner: "owner", Name: "payments-api"},
		DefaultCapabilities: []string{"githubtoken"},
		PromptExtension:     awkward,
		SetupCommand:        "",
	}); err != nil {
		t.Fatalf("putting repo config: %v", err)
	}
	// Two schedules, one of each recurrence shape, so the columns that
	// are NULL for one kind and set for the other are covered both ways.
	if err := store.PutSchedule(ctx, model.Schedule{
		ID: "sch-1", Title: awkward, Body: "", Target: model.RepoRef{Owner: "owner", Name: "payments-api"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 6},
		Enabled:    true, NextRunAt: sub, CreatedAt: sub,
	}); err != nil {
		t.Fatalf("putting a schedule: %v", err)
	}
	last := sub.Add(-time.Hour)
	if err := store.PutSchedule(ctx, model.Schedule{
		ID: "sch-2", Title: "weekly", Base: "release",
		Target:     model.RepoRef{Owner: "owner", Name: "payments-api"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceWeekly, TimeOfDay: 90, Weekday: time.Tuesday},
		Enabled:    false, NextRunAt: sub, LastRunAt: &last, CreatedAt: sub,
	}); err != nil {
		t.Fatalf("putting a schedule: %v", err)
	}
	// Rows that carry a REAL sort key -- suite_item's order_key above and
	// task's own -- are already written by the calls above; grain_config
	// is the deployment's own row and the one every settings import
	// replaces.
	cfg := model.Config{}
	if stored, err := store.GetConfig(ctx); err != nil {
		t.Fatalf("reading the config: %v", err)
	} else if stored != nil {
		cfg = *stored
	}
	cfg.PromptExtension = awkward
	cfg.MaxWorkers = 7
	if err := store.PutConfig(ctx, cfg); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	// A handful of tables nothing above reaches, written straight through
	// SQL: they exist in the dump, and a value that does not survive them
	// is as lost as one that does not survive a task.
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO `task_tag` (`task_id`, `tag`) VALUES (?, ?)", []any{"a1b2", awkward}},
		{"INSERT INTO `task_link` (`task_id`, `kind`, `target`, `blocks`) VALUES (?, ?, ?, ?)",
			[]any{"a1b2", "depends_on", "c3d4", 1}},
		{"INSERT INTO `task_read` (`task_id`, `owner`, `name`) VALUES (?, ?, ?)",
			[]any{"a1b2", "owner", "docs"}},
		{"INSERT INTO `task_grant` (`task_id`, `capability`, `via`, `folder`) VALUES (?, ?, ?, ?)",
			[]any{"a1b2", "githubtoken", "", ""}},
	} {
		if _, err := db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("writing %s: %v", stmt.sql, err)
		}
	}
}

// A dump nothing in the database explains is still one every table of
// the database is in: an export writes a file per table, empty tables
// included, so a restore from it clears what it does not mention rather
// than leaving yesterday's rows behind.
func TestEveryTableGetsAFileEvenWithNoRows(t *testing.T) {
	ctx := context.Background()
	_, db := openDB(t)
	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	for _, table := range realTables(t, ctx, db) {
		path := filepath.Join(dir, staterepo.TablesDir, table+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s has no file in the dump: %v", table, err)
		}
	}
	// And nothing else: a file naming a table that is not there would be
	// imported as rows nothing wrote.
	entries, err := os.ReadDir(filepath.Join(dir, staterepo.TablesDir))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, table := range realTables(t, ctx, db) {
		known[table+".json"] = true
	}
	for _, e := range entries {
		if !known[e.Name()] {
			t.Errorf("the dump holds %s, which names no table", e.Name())
		}
	}
	if len(entries) == 0 {
		t.Fatal("the dump is empty")
	}
}
