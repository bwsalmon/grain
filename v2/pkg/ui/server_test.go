package ui_test

// The HTTP surface, over the same real store client_test.go uses. These
// assert on status codes and JSON shape; what the calls actually do to a
// task is client_test.go's job, since Server is a thin shim over Client
// and nothing more.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
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

func TestConfigEndpointReportsActorAndCapabilities(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cfg := decode[struct {
		Actor        model.Principal `json:"actor"`
		Capabilities []ui.Capability `json:"capabilities"`
	}](t, rec)

	if cfg.Actor.ID != "alice" {
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

func TestStaticFrontendIsServed(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
