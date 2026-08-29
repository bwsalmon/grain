package ui_test

// The HTTP surface, over the same real store client_test.go uses. These
// assert on status codes and JSON shape; what the calls actually do to a
// task is client_test.go's job, since Server is a thin shim over Client
// and nothing more.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/ui"
	"github.com/bwsalmon/grain/v2/pkg/upgrade"
)

func testServer(t *testing.T) (*ui.Server, *ui.Client) {
	t.Helper()
	client, _, _ := testClient(t)
	return ui.NewServerWithClient(client), client
}

func do(t *testing.T, srv *ui.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func TestCreateThenListAndGet(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks",
		`{"title":"fix it","description":"please","approved":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Task](t, rec)
	if created.ID == "" {
		t.Fatal("created task came back with no id")
	}
	if created.State != model.StateQueued {
		t.Fatalf("state = %q, want queued", created.State)
	}

	rec = do(t, srv, http.MethodGet, "/api/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	if listed := decode[[]ui.Task](t, rec); len(listed) != 1 {
		t.Fatalf("listed %d tasks, want 1", len(listed))
	}

	rec = do(t, srv, http.MethodGet, "/api/tasks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	if detail := decode[ui.TaskDetail](t, rec); detail.ID != created.ID {
		t.Fatalf("got task %q, want %q", detail.ID, created.ID)
	}
}

func TestGetUnknownTaskIs404(t *testing.T) {
	srv, _ := testServer(t)
	if rec := do(t, srv, http.MethodGet, "/api/tasks/404", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreateRejectionsAre400(t *testing.T) {
	srv, _ := testServer(t)

	for name, body := range map[string]string{
		"empty title":        `{"title":"  "}`,
		"unknown capability": `{"title":"t","capabilities":["nope"]}`,
		"malformed json":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, srv, http.MethodPost, "/api/tasks", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}

// Every mutating route answers with the task as it now stands, so the
// frontend never has to assume its own optimistic update was right.
func TestMutatingRoutesRespondWithTheTask(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it"}`)
	id := decode[ui.Task](t, rec).ID

	// A slice, not a map: these run against one task in sequence, so the
	// expected state after each step depends on the step before it. Map
	// iteration order is randomised, which would make that a coin flip.
	for _, call := range []struct {
		name string
		path string
		body string
		want model.State
	}{
		{name: "approve", path: "/approve", want: model.StateQueued},
		{name: "capability", path: "/capabilities", body: `{"id":"gemini-key","attach":true}`, want: model.StateQueued},
		{name: "comment", path: "/comments", body: `{"body":"hello"}`, want: model.StateQueued},
		{name: "close", path: "/close", want: model.StateClosed},
	} {
		name := call.name
		t.Run(name, func(t *testing.T) {
			rec := do(t, srv, http.MethodPost, "/api/tasks/"+id+call.path, call.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			task := decode[ui.Task](t, rec)
			if task.ID != id {
				t.Fatalf("responded with task %q, want %q", task.ID, id)
			}
			if task.State != call.want {
				t.Fatalf("state = %q, want %q", task.State, call.want)
			}
		})
	}

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+id+"/reopen", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status = %d, want 200", rec.Code)
	}
	if task := decode[ui.Task](t, rec); task.State != model.StateQueued {
		t.Fatalf("state after reopen = %q, want queued", task.State)
	}
}

// TestUpdateRouteEditsFields is handleUpdateTask's own route test --
// PATCH /api/tasks/{id} exists so the CLI's `grain update` (cmdUpdate)
// has an endpoint to call now that it is an HTTPClient rather than a
// direct Client caller; client_test.go's own TestUpdateTask* already
// cover every field-level rule, so this only has to prove the route
// itself is wired to Client.UpdateTask correctly.
func TestUpdateRouteEditsFields(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it"}`)
	id := decode[ui.Task](t, rec).ID

	rec = do(t, srv, http.MethodPatch, "/api/tasks/"+id,
		`{"title":"fix it for real","base":"release","autoMerge":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	task := decode[ui.Task](t, rec)
	if task.Title != "fix it for real" || task.Base != "release" || !task.AutoMerge {
		t.Fatalf("task after update = %+v, want the three fields changed", task)
	}
}

func TestUpdateRouteUnknownTaskIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPatch, "/api/tasks/does-not-exist", `{"title":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestSubmitRouteSetsAutoMerge(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it","approved":true}`)
	id := decode[ui.Task](t, rec).ID

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+id+"/submit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if task := decode[ui.Task](t, rec); !task.AutoMerge {
		t.Fatalf("autoMerge = %v, want true", task.AutoMerge)
	}
}

func TestDependsOnRouteAttachesAndReportsBlocked(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"blocker"}`)
	blockerID := decode[ui.Task](t, rec).ID
	rec = do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it"}`)
	id := decode[ui.Task](t, rec).ID

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+id+"/depends-on",
		`{"id":"`+blockerID+`","attach":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	task := decode[ui.Task](t, rec)
	if !task.Blocked {
		t.Fatal("blocked = false, want true")
	}
	if len(task.DependsOn) != 1 || task.DependsOn[0] != blockerID {
		t.Fatalf("dependsOn = %v, want [%s]", task.DependsOn, blockerID)
	}
}

func TestDependsOnRouteRejectsSelfDependency(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it"}`)
	id := decode[ui.Task](t, rec).ID

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+id+"/depends-on", `{"id":"`+id+`","attach":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestSubmitUnknownTaskIs404(t *testing.T) {
	srv, _ := testServer(t)
	if rec := do(t, srv, http.MethodPost, "/api/tasks/404/submit", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestConfigEndpointReportsActorAndCapabilities(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cfg := decode[struct {
		Actor        string          `json:"actor"`
		Capabilities []ui.Capability `json:"capabilities"`
	}](t, rec)

	if cfg.Actor != "alice" {
		t.Fatalf("actor = %+v, want the configured one", cfg.Actor)
	}
	if len(cfg.Capabilities) != len(ui.DefaultCapabilities()) {
		t.Fatalf("capabilities = %d, want %d", len(cfg.Capabilities), len(ui.DefaultCapabilities()))
	}
	// The GitHub label a capability used to carry is gone from the wire
	// shape along with the labels themselves.
	if strings.Contains(rec.Body.String(), "grain-gemini-key") {
		t.Fatalf("config still reports a GitHub label: %s", rec.Body)
	}
}

func TestSettingsRoutesReadAndWrite(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.Settings](t, rec); got.Configured {
		t.Fatalf("settings = %+v, want Configured false on a fresh store", got)
	}

	rec = do(t, srv, http.MethodPut, "/api/settings",
		`{"pollInterval":"1m","slots":["a","b"],"geminiModel":"gemini-2.5-pro","githubHost":"github.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200: %s", rec.Code, rec.Body)
	}
	saved := decode[ui.Settings](t, rec)
	if !saved.Configured || saved.PollInterval != "1m0s" {
		t.Fatalf("saved settings = %+v", saved)
	}

	rec = do(t, srv, http.MethodGet, "/api/settings", "")
	if got := decode[ui.Settings](t, rec); !reflect.DeepEqual(got, saved) {
		t.Fatalf("get after put = %+v, want %+v", got, saved)
	}
}

func TestSettingsRejectionsAre400(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPut, "/api/settings", `{"pollInterval":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestStaticFrontendIsServed(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// --- secrets ----------------------------------------------------------
//
// bwsalmon/agents#357: a UI colocated with the server can set and delete
// secrets, and list which ones exist, but the API surface never hands a
// value back -- these assert on both halves, the presence of names/keys
// and the total absence of any value anywhere in a response body.

func testServerWithSecrets(t *testing.T) (*ui.Server, *secrets.Store) {
	t.Helper()
	_, store, _ := testClient(t)
	secretStore := secrets.New(t.TempDir())
	cfg := ui.Config{Actor: ui.DefaultActor("alice"), Capabilities: ui.DefaultCapabilities(), Secrets: secretStore}
	return ui.NewServer(cfg, store), secretStore
}

func TestSecretsDisabledByDefault(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/api/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestSecretsDisabledRejectsMutation(t *testing.T) {
	srv, _ := testServer(t)
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/secrets/db/password", `{"value":"hunter2"}`},
		{http.MethodDelete, "/api/secrets/db/password", ""},
		{http.MethodDelete, "/api/secrets/db", ""},
	} {
		rec := do(t, srv, call.method, call.path, call.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404: %s", call.method, call.path, rec.Code, rec.Body)
		}
	}
}

func TestSetSecretThenList(t *testing.T) {
	srv, store := testServerWithSecrets(t)

	rec := do(t, srv, http.MethodPut, "/api/secrets/db/password", `{"value":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("response body leaked the secret value: %s", rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}

	// Confirmed independently of the API surface, which never reads a
	// value back: the value actually landed on disk.
	value, err := store.Resolve(context.Background(), "db/password")
	if err != nil || value != "hunter2" {
		t.Fatalf("Resolve(db/password) = %q, %v", value, err)
	}

	rec = do(t, srv, http.MethodGet, "/api/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("list response leaked the secret value: %s", rec.Body)
	}
	list := decode[map[string]any](t, rec)
	secretsList, _ := list["secrets"].([]any)
	if len(secretsList) != 1 {
		t.Fatalf("secrets = %+v, want one entry", list["secrets"])
	}
}

func TestSetSecretRequiresValue(t *testing.T) {
	srv, _ := testServerWithSecrets(t)
	rec := do(t, srv, http.MethodPut, "/api/secrets/db/password", `{"value":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestSetSecretRejectsTraversalNames(t *testing.T) {
	srv, _ := testServerWithSecrets(t)
	rec := do(t, srv, http.MethodPut, "/api/secrets/..%2Fescape/key", `{"value":"x"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a non-2xx rejection: %s", rec.Code, rec.Body)
	}
}

// --- host reboot --------------------------------------------------------
//
// bwsalmon/agents#395: a human operator's "reboot host" button. Disabled
// by default the same way secrets are (testServer's Config.Reboot is
// nil, the same as a Server nothing has wired one into) -- these assert
// on both halves, /api/config reporting whether it's available and
// POST /api/host/reboot actually calling (or refusing to call) the
// configured func.

func TestRebootDisabledByDefault(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["rebootEnabled"] != false {
		t.Fatalf("rebootEnabled = %v, want false", got["rebootEnabled"])
	}

	rec = do(t, srv, http.MethodPost, "/api/host/reboot", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestRebootCallsConfiguredFunc(t *testing.T) {
	client, _, _ := testClient(t)
	calls := 0
	client.Config.Reboot = func(ctx context.Context) error {
		calls++
		return nil
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["rebootEnabled"] != true {
		t.Fatalf("rebootEnabled = %v, want true", got["rebootEnabled"])
	}

	rec = do(t, srv, http.MethodPost, "/api/host/reboot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRebootSurfacesError(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.Reboot = func(ctx context.Context) error {
		return errors.New("sudo: a password is required")
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodPost, "/api/host/reboot", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "a password is required") {
		t.Fatalf("body = %s, want the underlying error message", rec.Body)
	}
}

func TestDeleteSecretKeyLeavesOtherKeys(t *testing.T) {
	srv, store := testServerWithSecrets(t)
	if err := store.Set("db", "username", []byte("app")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodDelete, "/api/secrets/db/password", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if _, err := store.Resolve(context.Background(), "db/password"); err == nil {
		t.Fatal("expected password to be gone")
	}
	if v, err := store.Resolve(context.Background(), "db/username"); err != nil || v != "app" {
		t.Fatalf("username should survive: %q, %v", v, err)
	}
}

func TestDeleteWholeSecret(t *testing.T) {
	srv, store := testServerWithSecrets(t)
	if err := store.Set("db", "username", []byte("app")); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodDelete, "/api/secrets/db", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("got %+v, want no secrets left", list)
	}
}

func TestDeleteMissingSecretIs404(t *testing.T) {
	srv, _ := testServerWithSecrets(t)
	rec := do(t, srv, http.MethodDelete, "/api/secrets/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// --- upgrade ------------------------------------------------------------
//
// bwsalmon/agents#396: fakeUpgrader stands in for a real
// *upgrade.Upgrader (which would need a real git checkout, docker
// daemon, and restart command behind it) so these can assert on the
// route wiring -- disabled-by-default, the branch/status shape, and
// error mapping -- without any of that. pkg/upgrade's own tests already
// cover the checkout/build/install/restart pipeline itself.

type fakeUpgrader struct {
	startErr error
	status   upgrade.Status
	started  string
}

func (f *fakeUpgrader) Start(branch string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = branch
	f.status.Branch = branch
	f.status.Phase = upgrade.PhaseRunning
	return nil
}

func (f *fakeUpgrader) Status() (upgrade.Status, error) { return f.status, nil }

func testServerWithUpgrader(t *testing.T, up ui.Upgrader) *ui.Server {
	t.Helper()
	_, store, _ := testClient(t)
	cfg := ui.Config{Actor: ui.DefaultActor("alice"), Capabilities: ui.DefaultCapabilities(), Upgrader: up}
	return ui.NewServer(cfg, store)
}

func TestUpgradeDisabledByDefault(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/upgrade", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}

	rec = do(t, srv, http.MethodPost, "/api/upgrade", `{"branch":"main"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestStartUpgradeRoute(t *testing.T) {
	fake := &fakeUpgrader{}
	srv := testServerWithUpgrader(t, fake)

	rec := do(t, srv, http.MethodPost, "/api/upgrade", `{"branch":"grain/issue-396"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if fake.started != "grain/issue-396" {
		t.Fatalf("Start was called with %q, want %q", fake.started, "grain/issue-396")
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != true || got["branch"] != "grain/issue-396" || got["phase"] != "running" {
		t.Fatalf("response = %+v", got)
	}

	rec = do(t, srv, http.MethodGet, "/api/upgrade", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got = decode[map[string]any](t, rec)
	if got["phase"] != "running" {
		t.Fatalf("status phase = %v, want running", got["phase"])
	}
}

func TestStartUpgradeRequiresBranch(t *testing.T) {
	srv := testServerWithUpgrader(t, &fakeUpgrader{})
	rec := do(t, srv, http.MethodPost, "/api/upgrade", `{"branch":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestStartUpgradeRejectsWhenAlreadyRunning(t *testing.T) {
	fake := &fakeUpgrader{startErr: upgrade.ErrUpgradeInProgress}
	srv := testServerWithUpgrader(t, fake)
	rec := do(t, srv, http.MethodPost, "/api/upgrade", `{"branch":"main"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
