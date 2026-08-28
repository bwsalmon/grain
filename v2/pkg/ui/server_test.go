package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func testServer() (*Server, *memClient) {
	client := newMemClient()
	cfg := Config{
		TaskRepo:     model.RepoRef{Owner: "acme", Name: "tasks"},
		Labels:       DefaultLabels(),
		Capabilities: DefaultCapabilities(),
	}
	return NewServer(cfg, client, nil), client
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response %s: %v", rec.Body.String(), err)
	}
	return v
}

// TestListTasksMergesUntrackedLabelsAndDeduplicates covers ListTasks' own
// GitHub-only half (no store configured, or a task not yet filed): the
// trigger and needsApproval labels are the only two an issue can still
// carry with no store row behind it once pkg/orchestrator.PollIssues is
// live (see untrackedTasks' own doc comment for why Completed/InProgress/
// AwaitingReply are no longer scanned here) -- store-backed states are
// covered by TestListTasksPrefersTrackedTaskOverItsOwnIssue and friends
// in store_test.go, against a real model.Store.
func TestListTasksMergesUntrackedLabelsAndDeduplicates(t *testing.T) {
	s, client := testServer()
	l := DefaultLabels()
	client.seed(1, "queued task", "body", l.Trigger)
	client.seed(2, "proposed task", "body", l.NeedsApproval)
	// An issue on the repo carrying none of the state labels (not a task
	// at all) must not appear.
	client.seed(3, "unrelated issue", "body")

	rec := do(t, s, "GET", "/api/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	tasks := decode[[]Task](t, rec)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2: %+v", len(tasks), tasks)
	}
	byNumber := map[int]Task{}
	for _, task := range tasks {
		byNumber[task.Number] = task
	}
	if byNumber[1].State != StateQueued {
		t.Errorf("task 1 state = %q, want queued", byNumber[1].State)
	}
	if byNumber[2].State != StateNeedsApproval {
		t.Errorf("task 2 state = %q, want needs_approval", byNumber[2].State)
	}
	if _, ok := byNumber[3]; ok {
		t.Errorf("unrelated issue #3 should not appear in the task list")
	}
	// Newest first.
	if tasks[0].Number < tasks[1].Number {
		t.Errorf("tasks not sorted newest-first: %v", tasks)
	}
}

func TestCreateTaskRendersDirectivesAndLabels(t *testing.T) {
	s, client := testServer()
	payload := `{
		"title": "Fix the widget",
		"description": "The widget is broken.",
		"repo": "acme/widget",
		"base": "release",
		"autoMerge": true,
		"capabilities": ["gemini-key"],
		"approved": true
	}`
	rec := do(t, s, "POST", "/api/tasks", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	task := decode[Task](t, rec)
	if task.Repo != "acme/widget" || task.Base != "release" || task.AutoMerge == nil || !*task.AutoMerge {
		t.Fatalf("declared fields not round-tripped: %+v", task)
	}
	if task.State != StateQueued {
		t.Fatalf("approved task state = %q, want queued", task.State)
	}
	if len(task.Capabilities) != 1 || task.Capabilities[0] != "gemini-key" {
		t.Fatalf("capabilities = %v, want [gemini-key]", task.Capabilities)
	}

	issue, _ := client.GetIssue("acme", "tasks", task.Number)
	if !issue.HasLabel(DefaultLabels().Trigger) {
		t.Errorf("created issue missing trigger label: %+v", issue.Labels)
	}
	if issue.HasLabel(DefaultLabels().NeedsApproval) {
		t.Errorf("approved task should not carry needsApproval label")
	}
	if !strings.Contains(issue.Body, "The widget is broken.") {
		t.Errorf("issue body dropped the description: %q", issue.Body)
	}
}

func TestCreateTaskUnapprovedFilesAsProposal(t *testing.T) {
	s, client := testServer()
	rec := do(t, s, "POST", "/api/tasks", `{"title": "Maybe do this"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	task := decode[Task](t, rec)
	if task.State != StateNeedsApproval {
		t.Fatalf("state = %q, want needs_approval", task.State)
	}
	issue, _ := client.GetIssue("acme", "tasks", task.Number)
	if !issue.HasLabel(DefaultLabels().NeedsApproval) {
		t.Errorf("proposed task missing needsApproval label: %+v", issue.Labels)
	}
}

func TestCreateTaskRejectsEmptyTitle(t *testing.T) {
	s, _ := testServer()
	rec := do(t, s, "POST", "/api/tasks", `{"title": ""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreateTaskRejectsUnknownCapability(t *testing.T) {
	s, _ := testServer()
	rec := do(t, s, "POST", "/api/tasks", `{"title": "x", "capabilities": ["no-such-thing"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestApproveSwapsNeedsApprovalForTrigger(t *testing.T) {
	s, client := testServer()
	l := DefaultLabels()
	client.seed(5, "proposed", "body", l.NeedsApproval)

	rec := do(t, s, "POST", "/api/tasks/5/approve", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	task := decode[Task](t, rec)
	if task.State != StateQueued {
		t.Fatalf("state = %q, want queued", task.State)
	}
	issue, _ := client.GetIssue("acme", "tasks", 5)
	if issue.HasLabel(l.NeedsApproval) {
		t.Errorf("needsApproval label should be gone")
	}
	if !issue.HasLabel(l.Trigger) {
		t.Errorf("trigger label should be present")
	}
}

func TestSetCapabilityAttachAndDetach(t *testing.T) {
	s, client := testServer()
	l := DefaultLabels()
	client.seed(6, "task", "body", l.Trigger)

	rec := do(t, s, "POST", "/api/tasks/6/capabilities", `{"id": "self-debug", "attach": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	task := decode[Task](t, rec)
	if len(task.Capabilities) != 1 || task.Capabilities[0] != "self-debug" {
		t.Fatalf("after attach, capabilities = %v", task.Capabilities)
	}

	rec = do(t, s, "POST", "/api/tasks/6/capabilities", `{"id": "self-debug", "attach": false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	task = decode[Task](t, rec)
	if len(task.Capabilities) != 0 {
		t.Fatalf("after detach, capabilities = %v, want none", task.Capabilities)
	}

	issue, _ := client.GetIssue("acme", "tasks", 6)
	if issue.HasLabel("grain-self-debug") {
		t.Errorf("label should have been removed from the issue")
	}
}

func TestCommentsAndCloseReopen(t *testing.T) {
	s, client := testServer()
	l := DefaultLabels()
	client.seed(7, "task", "body", l.Trigger)

	rec := do(t, s, "POST", "/api/tasks/7/comments", `{"body": "hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("comment: status = %d body = %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, "GET", "/api/tasks/7", "")
	detail := decode[TaskDetail](t, rec)
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "hello" {
		t.Fatalf("comments = %+v", detail.Comments)
	}

	rec = do(t, s, "POST", "/api/tasks/7/close", "")
	task := decode[Task](t, rec)
	if task.GitHubState != "closed" {
		t.Fatalf("githubState = %q after close, want closed", task.GitHubState)
	}

	rec = do(t, s, "POST", "/api/tasks/7/reopen", "")
	task = decode[Task](t, rec)
	if task.GitHubState != "open" {
		t.Fatalf("githubState = %q after reopen, want open", task.GitHubState)
	}
	_ = client
}

func TestGetTaskNotFound(t *testing.T) {
	s, _ := testServer()
	rec := do(t, s, "GET", "/api/tasks/999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestConfigEndpointListsCapabilities(t *testing.T) {
	s, _ := testServer()
	rec := do(t, s, "GET", "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var cfg struct {
		Capabilities []Capability `json:"capabilities"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Capabilities) != len(DefaultCapabilities()) {
		t.Fatalf("got %d capabilities, want %d", len(cfg.Capabilities), len(DefaultCapabilities()))
	}
}
