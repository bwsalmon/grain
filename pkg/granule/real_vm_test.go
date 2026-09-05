package granule_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/granule"
)

// realVMPrereqs gates the one test in this package that boots a VM.
// Everything else here runs against fakes on purpose; this exists
// because those fakes are granule's own idea of what kontur does, and an
// idea of a vsock is not a vsock.
//
// Opt-in rather than "run wherever the host happens to allow it", the
// same shape as pkg/orchestrator's konturDockerRealTestPrereqs: GitHub's
// Linux runners do expose /dev/kvm, so without the variable this would
// also run in the plain go-test job, where it has neither the image nor
// the time budget.
func realVMPrereqs(t *testing.T) string {
	t.Helper()
	if os.Getenv("GRAIN_REAL_VM_TESTS") == "" {
		t.Skip("GRAIN_REAL_VM_TESTS not set; this runs in tests.yml's granule-vm job (set it to 1 to run locally)")
	}
	if runtime.GOOS != "linux" {
		t.Skip("granule boots a cloud-hypervisor VM, which is wired up for Linux hosts only")
	}
	image := os.Getenv("GRAIN_GRANULE_IMAGE")
	if image == "" {
		t.Skip("GRAIN_GRANULE_IMAGE is not set; build it with `make granule-image` and name it here")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not on PATH: %v", err)
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	// Stat rather than open: the container runs privileged and reaches
	// the device as root, which this test process may not be.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm is not present: %v", err)
	}
	return image
}

// The one test that proves granule against a real guest rather than
// against this package's idea of one. It exercises, in a single boot,
// every part of provisioning that a fake cannot honestly stand in for:
//
//   - kontur run as granule's child, and its console reaching the stream
//   - the guest actually coming up, over vsock
//   - a tar unpacked into it, at the guest paths and modes PlanProvision
//     chose -- which the setup script then reads back, so the assertion
//     is the guest's own view of the file rather than granule's
//   - a command exec'd in the guest and its exit code and output
//     surviving back into SetupResult
//   - GuestActivityFile, written by something inside the sandbox and read
//     back out by the shim
//   - one ending, on the stream and as the process's exit code
func TestGranuleProvisionsARealSandbox(t *testing.T) {
	image := realVMPrereqs(t)

	root := t.TempDir()
	placement := filepath.Join(root, "placements", "etc", "grain-test", "token")
	if err := os.MkdirAll(filepath.Dir(placement), 0o755); err != nil {
		t.Fatalf("creating the placement directory: %v", err)
	}
	// A credential-shaped placement, at the default mode, so that what
	// this asserts is the path a real secret takes.
	if err := os.WriteFile(placement, []byte("placement-arrived-intact"), 0o600); err != nil {
		t.Fatalf("writing the placement: %v", err)
	}

	// The setup script is the assertion. It runs inside the guest, so
	// everything it can see is something that genuinely crossed the
	// vsock: the placement's content, its mode, and the client granule
	// installed. Its own exit code is what granule reports.
	setup := `#!/bin/sh
set -eu
mkdir -p /run/grain
echo "provisioning the widget" > ` + granule.GuestActivityFile + `
echo "placement: $(cat /etc/grain-test/token)"
echo "mode: $(stat -c %a /etc/grain-test/token)"
echo "uname: $(uname -s)"
test -f ` + granule.GuestSetupPath + `
`
	if err := os.WriteFile(filepath.Join(root, "setup"), []byte(setup), 0o755); err != nil {
		t.Fatalf("writing the setup script: %v", err)
	}

	args := []string{
		"run", "--rm",
		// Same grants kontur's own docker backend takes: a VMM needs the
		// device, and netshim needs the capability.
		"--privileged", "--device", "/dev/kvm",
		"-e", granule.EnvVersion + "=" + granule.Version,
		"-v", root + ":" + granule.Root + ":ro",
		image,
	}
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the granule container: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatalf("granule did not finish within 10 minutes\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	// Read the stream the way a controller does, before deciding whether
	// the exit code was right: the records say *why*, and a bare exit
	// code from a failed boot is the least useful thing in this test.
	var recs []granule.Record
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var r granule.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// stdout is records only, so an unparseable line is a real
			// defect rather than noise to tolerate.
			t.Errorf("a line on the stream is not a record: %q: %v", line, err)
			continue
		}
		recs = append(recs, r)
	}
	if len(recs) == 0 {
		t.Fatalf("the container produced no records at all\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	var final granule.Status
	var sawConsole bool
	var setupOut string
	activities := map[string]bool{}
	for _, r := range recs {
		switch {
		case r.Src == granule.SrcConsole:
			sawConsole = true
		case r.Kind == granule.KindStatus:
			var st granule.Status
			if err := json.Unmarshal(r.Data, &st); err != nil {
				t.Fatalf("unmarshalling a status record: %v", err)
			}
			final = st
			if st.Activity != "" {
				activities[st.Activity] = true
			}
			if st.Setup != nil {
				setupOut = st.Setup.Output
			}
		}
	}

	if runErr != nil {
		t.Fatalf("granule exited non-zero: %v\nfinal status: %+v\nsetup output:\n%s\nstderr:\n%s",
			runErr, final, setupOut, stderr.String())
	}
	if final.Result == nil {
		t.Fatalf("no terminal status on the stream; last was %+v", final)
	}
	if final.Setup == nil || final.Setup.ExitCode != 0 {
		t.Fatalf("setup did not succeed in the guest: %+v\noutput:\n%s", final.Setup, setupOut)
	}

	// The guest read the placement back, so it landed at the right path
	// with a mode the guest could read -- neither of which granule's own
	// view of the tar could have shown.
	if !strings.Contains(setupOut, "placement: placement-arrived-intact") {
		t.Errorf("the placement did not reach the guest intact:\n%s", setupOut)
	}
	if !strings.Contains(setupOut, "mode: 600") {
		t.Errorf("the placement's mode did not survive the copy (want 600):\n%s", setupOut)
	}
	if !strings.Contains(setupOut, "uname: Linux") {
		t.Errorf("setup did not run in a Linux guest:\n%s", setupOut)
	}
	if !sawConsole {
		t.Error("no console records: the guest's serial output did not reach the stream")
	}
	// Written by the setup script inside the sandbox and read back out
	// by the shim, which is the whole of GuestActivityFile.
	if !activities["provisioning the widget"] {
		t.Errorf("the activity the guest set never reached a status record; saw %v", keysOf(activities))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
