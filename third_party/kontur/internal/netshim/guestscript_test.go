package netshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// guestScript is the guest's half of this package: what GuestConfig
// derives is only worth anything if the thing that reads it back applies
// it, and that thing is a shell script in the guest image rather than
// Go. It is exercised here, against a fake "ip", because the alternative
// is finding out from a booted guest on a cluster -- which is how the
// gap this fixes was found in the first place.
const guestScript = "../../deploy/guest-image/overlay-common/usr/local/libexec/kontur-configure-routes"

// fakeIP records every invocation it is given, one line per call, and
// answers "route show" with whatever table the test says the guest has.
const fakeIP = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKEIP_LOG"
if [ -n "${FAKEIP_NO_LINK:-}" ]; then
	case "$*" in
	*"link show"*) exit 1 ;;
	esac
fi
case "$*" in
*"route show"*) [ -z "${FAKEIP_ROUTES:-}" ] || printf '%s\n' "${FAKEIP_ROUTES}" ;;
esac
exit 0
`

// runGuestScript runs kontur-configure-routes against a command line of
// the test's choosing, with a fake "ip" on its PATH, and returns the
// calls it made.
func runGuestScript(t *testing.T, cmdline string, env ...string) []string {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("creating %s: %v", bin, err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte(fakeIP), 0o755); err != nil {
		t.Fatalf("writing the fake ip: %v", err)
	}
	cmdlinePath := filepath.Join(dir, "cmdline")
	if err := os.WriteFile(cmdlinePath, []byte(cmdline+"\n"), 0o644); err != nil {
		t.Fatalf("writing the fake cmdline: %v", err)
	}
	logPath := filepath.Join(dir, "ip.log")

	cmd := exec.Command("sh", guestScript)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KONTUR_CMDLINE_FILE="+cmdlinePath,
		"FAKEIP_LOG="+logPath,
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kontur-configure-routes: %v\n%s", err, out)
	}

	logged, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the fake ip's log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

func TestGuestScriptAppliesCarriedRoutes(t *testing.T) {
	const ptpCmdline = "console=ttyS0 root=/dev/vda rw " +
		"ip=10.244.0.5::10.244.0.1:255.255.255.0::eth0:off " +
		"kontur.routes=10.244.0.1/32,10.244.0.0/24@10.244.0.1,0.0.0.0/0@10.244.0.1"

	for _, tc := range []struct {
		name    string
		cmdline string
		env     []string
		want    []string
	}{
		{
			// A pod on kind's ptp CNI, where the guest's own netmask is
			// the wrong answer: the gateway's host route goes in first
			// (nothing else makes the gateway reachable), and the
			// subnet's own route then replaces, in place, the on-link
			// one "ip=" left behind.
			name:    "point-to-point",
			cmdline: ptpCmdline,
			env: []string{"FAKEIP_ROUTES=10.244.0.1 dev eth0 scope link\n" +
				"10.244.0.0/24 via 10.244.0.1 dev eth0\n" +
				"default via 10.244.0.1 dev eth0"},
			want: []string{
				"link show dev eth0",
				"route replace 10.244.0.1/32 dev eth0 scope link",
				"route replace 10.244.0.0/24 via 10.244.0.1 dev eth0",
				"route replace 0.0.0.0/0 via 10.244.0.1 dev eth0",
				"-4 route show dev eth0",
			},
		},
		{
			// docker's own bridge, which CI and the docker backend
			// exercise: the CNI's routes are the routes "ip=" already
			// installed, so every one of these replaces what is already
			// there with itself and nothing is dropped. This case
			// changing is the regression to watch for.
			name: "bridge",
			cmdline: "console=ttyS0 root=/dev/vda rw " +
				"ip=172.17.0.2::172.17.0.1:255.255.0.0::eth0:off " +
				"kontur.routes=172.17.0.0/16,0.0.0.0/0@172.17.0.1",
			env: []string{"FAKEIP_ROUTES=172.17.0.0/16 dev eth0 proto kernel scope link src 172.17.0.2\n" +
				"default via 172.17.0.1 dev eth0"},
			want: []string{
				"link show dev eth0",
				"route replace 172.17.0.0/16 dev eth0 scope link",
				"route replace 0.0.0.0/0 via 172.17.0.1 dev eth0",
				"-4 route show dev eth0",
			},
		},
		{
			// A CNI whose table has no entry for the prefix the address
			// implies -- the shape Calico and Cilium use, a gateway on
			// a link-local address nothing else routes to. There is no
			// route to replace the on-link subnet route with, so it is
			// deleted instead: left in place it is more specific than
			// the default route and would swallow every peer.
			name: "a route the container network does not have",
			cmdline: "console=ttyS0 " +
				"ip=10.244.0.5::169.254.1.1:255.255.255.0::eth0:off " +
				"kontur.routes=169.254.1.1/32,0.0.0.0/0@169.254.1.1",
			env: []string{"FAKEIP_ROUTES=169.254.1.1 dev eth0 scope link\n" +
				"10.244.0.0/24 dev eth0 proto kernel scope link src 10.244.0.5\n" +
				"default via 169.254.1.1 dev eth0"},
			want: []string{
				"link show dev eth0",
				"route replace 169.254.1.1/32 dev eth0 scope link",
				"route replace 0.0.0.0/0 via 169.254.1.1 dev eth0",
				"-4 route show dev eth0",
				"route del 10.244.0.0/24 dev eth0",
			},
		},
		{
			// The interface is read back out of the "ip=" parameter
			// rather than assumed, for a caller who wrote their own.
			name: "an interface other than eth0",
			cmdline: "console=ttyS0 ip=10.244.0.5::10.244.0.1:255.255.255.0::eth1:off " +
				"kontur.routes=0.0.0.0/0@10.244.0.1",
			want: []string{
				"link show dev eth1",
				"route replace 0.0.0.0/0 via 10.244.0.1 dev eth1",
				"-4 route show dev eth1",
			},
		},
		{
			// No netshim in front of this guest, or an operator's own
			// "ip=", which takes the routes with it: the guest's table
			// is not this script's to touch.
			name:    "no routes on the command line",
			cmdline: "console=ttyS0 root=/dev/vda rw ip=192.168.1.5::192.168.1.1:255.255.255.0::eth0:off",
			want:    nil,
		},
		{
			// A guest with no such NIC at all is a guest with nothing
			// to route over, not a failed boot.
			name:    "no interface to route over",
			cmdline: ptpCmdline,
			env:     []string{"FAKEIP_NO_LINK=1"},
			want:    []string{"link show dev eth0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGuestScript(t, tc.cmdline, tc.env...)
			if len(got) != len(tc.want) {
				t.Fatalf("ip calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ip call %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
