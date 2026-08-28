package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Task is a task's JSON shape -- everything the frontend needs to list
// and render one.
//
// ID is a string, not a number. It used to be the GitHub issue number a
// task was named after; it is now whatever Store.NewTaskID allocated,
// which happens to be decimal today and is opaque to everything here.
//
// Three fields the issue-backed shape carried are gone: htmlUrl and
// githubState, because there is no issue to link to or mirror the open/
// closed flag of, and labels, because state and capabilities are no
// longer derived from any. PullRequest replaces htmlUrl as the one thing
// worth linking to on GitHub, read off the task's own LinkFixes link.
type Task struct {
	ID           string      `json:"id"`
	Title        string      `json:"title"`
	Description  string      `json:"description"`
	Author       string      `json:"author"`
	State        model.State `json:"state"`
	Repo         string      `json:"repo,omitempty"`
	Base         string      `json:"base,omitempty"`
	AutoMerge    bool        `json:"autoMerge"`
	Capabilities []string    `json:"capabilities"`
	PullRequest  string      `json:"pullRequest,omitempty"`
	CreatedAt    *time.Time  `json:"createdAt,omitempty"`
}

// Comment is one entry in a task's conversation.
//
// AuthorAssociation is gone with the issue thread that had one. What
// replaces it says more: OnBehalfOf is set when grain relayed somebody
// else's words, so a question from a dispatched run renders as grain
// speaking for an agent rather than as grain's own.
type Comment struct {
	ID         int64     `json:"id"`
	Author     string    `json:"author"`
	AuthorKind string    `json:"authorKind"`
	OnBehalfOf string    `json:"onBehalfOf,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}

// TaskDetail is one task plus its conversation -- what GET
// /api/tasks/{id} returns, wider than the list shape since a list of many
// tasks has no reason to pay for every conversation.
type TaskDetail struct {
	Task
	Comments []Comment `json:"comments"`
}

func taskFrom(t model.Task, state model.State) Task {
	out := Task{
		ID:           t.ID,
		Title:        t.Title,
		Description:  t.Body,
		Author:       t.Origin.Attribution.Actor.ID,
		State:        state,
		Base:         t.Base,
		AutoMerge:    t.AutoMerge,
		Capabilities: []string{},
		CreatedAt:    t.CreatedAt,
	}
	if t.Target != nil {
		out.Repo = t.Target.String()
	}
	for _, g := range t.Grants {
		out.Capabilities = append(out.Capabilities, g.Capability)
	}
	for _, l := range t.Links {
		if l.Kind == model.LinkFixes {
			out.PullRequest = l.Target
		}
	}
	return out
}

func commentFrom(c model.Comment) Comment {
	out := Comment{
		ID:         c.ID,
		Author:     c.Author.Actor.ID,
		AuthorKind: string(c.Author.Actor.Kind),
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
	}
	if b := c.Author.OnBehalfOf; b != nil {
		out.OnBehalfOf = b.ID
	}
	return out
}

// --- handlers ------------------------------------------------------------
//
// Every handler below is a thin HTTP shim over a Client method: decode
// the request, call it, translate the result (or error) into a status
// code and a JSON body. The logic that actually reads or writes the store
// lives in client.go, once, so a non-HTTP caller (cmd/grain's CLI) gets
// identical behaviour by calling the same Client directly.

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"actor":         s.tasks.Config.Actor,
		"defaultTarget": s.tasks.Config.DefaultTarget,
		"capabilities":  s.tasks.Config.Capabilities,
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	detail, err := s.tasks.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeClientError(w, err)
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
	var req setCapabilityRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.tasks.SetCapability(r.Context(), id, req.ID, req.Attach); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleApprove is the UI's own "approve button" docs/data-model.md's UI
// direction asks for.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Approve(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleSubmit is the UI's own "submit" button docs/data-model.md's UI
// direction asks for, alongside the approve button handleApprove already
// is: once a task's run has produced a pull request, this is what queues
// it for the merge queue's automatic conflict resolution and merging.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Submit(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

type addCommentRequest struct {
	Body string `json:"body"`
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var req addCommentRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.tasks.AddComment(r.Context(), id, req.Body); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Close(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Reopen(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

func (s *Server) respondWithTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.tasks.Task(r.Context(), id)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// --- request/response plumbing -----------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeClientError maps a Client error to a status: 400 for a
// ValidationError (a mistake caught before the store was touched), 404
// for a task id with nothing behind it, and 500 otherwise.
//
// The fallback is 500 rather than the 502 this used to return. 502 was
// right when every failure came from an upstream GitHub call; the store
// is not upstream of this process in the same sense, and reporting a
// database error as "the gateway had trouble" would point an operator at
// the wrong thing.
func writeClientError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	// A conflict that survived the store's own retries: the change did not
	// land, and saying so plainly beats retrying forever or calling it a
	// server fault. It should be vanishingly rare -- it needs another
	// writer to win several times in a row.
	if errors.Is(err, model.ErrConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
