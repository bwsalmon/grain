package ui_test

// The bootstrap's API: the three answers to "where does this
// installation's state live", and the one property that matters most --
// that a UI with no state repository behind it says so rather than
// erroring on every call.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

// fakeStateRepo records what the handler asked for, so a test can assert
// on the call rather than on a real clone.
type fakeStateRepo struct {
	status     ui.StateRepoStatus
	adopted    []string // remote, branch, token of the last Adopt
	usedLocal  bool
	synced     bool
	adoptError error
}

func (f *fakeStateRepo) Status(context.Context) (ui.StateRepoStatus, error) { return f.status, nil }

func (f *fakeStateRepo) UseLocal(context.Context) (ui.StateRepoStatus, error) {
	f.usedLocal = true
	f.status.Mode, f.status.Remote = "local", ""
	return f.status, nil
}

func (f *fakeStateRepo) Adopt(_ context.Context, remote, branch, token string) (ui.StateRepoStatus, error) {
	if f.adoptError != nil {
		return ui.StateRepoStatus{}, f.adoptError
	}
	f.adopted = []string{remote, branch, token}
	f.status.Mode, f.status.Remote, f.status.Branch = "remote", remote, branch
	return f.status, nil
}

func (f *fakeStateRepo) Sync(context.Context) (ui.StateRepoStatus, error) {
	f.synced = true
	return f.status, nil
}

func stateRepoServer(t *testing.T, fake *fakeStateRepo) *ui.Server {
	t.Helper()
	_, client := testServer(t)
	client.Config.StateRepo = fake
	return ui.NewServerWithClient(client)
}

func TestStateRepoStatusReportsWhereStateLives(t *testing.T) {
	fake := &fakeStateRepo{status: ui.StateRepoStatus{
		Mode: "remote", Remote: "https://github.com/owner/grain-state.git", Branch: "main",
		Head: "abc123", SchemaVersion: 16, BuildSchemaVersion: 16,
		SecretsPublicKey: "grain-secret-pub-v1:AAAA", SecretsKeyFile: "/var/lib/grain/secrets/secrets.key",
	}}
	rec := do(t, stateRepoServer(t, fake), http.MethodGet, "/api/state-repo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[ui.StateRepoStatus](t, rec)
	if !got.Available || got.Mode != "remote" || got.Remote == "" {
		t.Fatalf("got %+v", got)
	}
	// The public key is safe to show and is how an operator checks the key
	// they hold is the one this deployment encrypts to. Nothing else about
	// a secret ever comes back through here.
	if got.SecretsPublicKey == "" || got.SecretsKeyFile == "" {
		t.Fatalf("the pane cannot tell an operator which key to back up: %+v", got)
	}
}

func TestStateRepoIsUnavailableWithoutOne(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/api/state-repo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.StateRepoStatus](t, rec); got.Available {
		t.Fatal("a UI with no state repository claimed to have one")
	}
	// A write against a UI that manages none is refused rather than
	// silently accepted.
	if rec := do(t, srv, http.MethodPost, "/api/state-repo", `{"mode":"local"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestBootstrapChoosesLocalOnly(t *testing.T) {
	fake := &fakeStateRepo{status: ui.StateRepoStatus{Mode: "remote", Remote: "https://example.invalid/x.git"}}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo", `{"mode":"local"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !fake.usedLocal {
		t.Fatal("the local choice did not reach the manager")
	}
	if got := decode[ui.StateRepoStatus](t, rec); got.Mode != "local" || got.Remote != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestBootstrapAdoptsARepository(t *testing.T) {
	fake := &fakeStateRepo{}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo",
		`{"mode":"remote","remote":"https://github.com/owner/grain-state.git","branch":"main","token":"ghp_x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := []string{"https://github.com/owner/grain-state.git", "main", "ghp_x"}
	for i, v := range want {
		if len(fake.adopted) != 3 || fake.adopted[i] != v {
			t.Fatalf("adopted %v, want %v", fake.adopted, want)
		}
	}
	// The token is write-only, like every other credential this package
	// handles: it goes to the manager and never comes back in a response.
	if body := rec.Body.String(); strings.Contains(body, "ghp_x") {
		t.Fatalf("the pasted token came back in the response: %s", body)
	}
}

func TestAdoptingNeedsARemote(t *testing.T) {
	fake := &fakeStateRepo{}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo", `{"mode":"remote"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if fake.adopted != nil {
		t.Fatal("an adopt with no remote reached the manager")
	}
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	rec := do(t, stateRepoServer(t, &fakeStateRepo{}), http.MethodPost, "/api/state-repo", `{"mode":"whatever"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestAFailedAdoptSaysWhy(t *testing.T) {
	fake := &fakeStateRepo{adoptError: errors.New("no credential covers owner/grain-state")}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo",
		`{"mode":"remote","remote":"https://github.com/owner/grain-state.git"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no credential covers") {
		t.Fatalf("the failure does not reach the operator: %s", rec.Body)
	}
}

func TestSyncNowGoesThroughTheManager(t *testing.T) {
	fake := &fakeStateRepo{}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo/sync", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !fake.synced {
		t.Fatal("the sync did not reach the manager")
	}
}
