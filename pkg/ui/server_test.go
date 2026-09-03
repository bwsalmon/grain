package ui_test

// The HTTP surface, over the same real store client_test.go uses. These
// assert on status codes and JSON shape; what the calls actually do to a
// task is client_test.go's job, since Server is a thin shim over Client
// and nothing more.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/ui"
	"github.com/bwsalmon/grain/pkg/upgrade"
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

// TestAttachmentRoutesRoundTripThroughCreateCommentAndDownload is the HTTP
// surface for bwsalmon/agents#522: a base64 upload on POST /api/tasks
// lands as metadata on GET /api/tasks/{id} (never content -- Attachment's
// own doc comment), one on POST .../comments lands on that comment
// instead, and GET .../attachments/{attachmentId} is the one route that
// serves an attachment's actual bytes back, with its own content type.
func TestAttachmentRoutesRoundTripThroughCreateCommentAndDownload(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{
		"title":"fix it","approved":true,
		"attachments":[{"filename":"repro.zip","contentType":"application/zip","content":"UEsDBA=="}]
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Task](t, rec)

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+created.ID+"/comments", `{
		"attachments":[{"filename":"screenshot.png","contentType":"image/png","content":"ZmFrZQ=="}]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("comment status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodGet, "/api/tasks/"+created.ID, "")
	detail := decode[ui.TaskDetail](t, rec)
	if len(detail.Attachments) != 1 || detail.Attachments[0].Filename != "repro.zip" {
		t.Fatalf("task attachments = %+v, want one repro.zip", detail.Attachments)
	}
	if len(detail.Comments) != 1 || len(detail.Comments[0].Attachments) != 1 ||
		detail.Comments[0].Attachments[0].Filename != "screenshot.png" {
		t.Fatalf("comment attachments = %+v, want one screenshot.png", detail.Comments)
	}

	rec = do(t, srv, http.MethodGet,
		fmt.Sprintf("/api/tasks/%s/attachments/%d", created.ID, detail.Attachments[0].ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("content type = %q, want application/zip", got)
	}
	if rec.Body.String() != "PK\x03\x04" {
		t.Errorf("body = %q, want the decoded attachment bytes", rec.Body.String())
	}
}

func TestAttachmentDownloadUnknownIDIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it","approved":true}`)
	created := decode[ui.Task](t, rec)

	rec = do(t, srv, http.MethodGet, "/api/tasks/"+created.ID+"/attachments/999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// TestReorderRoute is the HTTP surface for Client.Reorder
// (client_test.go's TestReorderMovesATaskInTheBacklog already covers what
// it does to the store): POST /api/tasks/reorder, a literal path segment
// that has to win over the PATCH /api/tasks/{id} wildcard registered at
// the same depth rather than being swallowed by it.
func TestReorderRoute(t *testing.T) {
	srv, _ := testServer(t)

	var ids [3]string
	for i := range ids {
		rec := do(t, srv, http.MethodPost, "/api/tasks",
			`{"title":"t","approved":true}`)
		ids[i] = decode[ui.Task](t, rec).ID
	}

	rec := do(t, srv, http.MethodPost, "/api/tasks/reorder",
		`{"ids":["`+ids[2]+`"],"beforeId":"`+ids[0]+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status = %d, want 200: %s", rec.Code, rec.Body)
	}
	listed := decode[[]ui.Task](t, rec)
	if len(listed) != 3 {
		t.Fatalf("reorder returned %d tasks, want 3", len(listed))
	}
	// The default display order is still newest first; the third task
	// moving to the front of the backlog does not change that it was
	// created last.
	if listed[0].ID != ids[1] {
		t.Fatalf("listed[0] = %q, want %q (still newest first)", listed[0].ID, ids[1])
	}
}

func TestReorderRouteRejectsEmptyIDs(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/tasks/reorder", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
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

// TestRetryRouteClearsAFailedTasksStreak is handleRetry's own route test
// (bwsalmon/agents#403's "Retry" button) -- client_test.go's
// TestRetryClearsAFailedTasksStreak already proves Client.Retry itself
// works; this proves the route wires a POST through to it and answers
// with the task in its now-unfailed state, the same "respond with the
// task" contract TestMutatingRoutesRespondWithTheTask checks for every
// other mutating route.
func TestRetryRouteClearsAFailedTasksStreak(t *testing.T) {
	srv, client := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/tasks", `{"title":"fix it","approved":true}`)
	id := decode[ui.Task](t, rec).ID

	for i := 0; i < model.MaxConsecutiveFailures; i++ {
		runID := id + "-r" + strconv.Itoa(i+1)
		started := baseTime.Add(time.Duration(i) * time.Hour)
		if err := client.Store.StartRun(context.Background(), model.Run{
			ID: runID, TaskID: id, Sandbox: "s1", Attempt: i + 1, StartedAt: started,
		}, model.Limits{}); err != nil {
			t.Fatal(err)
		}
		if err := client.Store.FinishRun(context.Background(), runID, started.Add(time.Minute), "failed", "boom"); err != nil {
			t.Fatal(err)
		}
	}

	rec = do(t, srv, http.MethodGet, "/api/tasks/"+id, "")
	if got := decode[ui.TaskDetail](t, rec).State; got != model.StateFailed {
		t.Fatalf("state before retrying = %q, want failed", got)
	}

	rec = do(t, srv, http.MethodPost, "/api/tasks/"+id+"/retry", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", rec.Code, rec.Body)
	}
	task := decode[ui.Task](t, rec)
	if task.ID != id {
		t.Fatalf("responded with task %q, want %q", task.ID, id)
	}
	if task.State != model.StateQueued {
		t.Fatalf("state after retrying = %q, want queued", task.State)
	}
}

func TestRetryRouteOnAnUnknownTaskIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/tasks/nope/retry", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
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

// TestConfigEndpointReportsTargetRepos is the dropdown source
// bwsalmon/agents#447 added the frontend's repo fields: whatever
// CreateTask enforces a task's repo against should be exactly what GET
// /api/config reports, so the UI never offers a repo the server would
// then park off the allowlist.
func TestConfigEndpointReportsTargetRepos(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.TargetRepos = []string{"acme/widgets", "acme/other"}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cfg := decode[struct {
		TargetRepos []string `json:"targetRepos"`
	}](t, rec)
	if !reflect.DeepEqual(cfg.TargetRepos, []string{"acme/widgets", "acme/other"}) {
		t.Fatalf("targetRepos = %v, want [acme/widgets acme/other]", cfg.TargetRepos)
	}
}

// TestConfigEndpointReportsShowClosedByDefault is bwsalmon/agents#537's
// global default reaching a list before Settings has ever been opened
// this session: unconfigured (no UpdateSettings call yet) reports false,
// matching "hide closed tasks by default", and an operator turning it on
// through Settings is reflected immediately, the same store round trip
// TestConfigEndpointReportsTargetRepos already covers for targetRepos.
func TestConfigEndpointReportsShowClosedByDefault(t *testing.T) {
	srv, client := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	cfg := decode[struct {
		ShowClosedByDefault bool `json:"showClosedByDefault"`
	}](t, rec)
	if cfg.ShowClosedByDefault {
		t.Fatalf("showClosedByDefault = true with nothing configured, want false")
	}

	if _, err := client.UpdateSettings(context.Background(), firstSettings()); err != nil {
		t.Fatal(err)
	}
	show := true
	if _, err := client.UpdateSettings(context.Background(), ui.UpdateSettingsRequest{ShowClosedByDefault: &show}); err != nil {
		t.Fatal(err)
	}

	rec = do(t, srv, http.MethodGet, "/api/config", "")
	cfg = decode[struct {
		ShowClosedByDefault bool `json:"showClosedByDefault"`
	}](t, rec)
	if !cfg.ShowClosedByDefault {
		t.Fatalf("showClosedByDefault = false after UpdateSettings, want true")
	}
}

// TestConfigEndpointReportsEnvironmentName is grain/task-69's label
// reaching the frontend on the one call it makes before rendering
// anything: unconfigured reports nothing (an unnamed deployment, and the
// sidebar draws no badge), and naming the deployment through Settings is
// reflected immediately, the same store round trip
// TestConfigEndpointReportsShowClosedByDefault covers above.
func TestConfigEndpointReportsEnvironmentName(t *testing.T) {
	srv, client := testServer(t)

	type environment struct {
		EnvironmentName string `json:"environmentName"`
	}
	rec := do(t, srv, http.MethodGet, "/api/config", "")
	cfg := decode[environment](t, rec)
	if cfg.EnvironmentName != "" {
		t.Fatalf("environmentName = %q with nothing configured, want empty", cfg.EnvironmentName)
	}

	if _, err := client.UpdateSettings(context.Background(), firstSettings()); err != nil {
		t.Fatal(err)
	}
	name := "staging"
	if _, err := client.UpdateSettings(context.Background(), ui.UpdateSettingsRequest{EnvironmentName: &name}); err != nil {
		t.Fatal(err)
	}

	rec = do(t, srv, http.MethodGet, "/api/config", "")
	cfg = decode[environment](t, rec)
	if cfg.EnvironmentName != "staging" {
		t.Fatalf("environmentName = %q after UpdateSettings, want %q", cfg.EnvironmentName, "staging")
	}
}

// TestConfigEndpointReportsTaskDefaults is the same round trip for
// bwsalmon/agents#612's pair, which differs from showClosedByDefault
// above in what "nothing configured" reports: both default on
// (model.DefaultConfig), so a deployment with no stored row yet has to
// say so rather than report the zero value of the field, and an operator
// turning one off through Settings is reflected immediately.
func TestConfigEndpointReportsTaskDefaults(t *testing.T) {
	srv, client := testServer(t)

	type taskDefaults struct {
		ApprovedByDefault  bool `json:"approvedByDefault"`
		AutoMergeByDefault bool `json:"autoMergeByDefault"`
	}
	rec := do(t, srv, http.MethodGet, "/api/config", "")
	cfg := decode[taskDefaults](t, rec)
	if !cfg.ApprovedByDefault || !cfg.AutoMergeByDefault {
		t.Fatalf("approvedByDefault/autoMergeByDefault = %v/%v with nothing configured, want true/true",
			cfg.ApprovedByDefault, cfg.AutoMergeByDefault)
	}

	if _, err := client.UpdateSettings(context.Background(), firstSettings()); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := client.UpdateSettings(context.Background(), ui.UpdateSettingsRequest{ApprovedByDefault: &off}); err != nil {
		t.Fatal(err)
	}

	rec = do(t, srv, http.MethodGet, "/api/config", "")
	cfg = decode[taskDefaults](t, rec)
	if cfg.ApprovedByDefault {
		t.Fatalf("approvedByDefault = true after UpdateSettings turned it off, want false")
	}
	if !cfg.AutoMergeByDefault {
		t.Fatalf("autoMergeByDefault = false after a save that never mentioned it, want true")
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
	if len(cfg.Capabilities) != len(ui.OfferedCapabilities()) {
		t.Fatalf("capabilities = %d, want %d", len(cfg.Capabilities), len(ui.OfferedCapabilities()))
	}
	// The GitHub label a capability used to carry is gone from the wire
	// shape along with the labels themselves.
	if strings.Contains(rec.Body.String(), "grain-gemini-key") {
		t.Fatalf("config still reports a GitHub label: %s", rec.Body)
	}
}

// GET /api/config carries the deployment's default capability set so
// NewTaskOverlay.jsx can open its picker with those boxes already
// ticked -- the form is where whoever files the task sees them, and
// where they untick one they do not want (grain/task-14). A stored
// id this build no longer offers is filtered out, so the form ticks what
// the task would really be filed with.
func TestConfigEndpointReportsDefaultCapabilities(t *testing.T) {
	srv, client := testServer(t)

	cfg := model.DefaultConfig()
	cfg.DefaultCapabilities = []string{"gcp-key", "scratch-repo"}
	if err := client.Store.PutConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decode[struct {
		DefaultCapabilities []string `json:"defaultCapabilities"`
	}](t, rec)
	if !reflect.DeepEqual(got.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("defaultCapabilities = %v, want [gcp-key]", got.DefaultCapabilities)
	}
}

// The per-repo layer travels with the same response, keyed by repo, so
// the new-task form can re-seed its picker the moment the repo picker
// changes rather than asking the server once per keystroke
// (grain/task-24). Filtered the same way, and only repos that add
// something appear at all.
func TestConfigEndpointReportsRepoDefaultCapabilities(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()

	if err := client.Store.PutRepoConfig(ctx, model.RepoConfig{
		Repo:                model.RepoRef{Owner: "acme", Name: "widgets"},
		DefaultCapabilities: []string{"gcp-key", "scratch-repo"},
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing but a retired id: the repo has nothing this build can
	// grant, so it is not reported as adding anything.
	if err := client.Store.PutRepoConfig(ctx, model.RepoConfig{
		Repo:                model.RepoRef{Owner: "acme", Name: "gadgets"},
		DefaultCapabilities: []string{"scratch-repo"},
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decode[struct {
		RepoDefaultCapabilities map[string][]string `json:"repoDefaultCapabilities"`
	}](t, rec)
	want := map[string][]string{"acme/widgets": {"gcp-key"}}
	if !reflect.DeepEqual(got.RepoDefaultCapabilities, want) {
		t.Fatalf("repoDefaultCapabilities = %v, want %v", got.RepoDefaultCapabilities, want)
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
		`{"pollInterval":"1m","maxWorkers":2,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)
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
	cfg := ui.Config{Actor: ui.DefaultActor("alice"), Capabilities: ui.OfferedCapabilities(), Secrets: secretStore}
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

// --- auto-merge degraded -------------------------------------------------
//
// bwsalmon/agents#483: Submit sets AutoMerge, but on a deployment whose
// GitHub credential can't read check runs (Config.AutoMergeDegraded's own
// doc comment), the merge that's supposed to follow never happens, and
// nothing said so anywhere the UI could see -- Submit looked like a
// no-op. These assert /api/config reports the (un)availability nil vs.
// a real func each give, the same "expose the func, don't hardcode true"
// shape TestRebootDisabledByDefault/TestRebootCallsConfiguredFunc already
// give Config.Reboot above.

func TestAutoMergeNotDegradedByDefault(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["autoMergeDegraded"] != nil && got["autoMergeDegraded"] != false {
		t.Fatalf("autoMergeDegraded = %v, want false or omitted", got["autoMergeDegraded"])
	}
}

func TestAutoMergeDegradedReportsConfiguredFunc(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.AutoMergeDegraded = func() bool { return true }
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["autoMergeDegraded"] != true {
		t.Fatalf("autoMergeDegraded = %v, want true", got["autoMergeDegraded"])
	}
}

// --- reconciler down -------------------------------------------------
//
// bwsalmon/agents#576: runDaemon dying (a one-time setup step that
// exhausted its own retries, or any other fatal-but-non-crashing error)
// used to be visible only in a server log line -- the UI/API server
// stays up regardless (bwsalmon/agents#550), so nothing said, anywhere
// the UI could see, that reconciliation itself had actually stopped.
// These mirror TestAutoMergeNotDegradedByDefault/
// TestAutoMergeDegradedReportsConfiguredFunc immediately above: the same
// "expose the func, don't hardcode true" shape, for Config.
// ReconcilerDown instead of Config.AutoMergeDegraded.

func TestReconcilerNotDownByDefault(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["reconcilerDown"] != nil && got["reconcilerDown"] != false {
		t.Fatalf("reconcilerDown = %v, want false or omitted", got["reconcilerDown"])
	}
}

func TestReconcilerDownReportsConfiguredFunc(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.ReconcilerDown = func() bool { return true }
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodGet, "/api/config", "")
	got := decode[map[string]any](t, rec)
	if got["reconcilerDown"] != true {
		t.Fatalf("reconcilerDown = %v, want true", got["reconcilerDown"])
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
	cfg := ui.Config{Actor: ui.DefaultActor("alice"), Capabilities: ui.OfferedCapabilities(), Upgrader: up}
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
