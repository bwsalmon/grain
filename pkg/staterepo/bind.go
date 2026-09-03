package staterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrSchemaTooNew is returned when the repository was written by a build
// that knows a later schema than this one. It is pkg/model's own
// ErrSchemaTooNew argument, one layer out: importing rows shaped for a
// schema this build does not have produces a database that is wrong
// rather than one that refuses to start.
var ErrSchemaTooNew = errors.New("state repository was written by a newer build of grain")

// ErrSchemaTooOld is returned when the repository holds a dump from an
// earlier schema. Nothing here migrates one: a state repository is a
// dump, not a database, and the supported move is to wipe it and let a
// fresh Seed write today's schema (README.md's own note on the state
// repository being destructive across schema changes).
var ErrSchemaTooOld = errors.New("state repository was written by an older build of grain")

// Load makes the repository and the database agree, in whichever
// direction has something to say.
//
// A repository that already holds a dump wins: it is the source of
// truth, and the database is a materialisation of it that this rebuilds
// outright. That is what makes a merged pull request take effect, and
// what makes "clone the repository onto a new host" a complete restore.
//
// A repository with no dump is seeded from the database instead, which
// covers both a brand new install (an empty database, exported as empty
// files) and pointing a running grain at a fresh empty repository, where
// what is already in the database is exactly what should land in the
// first commit.
func Load(ctx context.Context, r *Repo, db *sql.DB, version int) error {
	if _, err := r.Pull(ctx); err != nil {
		return err
	}
	if !HasDump(r.Dir()) {
		return Seed(ctx, r, db, version)
	}
	found, err := ReadSchemaVersion(r.Dir())
	if err != nil {
		return err
	}
	switch {
	case found > version:
		return fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooNew, found, version)
	case found < version:
		return fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooOld, found, version)
	}
	return Import(ctx, db, r.Dir())
}

// Seed writes the database out as the repository's first content: the
// dump, the schema stamp, and the README that tells whoever opens the
// repository next what they are looking at.
func Seed(ctx context.Context, r *Repo, db *sql.DB, version int) error {
	if err := writeReadme(r.Dir()); err != nil {
		return err
	}
	if err := Export(ctx, db, r.Dir()); err != nil {
		return err
	}
	if err := WriteSchemaVersion(r.Dir(), version); err != nil {
		return err
	}
	if _, err := r.Commit(ctx, "Initialise grain's state repository\n\n"+
		"grain exports its database here as JSON, one file per table, so its\n"+
		"settings can be read and changed through pull requests like anything\n"+
		"else. See README.md."); err != nil {
		return err
	}
	return r.Push(ctx)
}

// Sync writes the database out and commits and pushes anything that
// changed, reporting whether there was anything to commit.
//
// The commit message is deliberately plain and identical every time.
// What changed is in the diff, which is the whole reason the dump is
// text; a message that tried to summarise it would be a second, worse
// answer that could disagree with the first.
func Sync(ctx context.Context, r *Repo, db *sql.DB, version int) (bool, error) {
	if err := Export(ctx, db, r.Dir()); err != nil {
		return false, err
	}
	if err := WriteSchemaVersion(r.Dir(), version); err != nil {
		return false, err
	}
	changed, err := r.Commit(ctx, "Update grain state\n\n"+
		"Written by grain from its own database at "+time.Now().UTC().Format(time.RFC3339)+".")
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	return true, r.Push(ctx)
}

func writeReadme(dir string) error {
	return writeFileIfChanged(filepath.Join(dir, ReadmeFile), []byte(readme))
}

const readme = `# grain state

This repository is grain's database, written out as text.

grain runs against an embedded SQLite database, and exports every table
here after each change: ` + "`" + TablesDir + `/<table>.json` + "`" + `, one file per table,
rows sorted by primary key and columns in the table's declared order so
that an unchanged database always produces byte-identical files.

## What is here

- ` + "`" + TablesDir + `/` + "`" + ` -- the database. Settings (templates, suites, repo
  configuration, prompt extensions, schedules), tasks, runs and metrics.
- ` + "`" + SchemaVersionFile + "`" + ` -- the schema the dump was written by. A grain
  that knows a different one refuses to import rather than guessing.
- ` + "`" + SecretsFile + "`" + ` -- every secret grain holds, encrypted to a public key
  whose private half only the operator has. Nothing else in this
  repository is encrypted, and nothing but secrets goes in this file.

## Changing something

Open a pull request against this repository the way you would against
any other. On merge, grain pulls, and the merged files replace what is
in its database -- so a deleted row is a deleted row.

Do not hand-edit while grain is running unless you mean it: grain is the
only writer, exports on a timer, and a local edit it did not make is
overwritten by the next export.

## Secrets

` + "`" + SecretsFile + "`" + ` is an encrypted blob. Agents never read it: a run gets a
secret only through the secret input a human asked for, exactly as
before. Losing the private key means losing every secret in here -- the
file cannot be recovered from grain, which holds no copy of the key
beyond the one file the operator manages.
`

// EnsureIgnored writes a .gitignore that keeps the things which are not
// state out of the repository, for the case where the working tree
// shares a directory with anything else. Small, and worth having
// written down rather than assumed: a stray editor swap file committed
// into the state repository would be pushed to the remote.
func EnsureIgnored(dir string) error {
	const body = "# Written by grain.\n" +
		"*.swp\n" +
		".DS_Store\n" +
		// The private key never belongs here, and naming it makes an
		// operator who drops one in this directory by mistake fail to
		// commit it rather than push it to a remote.
		"*.key\n" +
		"secrets.key\n"
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeFileIfChanged(path, []byte(body))
}
