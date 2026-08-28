package dolt

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ServerConfig names a running Dolt SQL server to connect to, rather than
// a directory to open in-process.
//
// This is the second constructor the package doc comment promised. The
// embedded database is a single writer, which suited grain while a
// cron-driven controller was the only thing writing; it stopped suiting
// grain the moment the CLI and the UI became the way tasks are created
// (README, "Input is a model update, not a GitHub issue"), because that
// is a controller plus a UI plus a human at a terminal, all writing.
//
// Nothing above this package changes: model.Store still takes a *sql.DB
// and still imports no driver, so the same Store code runs against either
// end. What does change is that grain now has a server to run -- see
// Connect on what that buys and what it costs.
type ServerConfig struct {
	Addr     string // host:port of the Dolt SQL server
	Database string // the database name on it
	User     string
	Password string
}

// DefaultServerConfig is what a deployment gets without saying otherwise:
// a server on the loopback interface at Dolt's own default port, as the
// root user with no password, which is what `dolt sql-server` starts as
// out of the box. A deployment putting this anywhere other than
// localhost is expected to set all four fields.
func DefaultServerConfig(addr string) ServerConfig {
	if addr == "" {
		addr = "127.0.0.1:3306"
	}
	return ServerConfig{Addr: addr, Database: "grain", User: "root"}
}

// Connect dials a Dolt SQL server over the MySQL wire protocol.
//
// Unlike Open, MaxOpenConns is deliberately NOT pinned to one. That pin
// exists in the embedded case because embedded Dolt permits a single
// writer and a pool there produces lock contention that reads as a
// deadlock; a server is the thing that makes concurrent writers a
// supported case rather than a hazard, so pinning the pool here would
// throw away the entire reason for connecting to one. Concurrent writers
// still conflict -- they just conflict as transactions the server
// serialises and reports on, which a caller can retry, instead of as a
// file lock nobody can see.
//
// A connection lifetime is set because a long-lived daemon (graind) holds
// idle connections across a server restart otherwise, and gets an error
// on the next use of a connection the server has already forgotten.
func Connect(cfg ServerConfig) (*sql.DB, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("dolt: no server address configured")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("dolt: no database name configured")
	}

	db, err := sql.Open("mysql", ServerDSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("dolt: opening %s: %w", cfg.Addr, err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("dolt: connecting to %s: %w", cfg.Addr, err)
	}
	return db, nil
}

// ServerDSN builds the MySQL connection string Connect dials. Exported so
// a deployment can log or verify what it is about to connect to without
// connecting -- and so the two settings below are testable without a
// server to hand.
//
// Both query parameters are load-bearing, not defaults being restated:
//
//   - parseTime, because the wire protocol hands DATETIME back as bytes
//     otherwise, and every time.Time field on model.Task and
//     model.Observation would fail to scan. The embedded driver returns
//     them natively, so this is exactly the difference that would
//     otherwise show up as a scan error only when run against a server.
//   - loc=UTC, because the store writes UTC and a driver left on Local
//     would hand it back shifted, which is a wrong timestamp rather than
//     an error -- the merge queue orders by Task.CreatedAt.
func ServerDSN(cfg ServerConfig) string {
	auth := url.PathEscape(cfg.User)
	if cfg.Password != "" {
		auth += ":" + url.PathEscape(cfg.Password)
	}
	q := url.Values{}
	q.Set("parseTime", "true")
	q.Set("loc", "UTC")
	return fmt.Sprintf("%s@tcp(%s)/%s?%s", auth, cfg.Addr, cfg.Database, q.Encode())
}

// OpenOrConnect picks a constructor from what the deployment configured:
// a Dolt SQL server when one is addressed, the embedded database in dir
// otherwise.
//
// Both remain supported on purpose. Embedded is still the right answer
// for a one-process deployment and for every test in this repo, which is
// why the store's own tests run against it; a server is what makes the
// CLI, the UI and graind writing at once a supported case rather than a
// hazard. Since model.Store takes a *sql.DB and imports no driver,
// choosing between them is this one function and nothing above it.
func OpenOrConnect(dir string, server ServerConfig) (*sql.DB, error) {
	if server.Addr != "" {
		return Connect(server)
	}
	if dir == "" {
		return nil, fmt.Errorf("dolt: neither a server address nor a data directory configured")
	}
	return Open(DefaultConfig(dir))
}
