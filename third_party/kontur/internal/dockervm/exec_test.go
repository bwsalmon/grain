package dockervm

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// execCall returns the single "docker exec" invocation recorded, failing
// the test if there isn't exactly one.
func execCall(t *testing.T, calls [][]string) []string {
	t.Helper()
	var found [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "exec" {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d docker exec calls, want 1: %v", len(found), calls)
	}
	return found[0]
}

func TestExec_RunsKonturExecInTheVMContainer(t *testing.T) {
	d, calls := testDocker(t)

	var stdout bytes.Buffer
	code, err := Exec(context.Background(), d, "web", ExecOptions{
		Command: []string{"systemctl", "is-active", "nginx"},
		Stdin:   strings.NewReader("hello"),
		Stdout:  &stdout,
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Exec error = %v", err)
	}
	if code != 0 {
		t.Errorf("Exec status = %d, want 0", code)
	}

	call := execCall(t, calls())
	want := []string{"exec", "-i", "kontur-vm-web", "kontur", "exec", "--", "systemctl", "is-active", "nginx"}
	if strings.Join(call, " ") != strings.Join(want, " ") {
		t.Errorf("docker call = %v, want %v", call, want)
	}
	// fakedocker's "exec" copies stdin to stdout, so this is the session's
	// stdin proxying (the README's Flow 4) reaching the guest command.
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q, want the stdin that was passed through", stdout.String())
	}
}

func TestExec_TTYAndEnv(t *testing.T) {
	d, calls := testDocker(t)

	if _, err := Exec(context.Background(), d, "web", ExecOptions{
		TTY:    true,
		Env:    []string{"KONTUR_EXEC_USER=app", "KONTUR_EXEC_CONNECT_TIMEOUT=3m"},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}); err != nil {
		t.Fatalf("Exec error = %v", err)
	}

	call := execCall(t, calls())
	// No command and no "--": that is what asks "kontur exec" for an
	// interactive login shell rather than for one command.
	want := []string{"exec", "-i", "-t", "-e", "KONTUR_EXEC_USER=app", "-e", "KONTUR_EXEC_CONNECT_TIMEOUT=3m", "kontur-vm-web", "kontur", "exec"}
	if strings.Join(call, " ") != strings.Join(want, " ") {
		t.Errorf("docker call = %v, want %v", call, want)
	}
}

// TestExec_ReportsTheCommandsOwnStatus covers the difference "vm exec"
// exists to preserve: a guest command that exits non-zero is a status to
// pass on, not an error.
func TestExec_ReportsTheCommandsOwnStatus(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_EXEC_EXIT", "42")

	code, err := Exec(context.Background(), d, "web", ExecOptions{
		Command: []string{"false"},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Exec error = %v, want the status reported instead", err)
	}
	if code != 42 {
		t.Errorf("Exec status = %d, want 42", code)
	}
}

func TestExec_VMNotRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(t *testing.T)
	}{
		{"no such container", func(t *testing.T) { t.Setenv("FAKEDOCKER_MISSING", "kontur-vm-web") }},
		{"container exited", func(t *testing.T) { t.Setenv("FAKEDOCKER_RUNNING", "false") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := testDocker(t)
			tc.env(t)

			_, err := Exec(context.Background(), d, "web", ExecOptions{
				Command: []string{"true"},
				Stdout:  io.Discard,
				Stderr:  io.Discard,
			})
			if err == nil {
				t.Fatal("Exec error = nil, want one naming the VM")
			}
			if !strings.Contains(err.Error(), "kontur-vm-web") {
				t.Errorf("error = %v, want it to name the container", err)
			}
			// Nothing was exec'd: docker's own 125/126/127 must not be
			// mistaken for the guest command's status.
			for _, c := range calls() {
				if len(c) > 0 && c[0] == "exec" {
					t.Errorf("docker exec was attempted against a VM that isn't running: %v", c)
				}
			}
		})
	}
}

func TestWaitReady_RetriesUntilTheGuestAnswers(t *testing.T) {
	d, calls := testDocker(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "2")

	if err := WaitReady(context.Background(), d, "web", 30*time.Second); err != nil {
		t.Fatalf("WaitReady error = %v", err)
	}

	execs := 0
	for _, c := range calls() {
		if len(c) > 0 && c[0] == "exec" {
			execs++
			// The probe is "kontur ready", not a hand-rolled command of
			// this package's own: what counts as ready is decided in one
			// place, guest-side, and shared with the container readiness
			// probe a pod spec runs.
			if got := strings.Join(c, " "); !strings.HasSuffix(got, "kontur ready -timeout 0") {
				t.Errorf("readiness probe ran %q, want it to end in %q", got, "kontur ready -timeout 0")
			}
		}
	}
	if execs != 3 {
		t.Errorf("got %d readiness probes, want 3 (two refused, then one that answered)", execs)
	}
}

// An image without a "ready" mode cannot be probed however long anyone
// waits, so it has to be reported at once and named for what it is,
// rather than looking like a guest that never finished booting.
func TestWaitReady_ImageWithoutTheReadyMode(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_NO_READY_MODE", "1")

	start := time.Now()
	err := WaitReady(context.Background(), d, "web", time.Hour)
	if err == nil {
		t.Fatal("WaitReady error = nil, want one")
	}
	if !strings.Contains(err.Error(), "no \"kontur ready\" mode") {
		t.Errorf("error = %v, want it to name the missing mode", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %v to give up on something no wait can fix", elapsed)
	}
}

// TestInspect_ReportsReadinessWithoutWaiting is the question "vm wait"
// can only answer by blocking: asked once, answered now, whichever way.
func TestInspect_ReportsReadinessWithoutWaiting(t *testing.T) {
	d, _ := testDocker(t)

	st, err := Inspect(context.Background(), d, "web")
	if err != nil {
		t.Fatalf("Inspect error = %v", err)
	}
	if st.Container != "kontur-vm-web" {
		t.Errorf("Container = %q, want kontur-vm-web", st.Container)
	}
	if !st.Running || !st.Ready {
		t.Errorf("status = %+v, want a running container with a guest that answered", st)
	}
}

func TestInspect_NotReadyCarriesTheProbesOwnReason(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")

	st, err := Inspect(context.Background(), d, "web")
	// A guest that isn't up is a status, not a failure to ask.
	if err != nil {
		t.Fatalf("Inspect error = %v, want the status reported instead", err)
	}
	if !st.Running || st.Ready {
		t.Errorf("status = %+v, want a running container whose guest has not answered", st)
	}
	if !strings.Contains(st.Detail, "not reachable yet") {
		t.Errorf("Detail = %q, want the probe's own message", st.Detail)
	}
}

func TestInspect_ContainerNotRunning(t *testing.T) {
	d, calls := testDocker(t)
	t.Setenv("FAKEDOCKER_MISSING", "kontur-vm-web")

	st, err := Inspect(context.Background(), d, "web")
	if err != nil {
		t.Fatalf("Inspect error = %v", err)
	}
	if st.Running || st.Ready {
		t.Errorf("status = %+v, want neither running nor ready", st)
	}
	// Nothing to probe through: an exec against a container that isn't
	// there reports docker's failure, not the guest's.
	for _, c := range calls() {
		if len(c) > 0 && c[0] == "exec" {
			t.Errorf("probed a container that isn't running: %v", c)
		}
	}
}

// Each probe has to give up dialling well inside the wait's own budget:
// left to retry for the 30 seconds "kontur exec" gives a dial by
// default, one probe outlasts any shorter timeout the caller asked for
// and delays the container-exited check with it. "-timeout 0" is what
// keeps that from happening (see readyProbeArgs), so it is asserted
// rather than left to the argv comparison above.
func TestWaitReady_ProbesGiveUpDiallingBeforeTheWaitDoes(t *testing.T) {
	d, calls := testDocker(t)

	if err := WaitReady(context.Background(), d, "web", 30*time.Second); err != nil {
		t.Fatalf("WaitReady error = %v", err)
	}

	call := execCall(t, calls())
	if !slices.Contains(call, "-timeout") || call[len(call)-1] != "0" {
		t.Errorf("readiness probe ran %v, want it to ask for a single attempt (-timeout 0)", call)
	}
}

// TestWaitReady_ContainerExited covers the failure worth failing fast on:
// a VM whose container died is not going to become reachable, however
// long the timeout is.
func TestWaitReady_ContainerExited(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")
	t.Setenv("FAKEDOCKER_RUNNING", "false")

	err := WaitReady(context.Background(), d, "web", time.Hour)
	if err == nil {
		t.Fatal("WaitReady error = nil, want one")
	}
	if !strings.Contains(err.Error(), "exited before its guest became reachable") {
		t.Errorf("error = %v, want it to say the container exited", err)
	}
}

func TestWaitReady_Timeout(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")

	err := WaitReady(context.Background(), d, "web", time.Nanosecond)
	if err == nil {
		t.Fatal("WaitReady error = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
}
