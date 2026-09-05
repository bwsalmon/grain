package granule_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/granule"
)

// records parses a stream back the way a controller reads one.
func records(t *testing.T, out string) []granule.Record {
	t.Helper()
	var recs []granule.Record
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var r granule.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a line on the stream is not a record: %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// lastStatus is what a controller that joined the stream late would see.
func lastStatus(t *testing.T, recs []granule.Record) granule.Status {
	t.Helper()
	var st granule.Status
	found := false
	for _, r := range recs {
		if r.Kind != granule.KindStatus {
			continue
		}
		if err := json.Unmarshal(r.Data, &st); err != nil {
			t.Fatalf("unmarshalling a status record: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("the stream carries no status record at all")
	}
	return st
}

func testConfig(t *testing.T, root string) granule.Config {
	t.Helper()
	return granule.Config{
		Root:           root,
		TerminationLog: filepath.Join(t.TempDir(), "termination-log"),
		ReadyTimeout:   time.Minute,
	}
}

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// The whole provisioning path, with no agent: boot, wait, unpack, setup,
// and an ending. Every one of those has to show up on the stream,
// because the stream is the only thing a controller reads.
func TestAProvisionedGrainNarratesItselfAndEnds(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version, granule.EnvFramework: "claude"})
	root := mountedTree(t, map[string]string{
		"setup":                        "#!/bin/sh\n",
		"placements/home/agent/.netrc": "machine github.com",
	}, map[string]os.FileMode{"setup": 0o755})

	guest := &fakeGuest{}
	vmm := &fakeVMM{consoleMsg: "[    0.000000] Linux version 6.1\n"}
	var out bytes.Buffer
	cfg := testConfig(t, root)

	code := granule.Run(context.Background(), cfg, granule.Deps{VMM: vmm, Guest: guest}, granule.NewStream(&out, nil))
	if code != granule.ExitOK {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if !vmm.started || !vmm.shutdown {
		t.Errorf("vmm started=%v shutdown=%v, want both", vmm.started, vmm.shutdown)
	}
	if len(guest.unpacked) != 1 {
		t.Errorf("unpacked %d tars, want 1", len(guest.unpacked))
	}
	if !guest.ranSetup() {
		t.Errorf("setup was not run: %v", guest.execs)
	}

	recs := records(t, out.String())
	st := lastStatus(t, recs)
	if !st.Phase.Terminal() {
		t.Errorf("final phase = %q, want terminal", st.Phase)
	}
	if st.Result == nil {
		t.Fatal("the final status carries no Result")
	}
	if st.Setup == nil || st.Setup.ExitCode != 0 {
		t.Errorf("setup result = %+v", st.Setup)
	}

	// The console shares the stream and is tagged, which is what makes a
	// failed boot quotable.
	var sawConsole, sawProvisioning bool
	for _, r := range recs {
		if r.Src == granule.SrcConsole && strings.Contains(string(r.Data), "Linux version") {
			sawConsole = true
		}
		if r.Kind == granule.KindStatus && strings.Contains(string(r.Data), string(granule.PhaseProvisioning)) {
			sawProvisioning = true
		}
	}
	if !sawConsole {
		t.Error("the guest console did not reach the stream as records")
	}
	if !sawProvisioning {
		t.Error("no provisioning status was emitted before the ending")
	}

	// Seq is monotonic from 1, which is the cursor a controller pages by.
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
			break
		}
		if r.Version != granule.Version {
			t.Errorf("record %d carries version %q", i, r.Version)
		}
	}
}

// A failed setup ends the grain before an agent runs, which is what
// makes a bad checkout cost no model tokens.
func TestAFailedSetupEndsTheGrainWithoutRunningAnAgent(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	root := mountedTree(t, map[string]string{"setup": "#!/bin/sh\nexit 3\n"},
		map[string]os.FileMode{"setup": 0o755})

	guest := &fakeGuest{exitCode: 3, execOut: "fatal: repository not found\n"}
	var out bytes.Buffer
	agentRan := false

	code := granule.Run(context.Background(), testConfig(t, root), granule.Deps{
		VMM: &fakeVMM{}, Guest: guest,
		Agent: func(context.Context, granule.Spec, granule.Guest, io.Writer) (granule.Result, error) {
			agentRan = true
			return granule.Result{}, nil
		},
	}, granule.NewStream(&out, nil))

	if agentRan {
		t.Fatal("the agent ran after setup failed")
	}
	if code != granule.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, granule.ExitFailed)
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil || st.Result.Outcome != granule.OutcomeSetupFailed {
		t.Fatalf("result = %+v, want %s", st.Result, granule.OutcomeSetupFailed)
	}
	// The diagnosis is the script's own output, uninterpreted.
	if st.Setup == nil || !strings.Contains(st.Setup.Output, "repository not found") {
		t.Errorf("setup output did not survive: %+v", st.Setup)
	}
}

// An environment written to a wire this build does not speak is refused
// before anything boots, with its own exit code -- because a generic
// failure is indistinguishable from a bad setup script and the two want
// different responses.
func TestAnUnknownWireVersionIsRefusedBeforeBooting(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: "v99"})
	vmm := &fakeVMM{}
	var out bytes.Buffer

	code := granule.Run(context.Background(), testConfig(t, t.TempDir()),
		granule.Deps{VMM: vmm, Guest: &fakeGuest{}}, granule.NewStream(&out, nil))

	if code != granule.ExitWireVersion {
		t.Errorf("exit code = %d, want %d", code, granule.ExitWireVersion)
	}
	if vmm.started {
		t.Error("the VMM was started despite an unreadable environment")
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil || !strings.Contains(st.Result.Detail, "v99") {
		t.Errorf("the failure does not name the version it got: %+v", st.Result)
	}
}

// The one read that must not be missed. A controller that lost the
// stream to rotation still finds the ending in the pod listing.
func TestTheResultAlsoLandsInTheTerminationLog(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	cfg := testConfig(t, t.TempDir())
	var out bytes.Buffer

	granule.Run(context.Background(), cfg, granule.Deps{VMM: &fakeVMM{}, Guest: &fakeGuest{}},
		granule.NewStream(&out, nil))

	body, err := os.ReadFile(cfg.TerminationLog)
	if err != nil {
		t.Fatalf("reading the termination log: %v", err)
	}
	var got granule.Result
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the termination log is not a Result: %q: %v", body, err)
	}
	if got.Outcome == "" {
		t.Errorf("the termination log carries no outcome: %+v", got)
	}
}

// Cancellation is how a grain is stopped, and the shim's window is for
// writing an ending -- not for exiting silently.
func TestACancelledGrainStillWritesAnEnding(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer

	code := granule.Run(ctx, testConfig(t, t.TempDir()), granule.Deps{
		VMM: &fakeVMM{}, Guest: &fakeGuest{},
		Agent: func(ctx context.Context, _ granule.Spec, _ granule.Guest, _ io.Writer) (granule.Result, error) {
			cancel()
			<-ctx.Done()
			return granule.Result{}, ctx.Err()
		},
	}, granule.NewStream(&out, nil))

	if code != granule.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, granule.ExitFailed)
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil {
		t.Fatal("a cancelled grain wrote no Result")
	}
}

// MaxRuntime is the grain's own bound, so it has to hold with no
// controller involved at all.
func TestMaxRuntimeEndsTheGrainItself(t *testing.T) {
	withEnv(t, map[string]string{
		granule.EnvVersion:    granule.Version,
		granule.EnvMaxRuntime: "50ms",
	})
	var out bytes.Buffer

	code := granule.Run(context.Background(), testConfig(t, t.TempDir()), granule.Deps{
		VMM: &fakeVMM{}, Guest: &fakeGuest{},
		Agent: func(ctx context.Context, _ granule.Spec, _ granule.Guest, _ io.Writer) (granule.Result, error) {
			<-ctx.Done()
			return granule.Result{}, ctx.Err()
		},
	}, granule.NewStream(&out, nil))

	if code != granule.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, granule.ExitFailed)
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil || !strings.Contains(st.Result.Detail, "GRAIN_MAX_RUNTIME") {
		t.Fatalf("result does not name the bound it hit: %+v", st.Result)
	}
}

// A guest that never comes up is a grain that ends, not one that hangs:
// the container is the only thing holding the run, so hanging here is a
// slot nothing frees until the controller's own budget notices.
func TestAGuestThatNeverComesUpEndsTheGrain(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	cfg := testConfig(t, t.TempDir())
	cfg.ReadyTimeout = 10 * time.Millisecond
	var out bytes.Buffer

	code := granule.Run(context.Background(), cfg, granule.Deps{
		VMM: &fakeVMM{}, Guest: &fakeGuest{readyAfter: 1 << 30},
	}, granule.NewStream(&out, nil))

	if code != granule.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, granule.ExitFailed)
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil || st.Result.Outcome != granule.OutcomeSetupFailed {
		t.Fatalf("result = %+v", st.Result)
	}
}

// A VMM that will not start is reported, not retried: rebuilding is the
// grain's business but a VMM that cannot start once is not a guest that
// went wrong mid-run.
func TestAVMMThatWillNotStartIsReported(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	var out bytes.Buffer

	code := granule.Run(context.Background(), testConfig(t, t.TempDir()), granule.Deps{
		VMM: &fakeVMM{startErr: errors.New("no /dev/kvm")}, Guest: &fakeGuest{},
	}, granule.NewStream(&out, nil))

	if code != granule.ExitFailed {
		t.Errorf("exit code = %d, want %d", code, granule.ExitFailed)
	}
	st := lastStatus(t, records(t, out.String()))
	if st.Result == nil || !strings.Contains(st.Result.Detail, "/dev/kvm") {
		t.Fatalf("result does not carry the reason: %+v", st.Result)
	}
}

// The whole point of GuestActivityFile: something inside the sandbox --
// a setup script, a build the agent is blocked on -- sets the phrase a
// human reads on a task row, and the shim picks it up on the round trip
// it already makes.
func TestTheGuestCanSetTheActivityAGrainReports(t *testing.T) {
	withEnv(t, map[string]string{granule.EnvVersion: granule.Version})
	guest := &fakeGuest{activity: "cloning acme/widgets\n"}
	cfg := testConfig(t, t.TempDir())
	cfg.Heartbeat = 5 * time.Millisecond
	var out bytes.Buffer

	granule.Run(context.Background(), cfg, granule.Deps{
		VMM: &fakeVMM{}, Guest: guest,
		Agent: func(ctx context.Context, _ granule.Spec, _ granule.Guest, _ io.Writer) (granule.Result, error) {
			// Long enough for a few heartbeats, which is the only path
			// that reads the file.
			time.Sleep(60 * time.Millisecond)
			return granule.Result{Outcome: granule.OutcomeSucceeded}, nil
		},
	}, granule.NewStream(&out, nil))

	var saw bool
	for _, r := range records(t, out.String()) {
		if r.Kind != granule.KindStatus {
			continue
		}
		var st granule.Status
		if err := json.Unmarshal(r.Data, &st); err != nil {
			t.Fatalf("unmarshalling a status: %v", err)
		}
		if st.Activity == "cloning acme/widgets" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("the guest's activity never reached a status record:\n%s", out.String())
	}
}
