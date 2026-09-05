// Package systemlog implements pkg/ui.LogSource against the places
// grain v2 actually writes its own logs to: a systemd unit's journal
// (Journalctl), the kernel's own ring buffer under the daemon (Dmesg),
// and a plain append-only file such as the git proxy's audit log
// (File) -- bwsalmon/agents#444's "core system logs" for the UI's
// System pane.
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
	return journalctl(ctx, "journalctl -u "+j.Unit, lines, "--unit", j.Unit)
}

// Dmesg tails the kernel's own log for the machine the daemon runs on --
// the System pane's answer to the failures no userspace log can describe,
// because the process they happened to is the one that stopped writing:
// an OOM kill of an agent CLI mid-run, a disk erroring under a sandbox's
// checkout, a network interface dropping the git proxy's traffic. Sandbox
// health says the machine is unwell and Top says which process is wearing
// it out; this says what the kernel did about it.
//
// It reads that log through `journalctl --dmesg` rather than dmesg(1),
// which is the same thing by a route this deployment actually has.
// dmesg(1) reads the ring buffer through /dev/kmsg or the syslog(2)
// call, and the daemon's container is given neither -- it runs
// unprivileged as $GRAIN_USER, without CAP_SYSLOG and without that
// device (scripts/setup.sh's own docker_run_args) -- so dmesg in there
// fails on permissions, and would need util-linux added to the image to
// get that far. journald already collects the same kernel messages into
// the host journal that setup.sh bind-mounts in read-only for
// Journalctl above, group and all, so this needs nothing new installed
// and nothing new granted. Both spellings show the current boot only;
// --dmesg implies journalctl's own -b.
type Dmesg struct{}

func (Dmesg) Tail(ctx context.Context, lines int) ([]string, error) {
	return journalctl(ctx, "journalctl --dmesg", lines, "--dmesg")
}

// journalctl runs one journalctl read of the last `lines` entries and
// splits its output. desc names the command in the error, since what an
// operator needs from a failure here is which read failed -- a unit that
// does not exist on this host, a journal directory the container was not
// given -- rather than "exit status 1".
func journalctl(ctx context.Context, desc string, lines int, match ...string) ([]string, error) {
	args := append(append([]string{}, match...),
		"--lines", strconv.Itoa(lines),
		"--no-pager",
		"--output=short-iso")
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", desc, err, strings.TrimSpace(stderr.String()))
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
