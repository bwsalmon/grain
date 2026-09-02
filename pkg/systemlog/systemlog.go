// Package systemlog implements pkg/ui.LogSource against the places
// grain v2 actually writes its own logs to: a systemd unit's journal
// (Journalctl) and a plain append-only file such as the git proxy's
// audit log (File) -- bwsalmon/agents#444's "core system logs" for the
// UI's debugging page.
package systemlog

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Journalctl tails a systemd unit's journal by shelling out to
// journalctl -- the same mechanism v1's `read_grain_logs` MCP tool used
// (grain/automation/mcp_server.py), and what scripts/setup.sh's own
// printed "Logs: journalctl -u grain-daemon.service -f" already tells an
// operator to reach for by hand. There is no journal client library
// vendored here; the project otherwise stays pure Go, and journalctl
// itself is already a hard dependency of the systemd unit setup.sh
// installs.
type Journalctl struct {
	// Unit is the systemd unit name to read, e.g. "grain-daemon.service".
	Unit string
}

func (j Journalctl) Tail(ctx context.Context, lines int) ([]string, error) {
	cmd := exec.CommandContext(ctx, "journalctl",
		"--unit", j.Unit,
		"--lines", strconv.Itoa(lines),
		"--no-pager",
		"--output=short-iso")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("journalctl -u %s: %w: %s", j.Unit, err, strings.TrimSpace(stderr.String()))
	}
	return splitLines(stdout.String()), nil
}

// File tails a plain append-only log file, such as the git proxy's own
// audit.log (pkg/gitproxy.FileAuditLog) -- one JSON line per request,
// with no rotation, so reading the whole thing before taking the last N
// lines needs no size guard beyond what an operator's own disk already
// enforces.
type File struct {
	// Path is the log file to read.
	Path string
}

func (f File) Tail(ctx context.Context, lines int) ([]string, error) {
	data, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	all := splitLines(string(data))
	if len(all) <= lines {
		return all, nil
	}
	return all[len(all)-lines:], nil
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
