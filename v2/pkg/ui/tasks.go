package ui

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
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

// annotate fills in State and Capabilities against cfg's label taxonomy
// -- split out of taskFrom since that has no Config to read taxonomy off
// of, and every call site here has one on hand instead.
func (s *Server) annotate(t Task) Task {
	set := make(map[string]struct{}, len(t.Labels))
	for _, l := range t.Labels {
		set[l] = struct{}{}
	}
	t.State = deriveState(set, s.cfg.Labels)
	for _, c := range s.cfg.Capabilities {
		if _, ok := set[c.Label]; ok {
			t.Capabilities = append(t.Capabilities, c.ID)
		}
	}
	return t
}

func (s *Server) capabilityByID(id string) (Capability, bool) {
	for _, c := range s.cfg.Capabilities {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
}

// --- handlers ----------------------------------------------------------

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"taskRepo":     s.cfg.TaskRepo,
		"labels":       s.cfg.Labels,
		"capabilities": s.cfg.Capabilities,
	})
}

// handleListTasks merges the open issues carrying each state label into
// one deduplicated list. One call per state label rather than one
// unfiltered call: GitHub's issues-list endpoint has no "any of these
// labels" filter (a comma-separated list is AND, not OR), and every real
// task issue carries exactly one state label by construction, so the
// union of five label-scoped lists is exactly the task list -- with
// nothing skipped that a single, filterless call (which would also
// return every non-task issue on the repo) would have included instead.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	seen := map[int]Task{}
	for _, label := range []string{
		s.cfg.Labels.Trigger, s.cfg.Labels.InProgress, s.cfg.Labels.AwaitingReply,
		s.cfg.Labels.NeedsApproval, s.cfg.Labels.Completed,
	} {
		issues, err := s.client.ListIssues(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, label)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		for _, issue := range issues {
			seen[issue.Number] = s.annotate(taskFrom(issue))
		}
	}
	tasks := make([]Task, 0, len(seen))
	for _, t := range seen {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Number > tasks[j].Number })
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	issue, err := s.client.GetIssue(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number)
	if err != nil {
		writeGitHubError(w, err)
		return
	}
	comments, err := s.client.ListComments(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	detail := TaskDetail{Task: s.annotate(taskFrom(issue))}
	for _, c := range comments {
		detail.Comments = append(detail.Comments, Comment{
			ID: c.ID, User: c.User, Body: c.Body, AuthorAssociation: c.AuthorAssociation,
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

// createTaskRequest is POST /api/tasks' body -- a form's fields, not an
// issue body: the repo picker, base field, auto-merge checkbox and
// capability checkboxes docs/data-model.md's "a form knows all of that
// before the task exists" describes, rendered into directive lines and
// labels server-side so the frontend never constructs GitHub-flavoured
// text itself.
type createTaskRequest struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Repo         string   `json:"repo"`
	Base         string   `json:"base"`
	AutoMerge    *bool    `json:"autoMerge"`
	Capabilities []string `json:"capabilities"`
	// Approved is false for "file this as a proposal" (needsApproval
	// label, the same needs_approval_label a maintainer must apply the
	// trigger label over before anything runs) and true for "queue it
	// now" (trigger label directly) -- LandsQueued's own human/agent
	// distinction (pkg/model/task.go), decided by a checkbox instead of
	// which principal filed it, since every task this form files was
	// filed by the human using it.
	Approved bool `json:"approved"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, errors.New("title is required"))
		return
	}

	var repo *model.RepoRef
	if strings.TrimSpace(req.Repo) != "" {
		parsed, err := model.ParseRepo(req.Repo)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		repo = &parsed
	}

	labels := make([]string, 0, len(req.Capabilities)+1)
	for _, id := range req.Capabilities {
		capability, ok := s.capabilityByID(id)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("unknown capability "+id))
			return
		}
		labels = append(labels, capability.Label)
	}
	if req.Approved {
		labels = append(labels, s.cfg.Labels.Trigger)
	} else {
		labels = append(labels, s.cfg.Labels.NeedsApproval)
	}

	body := bodyOf(req.Description, repo, req.Base, req.AutoMerge)
	issue, err := s.client.CreateIssue(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, req.Title, body, labels)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.annotate(taskFrom(issue)))
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
	capability, ok := s.capabilityByID(req.ID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown capability "+req.ID))
		return
	}
	var err error
	if req.Attach {
		err = s.client.AddLabel(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number, capability.Label)
	} else {
		err = s.client.RemoveLabel(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number, capability.Label)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, number)
}

// handleApprove swaps needsApproval for trigger -- the UI's own "approve
// button" docs/data-model.md's UI direction asks for, since GitHub itself
// has none for a plain issue. Removing a label that isn't there 404s at
// the Client level, which RemoveLabel already treats as success (github.go's
// own doc comment on it), so approving an already-queued task is a no-op
// rather than an error.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.client.RemoveLabel(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number, s.cfg.Labels.NeedsApproval); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := s.client.AddLabel(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number, s.cfg.Labels.Trigger); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, number)
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
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, errors.New("body is required"))
		return
	}
	if _, err := s.client.CreateComment(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number, req.Body); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, number)
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.client.CloseIssue(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, number)
}

func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	number, ok := s.pathNumber(w, r)
	if !ok {
		return
	}
	if err := s.client.ReopenIssue(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.respondWithTask(w, number)
}

func (s *Server) respondWithTask(w http.ResponseWriter, number int) {
	issue, err := s.client.GetIssue(s.cfg.TaskRepo.Owner, s.cfg.TaskRepo.Name, number)
	if err != nil {
		writeGitHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.annotate(taskFrom(issue)))
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
