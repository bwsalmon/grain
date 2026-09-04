package main

// `grain pause`'s own rendering and dispatch (grain/task-182). The wire
// calls behind it are pkg/ui's (pause_test.go's HTTPClient round trips);
// what is only decidable here is what an operator at a terminal actually
// reads -- in particular that a daemon serving a UI with no gate wired
// does not print anything anyone could mistake for "nothing is paused".

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
)

// fakePause stands in for *orchestrator.Pause, the gate cmd/grain/daemon.go
// hands the UI in a real deployment -- ui.AgentPause is satisfied
// structurally, so a test needs nothing more than these two methods.
type fakePause struct {
	until  time.Time
	reason string
}

func (p *fakePause) Until() (time.Time, string) { return p.until, p.reason }

func (p *fakePause) Lift() bool {
	if p.until.IsZero() {
		return false
	}
	p.until, p.reason = time.Time{}, ""
	return true
}

func TestPrintAgentPause(t *testing.T) {
	until := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)
	paused := ui.AgentPauseStatus{
		Paused: true, Until: until, SecondsRemaining: (2 * time.Hour).Seconds(),
		Reason: "claude: usage limit reached; resets at " + until.Format(time.RFC3339),
	}
	lifted, notLifted := true, false
	for _, tc := range []struct {
		name    string
		status  ui.AgentPauseStatus
		enabled bool
		lifted  *bool
		want    []string
		wantNot []string
	}{
		{
			name:    "a paused deployment says until when, and what the provider said",
			status:  paused,
			enabled: true,
			want: []string{
				"dispatch is paused", "2026-09-04T17:00:00Z", "2h0m0s",
				"claude: usage limit reached", "grain pause -lift",
			},
		},
		{
			name:    "an unpaused deployment says dispatch is running",
			enabled: true,
			want:    []string{"dispatch is running"},
			// Nothing to suggest lifting: there is no pause to lift.
			wantNot: []string{"-lift", "paused until"},
		},
		{
			// The case this whole command has to get right: a daemon that
			// cannot see the gate must not answer for it. "Nothing is
			// paused" from here is exactly what an operator would act on
			// to rule the usage limit out.
			name:    "a deployment with no gate wired says so instead",
			wantNot: []string{"dispatch is running", "dispatch is paused"},
			want:    []string{"not wired"},
		},
		{
			name:    "a lift that ended a pause says so",
			enabled: true,
			lifted:  &lifted,
			want:    []string{"pause lifted", "next reconcile tick"},
			// Already said what it did; repeating the advice to lift it
			// at somebody who just did would read as it not having worked.
			wantNot: []string{"grain pause -lift"},
		},
		{
			name:    "a lift with nothing to lift says that rather than claiming one",
			enabled: true,
			lifted:  &notLifted,
			want:    []string{"nothing to lift"},
			wantNot: []string{"pause lifted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				(&printer{}).agentPause(tc.status, tc.enabled, tc.lifted)
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not mention %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(out, unwanted) {
					t.Errorf("output mentions %q, and should not:\n%s", unwanted, out)
				}
			}
		})
	}
}

// -json prints the route's own body, so a reading piped somewhere else
// and one fetched with curl are the same object -- including the
// enabled/pause split, which is the part a script has to branch on.
func TestPrintAgentPauseJSON(t *testing.T) {
	status := ui.AgentPauseStatus{
		Paused: true, Until: time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC),
		SecondsRemaining: 7200, Reason: "claude: usage limit reached",
	}
	out := captureStdout(t, func() { (&printer{json: true}).agentPause(status, true, nil) })
	var got agentPauseView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if !got.Enabled || got.Pause == nil || !got.Pause.Paused || got.Pause.SecondsRemaining != 7200 {
		t.Fatalf("got %+v, want an enabled, paused reading two hours from resuming", got)
	}
	if got.Lifted != nil {
		t.Errorf("lifted = %v on a plain read, want it omitted", *got.Lifted)
	}

	out = captureStdout(t, func() { (&printer{json: true}).agentPause(ui.AgentPauseStatus{}, false, nil) })
	got = agentPauseView{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if got.Enabled || got.Pause != nil {
		t.Fatalf("got %+v, want enabled false and no reading at all", got)
	}
}

// pauseServer starts a real daemon-shaped UI/API over an embedded store
// with gate wired, and returns its URL along with the gate itself.
func pauseServer(t *testing.T, pause ui.AgentPause) string {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	client := ui.NewClient(ui.Config{
		Actor:        ui.DefaultActor("tester"),
		Capabilities: ui.OfferedCapabilities(),
		AgentPause:   pause,
	}, store)
	srv := httptest.NewServer(ui.NewServerWithClient(client))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The dispatch itself, against a real server: every test above would
// still pass if runCLI's switch never reached cmdPause at all.
func TestRunCLIPauseReadsAndLifts(t *testing.T) {
	pause := &fakePause{until: time.Now().Add(time.Hour), reason: "claude: usage limit reached"}
	server := pauseServer(t, pause)

	out := captureStdout(t, func() {
		if err := runCLI([]string{"-server", server, "pause"}); err != nil {
			t.Fatalf("grain pause: %v", err)
		}
	})
	if !strings.Contains(out, "dispatch is paused") || !strings.Contains(out, "claude: usage limit reached") {
		t.Fatalf("`grain pause` printed %q, want the paused reading and the provider's sentence", out)
	}
	if pause.until.IsZero() {
		t.Fatal("`grain pause` cleared the gate; only the reconcile loop and an explicit -lift may do that")
	}

	out = captureStdout(t, func() {
		if err := runCLI([]string{"-server", server, "pause", "-lift"}); err != nil {
			t.Fatalf("grain pause -lift: %v", err)
		}
	})
	if !strings.Contains(out, "pause lifted") {
		t.Fatalf("`grain pause -lift` printed %q, want it saying the pause was lifted", out)
	}
	if !pause.until.IsZero() {
		t.Fatal("`grain pause -lift` left the gate shut")
	}

	out = captureStdout(t, func() {
		if err := runCLI([]string{"-json", "-server", server, "pause"}); err != nil {
			t.Fatalf("grain -json pause: %v", err)
		}
	})
	var got agentPauseView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if !got.Enabled || got.Pause == nil || got.Pause.Paused {
		t.Fatalf("got %+v, want an enabled reading of a deployment that is no longer paused", got)
	}
}

// A daemon serving a UI with no gate behind it: a read says it has been
// told nothing, and a lift -- an action that would otherwise report
// success for having done nothing at all -- fails.
func TestRunCLIPauseWithNoGateWired(t *testing.T) {
	server := pauseServer(t, nil)

	out := captureStdout(t, func() {
		if err := runCLI([]string{"-server", server, "pause"}); err != nil {
			t.Fatalf("grain pause: %v", err)
		}
	})
	if strings.Contains(out, "dispatch is running") {
		t.Fatalf("`grain pause` printed %q, which reads as an answer about a gate it never saw", out)
	}

	if err := runCLI([]string{"-server", server, "pause", "-lift"}); err == nil {
		t.Error("`grain pause -lift` against a deployment with no gate returned no error")
	}
}
