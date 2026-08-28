package ui

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
)

// Task is a task issue's JSON shape -- everything the frontend needs to
// list and render one, with the declared (/repo, /base, /auto-merge)
// fields already split out of the free-text body the same way a create
// form edits them, per docs/data-model.md's "a form knows all of that
// before the task exists."
type Task struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	HTMLURL      string   `json:"htmlUrl"`
	Author       string   `json:"author"`
	State        State    `json:"state"`
	GitHubState  string   `json:"githubState"`
	Repo         string   `json:"repo,omitempty"`
	Base         string   `json:"base,omitempty"`
	AutoMerge    *bool    `json:"autoMerge,omitempty"`
	Capabilities []string `json:"capabilities"`
	Labels       []string `json:"labels"`
}

// Comment is a plain top-level issue comment's JSON shape.
type Comment struct {
	ID                int    `json:"id"`
	User              string `json:"user"`
	Body              string `json:"body"`
	AuthorAssociation string `json:"authorAssociation"`
}

// TaskDetail is one task plus its conversation thread -- what GET
// /api/tasks/{number} returns, wider than the list shape (Task) since a
// list of many tasks has no reason to pay for every comment thread.
type TaskDetail struct {
	Task
	Comments []Comment `json:"comments"`
	// Runs is a tracked task's own run history, off the store -- nil for
	// a task not yet filed (see Client.GetTask), which has never been
	// dispatched. Additive to the wire shape pkg/ui/static/app.js already
	// parses: an old build simply never reads this key, the same as any
	// other field it does not render.
	Runs []Run `json:"runs,omitempty"`
}

// Run is one past attempt at a task, read off model.Store's own run
// history (Store.Runs) -- see TaskDetail's own doc comment on why this
// is only ever populated for a tracked task.
type Run struct {
	ID         string     `json:"id"`
	Slot       string     `json:"slot"`
	Sandbox    string     `json:"sandbox"`
	Attempt    int        `json:"attempt"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
}

func taskFrom(issue github.Issue) Task {
	d, err := parseDirectives(issue.Body)
	if err != nil {
		// A body a human hand-edited into something parseDirectives can't
		// read is still a real issue worth showing -- just with its
		// declared fields left blank, the same "genuinely dynamic, shows up
		// at read time" case docs/data-model.md carves out for the parked
		// path. Logged, not surfaced as a 500: one malformed task must not
		// take the whole list down with it.
		log.Printf("ui: parsing directives on #%d: %v", issue.Number, err)
	}
	t := Task{
		Number: issue.Number, Title: issue.Title, HTMLURL: issue.HTMLURL,
		Author: issue.Author, GitHubState: issue.State,
		Description:  stripDirectives(issue.Body),
		Capabilities: []string{},
		Labels:       []string{},
	}
	if d.Repo != nil {
		t.Repo = d.Repo.String()
	}
	t.Base = d.Base
	if d.HasAutoMerge {
		am := d.AutoMerge
		t.AutoMerge = &am
	}
	for name := range issue.Labels {
		t.Labels = append(t.Labels, name)
	}
	sort.Strings(t.Labels)
	return t
}

// stripDirectives removes directive lines from body, leaving the
// free-text description a create form edits -- bodyOf's inverse for the
// part that isn't a declared field.
func stripDirectives(body string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if directiveRe.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// --- handlers ------------------------------------------------------------
//
// Every handler below is a thin HTTP shim over a Client method: decode
// the request, call it, translate the result (or error) into a status
// code and a JSON body. The logic that actually reads or writes GitHub
// lives in client.go, once, so a non-HTTP caller (cmd/grain's CLI) gets
// identical behaviour by calling the same Client directly.

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"taskRepo":     s.tasks.Config.TaskRepo,
		"labels":       s.tasks.Config.Labels,
		"capabilities": s.tasks.Config.Capabilities,
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	detail, err := s.tasks.GetTask(r.Context(), number)
	if err != nil {
		writeGitHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	task, err := s.tasks.CreateTask(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

type setCapabilityRequest struct {
	ID     string `json:"id"`
	Attach bool   `json:"attach"`
}

func (s *Server) handleSetCapability(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	var req setCapabilityRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.tasks.SetCapability(r.Context(), number, req.ID, req.Attach); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, number)
}

// handleApprove is the UI's own "approve button" docs/data-model.md's UI
// direction asks for, since GitHub itself has none for a plain issue.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.tasks.Approve(r.Context(), number); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, r, number)
}

type addCommentRequest struct {
	Body string `json:"body"`
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	var req addCommentRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.tasks.AddComment(r.Context(), number, req.Body); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, number)
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.tasks.Close(r.Context(), number); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, r, number)
}

func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.tasks.Reopen(r.Context(), number); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, r, number)
}

func (s *Server) respondWithTask(w http.ResponseWriter, r *http.Request, number int) {
	task, err := s.tasks.Task(r.Context(), number)
	if err != nil {
		writeGitHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// --- request/response plumbing -----------------------------------------

func (s *Server) pathNumber(w http.ResponseWriter, r *http.Request) (int, bool) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid task number"))
		return 0, false
	}
	return number, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeClientError maps a Client error to 400 when it is a
// ValidationError (a mistake caught before any GitHub call was made) and
// to 502 otherwise (the GitHub call itself failing) -- the same split
// every mutating handler made inline before Client existed.
func writeClientError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeError(w, http.StatusBadGateway, err)
}

// writeGitHubError reports a github.Error's own status (e.g. 404 for a
// task number that doesn't exist) instead of flattening every upstream
// failure to 502 -- the one call site (get/respondWithTask) where the
// caller's mistake (a bad number) and GitHub's own trouble need telling
// apart.
func writeGitHubError(w http.ResponseWriter, err error) {
	var ghErr *github.Error
	if errors.As(err, &ghErr) && ghErr.Status == http.StatusNotFound {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadGateway, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
