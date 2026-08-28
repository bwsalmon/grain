package gitproxy

// Append-only, one-entry-per-request audit log. docs/design.md: "select
// the credential for that repo... and log the tuple." Every decision the
// proxy makes -- allowed, denied, or misconfigured -- gets an entry, so
// the powerful (personal-token) end of the credential ladder staying
// mostly unused is something an operator can actually verify rather than
// assume.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AuditEntry is one decision the proxy made.
type AuditEntry struct {
	Time       time.Time
	Sandbox    string
	Owner      string
	Repo       string
	Action     string
	Credential string // empty when no credential was selected
	Outcome    string
}

// AuditLog records one AuditEntry per request.
type AuditLog interface {
	Record(entry AuditEntry)
}

// NullAuditLog discards every entry.
type NullAuditLog struct{}

func (NullAuditLog) Record(AuditEntry) {}

// FileAuditLog appends one JSON line per entry to a file.
type FileAuditLog struct {
	path string
}

func NewFileAuditLog(path string) (*FileAuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &FileAuditLog{path: path}, nil
}

func (l *FileAuditLog) Record(entry AuditEntry) {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // an audit failure must not take the proxy itself down
	}
	defer f.Close()

	line := struct {
		Time       string `json:"time"`
		Sandbox    string `json:"sandbox"`
		Repo       string `json:"repo"`
		Action     string `json:"action"`
		Credential string `json:"credential,omitempty"`
		Outcome    string `json:"outcome"`
	}{
		Time:       entry.Time.UTC().Format(time.RFC3339Nano),
		Sandbox:    entry.Sandbox,
		Repo:       fmt.Sprintf("%s/%s", entry.Owner, entry.Repo),
		Action:     entry.Action,
		Credential: entry.Credential,
		Outcome:    entry.Outcome,
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
}

// RecordingAuditLog collects entries in memory. For tests.
type RecordingAuditLog struct {
	Entries []AuditEntry
}

func (l *RecordingAuditLog) Record(entry AuditEntry) {
	l.Entries = append(l.Entries, entry)
}
