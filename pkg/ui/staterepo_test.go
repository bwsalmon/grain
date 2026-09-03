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
	status      ui.StateRepoStatus
	adopted     *ui.AdoptRequest // the last Adopt, nil if there was none
	importedKey string
	usedLocal   bool
	synced      bool
	adoptError  error
	keyError    error
}

func (f *fakeStateRepo) Status(context.Context) (ui.StateRepoStatus, error) { return f.status, nil }

func (f *fakeStateRepo) UseLocal(context.Context) (ui.StateRepoStatus, error) {
	f.usedLocal = true
	f.status.Mode, f.status.Remote = "local", ""
	return f.status, nil
}

func (f *fakeStateRepo) Adopt(_ context.Context, req ui.AdoptRequest) (ui.StateRepoStatus, error) {
	if f.adoptError != nil {
		return ui.StateRepoStatus{}, f.adoptError
	}
	f.adopted = &req
	f.status.Mode, f.status.Remote, f.status.Branch = "remote", req.Remote, req.Branch
	return f.status, nil
}

func (f *fakeStateRepo) ImportSecretsKey(_ context.Context, key string) (ui.StateRepoStatus, error) {
	if f.keyError != nil {
		return ui.StateRepoStatus{}, f.keyError
	}
	f.importedKey = key
	f.status.SecretsError = ""
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
		`{"mode":"remote","remote":"https://github.com/owner/grain-state.git","branch":"main",`+
			`"token":"ghp_x","secretsKey":"grain-secret-key-v1:AAAA"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := ui.AdoptRequest{
		Remote: "https://github.com/owner/grain-state.git", Branch: "main",
		Token: "ghp_x", SecretsKey: "grain-secret-key-v1:AAAA",
	}
	if fake.adopted == nil || *fake.adopted != want {
		t.Fatalf("adopted %+v, want %+v", fake.adopted, want)
	}
	// Both pasted credentials are write-only, like every other credential
	// this package handles: they go to the manager and never come back in
	// a response.
	for _, secret := range []string{"ghp_x", "grain-secret-key-v1:AAAA"} {
		if body := rec.Body.String(); strings.Contains(body, secret) {
			t.Fatalf("a pasted credential came back in the response: %s", body)
		}
	}
}

// The other half of adopting somebody else's repository: the clone
// brings the sealed file, and this brings the key that opens it.
func TestBootstrapImportsASecretsKey(t *testing.T) {
	fake := &fakeStateRepo{status: ui.StateRepoStatus{
		Mode: "remote", SecretsError: "secrets: this file is encrypted to a different key",
	}}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo/secrets-key",
		`{"key":"grain-secret-key-v1:AAAA"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if fake.importedKey != "grain-secret-key-v1:AAAA" {
		t.Fatalf("the key did not reach the manager: %q", fake.importedKey)
	}
	if body := rec.Body.String(); strings.Contains(body, "grain-secret-key-v1:AAAA") {
		t.Fatalf("the private key came back in the response: %s", body)
	}
	if got := decode[ui.StateRepoStatus](t, rec); got.SecretsError != "" {
		t.Fatalf("the store still reports itself unreadable: %+v", got)
	}
}

func TestAnEmptySecretsKeyIsRefused(t *testing.T) {
	fake := &fakeStateRepo{}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo/secrets-key", `{"key":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if fake.importedKey != "" {
		t.Fatal("an empty key reached the manager")
	}
}

// A key that cannot open the file is refused by the manager, and the
// operator has to be told which one it wanted -- not left with a store
// that fails the next time a run asks it for a credential.
func TestARefusedSecretsKeySaysWhy(t *testing.T) {
	fake := &fakeStateRepo{keyError: errors.New(
		"secrets: this file is encrypted to a different key: secrets.enc is encrypted to grain-secret-pub-v1:BBBB")}
	rec := do(t, stateRepoServer(t, fake), http.MethodPost, "/api/state-repo/secrets-key",
		`{"key":"grain-secret-key-v1:AAAA"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "grain-secret-pub-v1:BBBB") {
		t.Fatalf("the failure does not say which key the file wants: %s", rec.Body)
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
