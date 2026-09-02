// Package sqlite opens an embedded SQLite database for the task store.
//
// This is the only package that imports a SQLite driver. Everything
// above it takes a *sql.DB, so the model and the store are testable
// against anything that speaks SQL and carry none of this dependency
// tree.
//
// The driver is modernc.org/sqlite, a pure-Go transpilation of SQLite
// with no cgo involved: the binary that embeds it needs no C toolchain,
// no shared library the target host has to already carry (the ICU/glibc
// coupling embedded Dolt required -- see the Makefile's git history for
// what that used to cost), and no server process anywhere.
//
// Multiple processes -- a daemon, a UI, a CLI, all writing -- open the
// same database file directly rather than dialing a server: SQLite's own
// file locking is what serialises them, in WAL mode so readers never
// block on a writer. Open sets that up. There is no second constructor
// here the way pkg/model/dolt had Connect/OpenOrConnect for a SQL
// server, because SQLite has no wire protocol to dial in the first
// place -- every writer just opens the file.
package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Config names the database file to open.
type Config struct {
	Dir  string // the directory holding the database file
	Name string // the database's file name, without its ".db" extension
}

// DefaultConfig is what a deployment gets without saying otherwise.
func DefaultConfig(dir string) Config {
	return Config{Dir: dir, Name: "grain"}
}

// Open connects to an embedded SQLite database, creating the directory
// and the database file if either is absent.
//
// busy_timeout and _txlock=immediate together are what make Store.write
// correct (store.go's own doc comment on write): every transaction takes
// SQLite's write lock up front, at BEGIN rather than at the first write
// statement, so two overlapping writers fail (or block, until
// busy_timeout elapses) at the same point every time instead of one
// succeeding partway through and losing to the other at COMMIT the way a
// deferred transaction would. A blocked writer is given five seconds to
// simply wait its turn before Store.write's own retry loop ever sees an
// error -- worth having on top of the retry loop, not instead of it,
// since a request that outlasts busy_timeout still needs somewhere to
// go.
func Open(cfg Config) (*sql.DB, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("sqlite: no directory configured")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("sqlite: preparing %s: %w", cfg.Dir, err)
	}
	name := cfg.Name
	if name == "" {
		name = "grain"
	}
	path := filepath.Join(cfg.Dir, name+".db")

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: connecting to %s: %w", path, err)
	}
	// WAL is what lets a reader run concurrently with the one writer that
	// holds the write lock, rather than every access serialising through
	// SQLite's older rollback-journal locking -- the property that makes
	// "a daemon, a UI and a CLI all pointed at the same file" work at all.
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: enabling WAL on %s: %w", path, err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: enabling foreign keys on %s: %w", path, err)
	}
	return db, nil
}

// dsn builds the driver's connection string. Assembled with net/url
// rather than by concatenation so a directory containing a space or a
// non-ASCII character does not silently point somewhere else.
func dsn(path string) string {
	q := url.Values{}
	// One _pragma per setting -- modernc.org/sqlite applies each on every
	// new connection the pool opens, which a bare PRAGMA statement run
	// once after Open would not: database/sql may open more connections
	// later, and each needs its own busy_timeout and txlock mode.
	q.Add("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}
