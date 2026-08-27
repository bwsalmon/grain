// Package dolt opens an embedded Dolt database for the task store.
//
// This is the only package that imports Dolt. Everything above it takes a
// *sql.DB, so the model and the store are testable against anything that
// speaks SQL and carry none of this dependency tree.
//
// Embedded means in-process: no server, no daemon on the VM that holds
// every credential, and no MySQL wire protocol. That is the property a
// Python controller could not have — Dolt embeds only in Go — and it is
// most of why v2 is written in this language.
//
// Single writer. Embedded Dolt permits one, which suits a cron-driven
// controller and does not suit a controller plus a UI plus a human at a
// CLI. When that becomes real, the answer is a Dolt SQL server and this
// package grows a second constructor; nothing above it changes.
package dolt

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/dolthub/driver"
)

// Config names the database and who its commits are attributed to.
//
// Dolt records an author on every commit, so these are not decoration:
// they are what a later `dolt log` shows for changes grain made itself,
// which is the audit trail a data store with version control is for.
type Config struct {
	Dir    string // the directory holding the Dolt database
	Name   string // the database name within it
	Author string // commit author name
	Email  string // commit author email
}

// DefaultConfig is what a deployment gets without saying otherwise.
func DefaultConfig(dir string) Config {
	return Config{
		Dir:    dir,
		Name:   "grain",
		Author: "grain-automation",
		Email:  "grain-automation@localhost",
	}
}

// Open connects to an embedded Dolt database, creating the directory if
// it is absent.
//
// The caller closes the returned *sql.DB. MaxOpenConns is pinned to one
// because embedded Dolt is a single writer and a pool would produce
// lock contention that looks like a deadlock rather than like a
// misconfiguration.
func Open(cfg Config) (*sql.DB, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("dolt: no directory configured")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("dolt: preparing %s: %w", cfg.Dir, err)
	}
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}

	// Embedded Dolt serves one database per directory beneath the data
	// dir, so naming it in the DSN before it exists fails with "database
	// not found". Opening therefore happens twice: once with no database
	// selected, purely to create it, and once for real. Two connections
	// rather than a CREATE-then-USE on the first, because database/sql
	// pools and a USE would bind to whichever connection happened to run
	// it -- correct today only because MaxOpenConns is 1, and silently
	// wrong the moment that changes.
	if err := ensureDatabase(abs, cfg); err != nil {
		return nil, err
	}
	db, err := connect(abs, cfg, cfg.Name)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// dsn builds the driver's file:// connection string. Assembled with
// net/url rather than by concatenation so a directory containing a space
// or a non-ASCII character does not silently point somewhere else.
func dsn(dir string, cfg Config, database string) string {
	q := url.Values{}
	q.Set("commitname", cfg.Author)
	q.Set("commitemail", cfg.Email)
	q.Set("multistatements", "true")
	if database != "" {
		q.Set("database", database)
	}
	return "file://" + dir + "?" + q.Encode()
}

func connect(dir string, cfg Config, database string) (*sql.DB, error) {
	db, err := sql.Open("dolt", dsn(dir, cfg, database))
	if err != nil {
		return nil, fmt.Errorf("dolt: opening %s: %w", dir, err)
	}
	// Embedded Dolt is a single writer; a pool would produce lock
	// contention that reads as a deadlock rather than as a
	// misconfiguration.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("dolt: connecting to %s: %w", dir, err)
	}
	return db, nil
}

func ensureDatabase(dir string, cfg Config) error {
	db, err := connect(dir, cfg, "")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + quoteIdent(cfg.Name)); err != nil {
		return fmt.Errorf("dolt: creating database %s: %w", cfg.Name, err)
	}
	return nil
}

// quoteIdent backtick-quotes a database name. Rejecting a backtick rather
// than escaping it: the name comes from deployment config, and one
// containing a backtick is a misconfiguration worth saying so about.
func quoteIdent(name string) string {
	if name == "" || strings.ContainsAny(name, "`\x00") {
		return "`grain`"
	}
	return "`" + name + "`"
}

// Commit makes a Dolt commit of whatever is in the working set.
//
// This is the durability boundary, not the SQL transaction. Transactions
// keep one logical change atomic; a commit is what makes a cycle's work a
// point the deployment can be reset to, and what a data diff is taken
// against when a declaration change is reviewed.
func Commit(db *sql.DB, message string) error {
	// CALL rather than SELECT: DOLT_COMMIT is a procedure, and -A stages
	// every table so a caller does not have to enumerate them. --allow-empty
	// keeps a cycle that changed nothing from being an error to special-case.
	_, err := db.Exec("CALL DOLT_COMMIT('-A', '--allow-empty', '-m', ?)", message)
	return err
}
