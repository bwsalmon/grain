package ui_test

// grain/task-132: the agent usage-limit pause as an operator sees it --
// GET /api/pause, DELETE /api/pause, and the copy of the same reading
// GET /api/config carries for the banner.

import (
	"net/http"
	"testing"
	"time"
)

// fakePause stands in for *orchestrator.Pause, which pkg/ui cannot
// import (ui.AgentPause's own doc comment). It keeps its own state and
// counts lifts, so a test can assert not just what a read reported but
// that reading left the gate alone -- the property Until exists for.
type fakePause struct {
	until  time.Time
	reason string
	lifts  int
}

func (p *fakePause) Until() (time.Time, string) { return p.until, p.reason }

func (p *fakePause) Lift() bool {
	p.lifts++
	if p.until.IsZero() {
		return false
	}
	p.until, p.reason = time.Time{}, ""
	return true
}

type pauseBody struct {
	Enabled bool `json:"enabled"`
	Pause   *struct {
		Paused           bool      `json:"paused"`
		Until            time.Time `json:"until"`
		Reason           string    `json:"reason"`
		SecondsRemaining float64   `json:"secondsRemaining"`
	} `json:"pause"`
	Lifted *bool `json:"lifted"`
}

func TestGetPauseReportsUnavailableWithNoGateWired(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[pauseBody](t, rec)
	if got.Enabled {
		t.Fatal("enabled = true, want false with no Config.AgentPause")
	}
	if got.Pause != nil {
		t.Fatalf("pause = %+v, want none reported at all", *got.Pause)
	}
}

func TestGetPauseReportsTheCurrentWindow(t *testing.T) {
	srv, client := testServer(t)
	until := baseTime.Add(4 * time.Hour)
	client.Config.AgentPause = &fakePause{until: until, reason: "claude: usage limit reached; resets at " + until.Format(time.RFC3339)}

	rec := do(t, srv, http.MethodGet, "/api/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[pauseBody](t, rec)
	if !got.Enabled || got.Pause == nil || !got.Pause.Paused {
		t.Fatalf("got %+v, want an enabled, paused reading", got)
	}
	if !got.Pause.Until.Equal(until) {
		t.Fatalf("until = %s, want %s", got.Pause.Until, until)
	}
	if got.Pause.SecondsRemaining != (4 * time.Hour).Seconds() {
		t.Fatalf("secondsRemaining = %v, want %v", got.Pause.SecondsRemaining, (4 * time.Hour).Seconds())
	}
	// The provider's own sentence, verbatim: it names the framework and
	// the window, which is the half of this an operator can act on.
	if got.Pause.Reason == "" {
		t.Fatal("reason is empty, want the provider's own sentence")
	}
}

func TestGetPauseReportsAnUnpausedDeployment(t *testing.T) {
	srv, client := testServer(t)
	client.Config.AgentPause = &fakePause{}

	got := decode[pauseBody](t, do(t, srv, http.MethodGet, "/api/pause", ""))
	if !got.Enabled {
		t.Fatal("enabled = false, want true: a gate is wired, it is simply not shut")
	}
	if got.Pause == nil || got.Pause.Paused {
		t.Fatalf("got %+v, want a reading that says nothing is paused", got)
	}
}

// An expired pause is one the reconcile loop has not looked at since its
// window ran out -- only that loop's own Pause.Blocked clears one. It
// reads here as not paused, and this read leaves it exactly as it found
// it: a UI poll that could clear a pause would open the gate out from
// under the loop that owns it.
func TestGetPauseReadsAnExpiredWindowWithoutClearingIt(t *testing.T) {
	srv, client := testServer(t)
	pause := &fakePause{until: baseTime.Add(-time.Minute), reason: "gemini: usage limit reached"}
	client.Config.AgentPause = pause

	got := decode[pauseBody](t, do(t, srv, http.MethodGet, "/api/pause", ""))
	if got.Pause == nil || got.Pause.Paused {
		t.Fatalf("got %+v, want an expired window to read as not paused", got)
	}
	if pause.lifts != 0 {
		t.Fatalf("lifts = %d, want 0: reading a pause must not clear one", pause.lifts)
	}
	if pause.until.IsZero() {
		t.Fatal("the gate itself was cleared by a read; only the reconcile loop may do that")
	}
}

func TestDeletePauseLiftsItAndSaysSo(t *testing.T) {
	srv, client := testServer(t)
	pause := &fakePause{until: baseTime.Add(time.Hour), reason: "claude: usage limit reached"}
	client.Config.AgentPause = pause

	rec := do(t, srv, http.MethodDelete, "/api/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[pauseBody](t, rec)
	if got.Lifted == nil || !*got.Lifted {
		t.Fatalf("lifted = %v, want true", got.Lifted)
	}
	if got.Pause == nil || got.Pause.Paused {
		t.Fatalf("got %+v, want the state left behind to be unpaused", got)
	}

	// Lifting a pause that is not there is not an error: the banner's
	// own button and an expiring window are racing to do the same thing.
	rec = do(t, srv, http.MethodDelete, "/api/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got = decode[pauseBody](t, rec)
	if got.Lifted == nil || *got.Lifted {
		t.Fatalf("lifted = %v on the second call, want false", got.Lifted)
	}
}

func TestDeletePauseIsNotFoundWithNoGateWired(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodDelete, "/api/pause", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// The banner reads this copy rather than polling a second endpoint, so
// the two have to agree -- both come from the same Client.AgentPause.
func TestConfigCarriesThePauseForTheBanner(t *testing.T) {
	srv, client := testServer(t)
	pause := &fakePause{until: baseTime.Add(90 * time.Minute), reason: "claude: usage limit reached"}
	client.Config.AgentPause = pause

	got := decode[struct {
		AgentPause *struct {
			Paused           bool      `json:"paused"`
			Until            time.Time `json:"until"`
			Reason           string    `json:"reason"`
			SecondsRemaining float64   `json:"secondsRemaining"`
		} `json:"agentPause"`
	}](t, do(t, srv, http.MethodGet, "/api/config", ""))
	if got.AgentPause == nil {
		t.Fatal("agentPause is absent from /api/config while dispatch is paused")
	}
	if !got.AgentPause.Paused || got.AgentPause.SecondsRemaining != (90*time.Minute).Seconds() {
		t.Fatalf("agentPause = %+v, want a paused reading 90 minutes from resuming", *got.AgentPause)
	}

	// And nothing at all once it is lifted: a banner draws only when
	// this field is there, so an unpaused deployment must not send one.
	pause.Lift()
	got = decode[struct {
		AgentPause *struct {
			Paused           bool      `json:"paused"`
			Until            time.Time `json:"until"`
			Reason           string    `json:"reason"`
			SecondsRemaining float64   `json:"secondsRemaining"`
		} `json:"agentPause"`
	}](t, do(t, srv, http.MethodGet, "/api/config", ""))
	if got.AgentPause != nil {
		t.Fatalf("agentPause = %+v on an unpaused deployment, want it omitted", *got.AgentPause)
	}
}
