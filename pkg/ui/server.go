package ui

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

//go:embed static
var staticFS embed.FS

// Server is the JSON API plus the static frontend it serves, a thin HTTP
// shim over a Client. The store behind that Client is the record -- this
// holds no state of its own and caches nothing, so there is nowhere here
// for a stale value to hide.
type Server struct {
	tasks *Client
	mux   *http.ServeMux
	// paths carries the same API paths as mux with the method dropped,
	// so an unmatched /api/ request can say whether the path exists at
	// all -- see apiFallback.
	paths *http.ServeMux
}

// NewServer builds a Server over a store.
func NewServer(cfg Config, store *model.Store) *Server {
	s := &Server{tasks: NewClient(cfg, store), mux: http.NewServeMux(), paths: http.NewServeMux()}
	s.routes()
	return s
}

// NewServerWithClient builds a Server over an already-configured Client
// -- for a caller that needs to set its clock, or share one Client with a
// non-HTTP path.
func NewServerWithClient(client *Client) *Server {
	s := &Server{tasks: client, mux: http.NewServeMux(), paths: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.route("GET /api/config", s.handleConfig)
	s.route("GET /api/settings", s.handleGetSettings)
	s.route("PUT /api/settings", s.handleUpdateSettings)
	s.route("POST /api/capabilities/{id}/check", s.handleCheckCapability)
	s.route("GET /api/tasks", s.handleListTasks)
	s.route("POST /api/tasks", s.handleCreateTask)
	s.route("POST /api/tasks/reorder", s.handleReorder)
	s.route("GET /api/tasks/{id}", s.handleGetTask)
	s.route("PATCH /api/tasks/{id}", s.handleUpdateTask)
	s.route("POST /api/tasks/{id}/capabilities", s.handleSetCapability)
	s.route("POST /api/tasks/{id}/depends-on", s.handleSetDependency)
	s.route("POST /api/tasks/{id}/approve", s.handleApprove)
	s.route("POST /api/tasks/{id}/withdraw-approval", s.handleWithdrawApproval)
	s.route("POST /api/tasks/{id}/submit", s.handleSubmit)
	s.route("POST /api/tasks/{id}/comments", s.handleAddComment)
	// PUT, not POST: setting the one secret a parked run asked for is an
	// idempotent write of a value at a name the task already fixed --
	// the same shape (and the same verb) /api/secrets/{secret}/{key}
	// already uses for the write this one delegates to.
	s.route("PUT /api/tasks/{id}/secret", s.handleSetTaskSecret)
	s.route("GET /api/tasks/{id}/attachments/{attachmentId}", s.handleGetAttachment)
	s.route("POST /api/tasks/{id}/close", s.handleClose)
	s.route("POST /api/tasks/{id}/reopen", s.handleReopen)
	s.route("POST /api/tasks/{id}/retry", s.handleRetry)
	s.route("POST /api/tasks/{id}/pull-request", s.handleOpenPullRequest)
	s.route("POST /api/tasks/{id}/sandbox/recreate", s.handleRecreateSandbox)
	s.route("POST /api/tasks/{id}/activity", s.handleSetTaskActivity)
	s.route("GET /api/tasks/{id}/attempts/{number}/transcript", s.handleGetAttemptTranscript)
	s.route("GET /api/tasks/{id}/prompt", s.handleGetTaskPrompt)

	s.route("POST /api/repos", s.handleAddTargetRepo)
	s.route("DELETE /api/repos/{owner}/{name}", s.handleRemoveTargetRepo)

	s.route("GET /api/repos/{owner}/{name}/capabilities", s.handleGetRepoCapabilities)
	s.route("PUT /api/repos/{owner}/{name}/capabilities", s.handleSetRepoCapabilities)
	s.route("GET /api/repos/{owner}/{name}/prompt-extension", s.handleGetRepoPromptExtension)
	s.route("PUT /api/repos/{owner}/{name}/prompt-extension", s.handleSetRepoPromptExtension)
	s.route("GET /api/repos/{owner}/{name}/setup-command", s.handleGetRepoSetupCommand)
	s.route("PUT /api/repos/{owner}/{name}/setup-command", s.handleSetRepoSetupCommand)

	s.route("GET /api/repos/{owner}/{name}/releases", s.handleListReleases)
	s.route("POST /api/repos/{owner}/{name}/releases", s.handleCreateRelease)
	s.route("GET /api/repos/{owner}/{name}/releases/{release}", s.handleGetRelease)
	s.route("POST /api/repos/{owner}/{name}/releases/{release}/merge", s.handleRequestReleaseMerge)
	s.route("GET /api/repos/{owner}/{name}/releases/{release}/candidates", s.handleListCandidates)
	s.route("POST /api/repos/{owner}/{name}/releases/{release}/candidates", s.handleCutCandidate)
	s.route("POST /api/repos/{owner}/{name}/releases/{release}/candidates/promote", s.handlePromoteCandidate)

	s.route("GET /api/repos/{owner}/{name}/branches", s.handleListBranches)
	s.route("POST /api/repos/{owner}/{name}/branches", s.handleCreateBranch)

	s.route("GET /api/repos/{owner}/{name}/qualification-plan", s.handleGetQualificationPlan)
	s.route("PUT /api/repos/{owner}/{name}/qualification-plan", s.handlePutQualificationPlan)
	s.route("GET /api/repos/{owner}/{name}/candidates/{id}/qualification", s.handleGetCandidateQualification)
	s.route("POST /api/repos/{owner}/{name}/candidates/{id}/qualification/approve", s.handleApproveQualificationRun)

	s.route("GET /api/schedules", s.handleListSchedules)
	s.route("POST /api/schedules", s.handleCreateSchedule)
	s.route("PATCH /api/schedules/{id}", s.handleUpdateSchedule)
	s.route("DELETE /api/schedules/{id}", s.handleDeleteSchedule)

	s.route("GET /api/templates", s.handleListTemplates)
	s.route("POST /api/templates", s.handleCreateTemplate)
	s.route("PATCH /api/templates/{id}", s.handleUpdateTemplate)
	s.route("DELETE /api/templates/{id}", s.handleDeleteTemplate)

	s.route("GET /api/suites", s.handleListSuites)
	s.route("POST /api/suites", s.handleCreateSuite)
	s.route("PATCH /api/suites/{id}", s.handleUpdateSuite)
	s.route("DELETE /api/suites/{id}", s.handleDeleteSuite)
	s.route("GET /api/suite-runs", s.handleListSuiteRuns)
	s.route("POST /api/suite-runs", s.handleCreateSuiteRun)
	s.route("GET /api/suite-runs/{id}", s.handleGetSuiteRun)

	s.route("GET /api/agent-models", s.handleListAgentModels)

	s.route("GET /api/agent-keys", s.handleListAgentKeys)
	s.route("PUT /api/agent-keys/{framework}", s.handleSetAgentKey)
	s.route("DELETE /api/agent-keys/{framework}", s.handleDeleteAgentKey)
	// The one route in this family that asks the vendor rather than the
	// store: whether the credential set above is still one that works.
	s.route("POST /api/agent-keys/{framework}/check", s.handleCheckAgentKey)

	s.route("GET /api/github-tokens", s.handleListGitHubTokens)
	s.route("PUT /api/github-tokens/{name}", s.handleSetGitHubToken)
	s.route("DELETE /api/github-tokens/{name}", s.handleDeleteGitHubToken)
	s.route("PUT /api/github-credential-patterns", s.handleSetGitHubCredentialPattern)
	s.route("DELETE /api/github-credential-patterns", s.handleDeleteGitHubCredentialPattern)

	s.route("GET /api/secrets", s.handleListSecrets)
	s.route("PUT /api/secrets/{secret}/{key}", s.handleSetSecret)
	s.route("DELETE /api/secrets/{secret}/{key}", s.handleDeleteSecretKey)
	s.route("DELETE /api/secrets/{secret}", s.handleDeleteSecret)

	s.route("GET /api/state-repo", s.handleGetStateRepo)
	s.route("POST /api/state-repo", s.handleSetStateRepo)
	s.route("POST /api/state-repo/sync", s.handleSyncStateRepo)
	s.route("POST /api/state-repo/secrets-key", s.handleImportSecretsKey)

	s.route("POST /api/host/reboot", s.handleRebootHost)
	s.route("GET /api/host/top", s.handleGetHostTop)
	s.route("POST /api/host/shell", s.handleRootShell)
	s.route("GET /api/upgrade", s.handleGetUpgradeStatus)
	s.route("POST /api/upgrade", s.handleStartUpgrade)

	s.route("GET /api/logs", s.handleListLogSources)
	s.route("GET /api/logs/{source}", s.handleGetLogLines)

	s.route("GET /api/sandboxes", s.handleGetSandboxHealth)

	s.route("GET /api/metrics", s.handleGetMetrics)

	s.route("GET /api/pause", s.handleGetAgentPause)
	s.route("DELETE /api/pause", s.handleLiftAgentPause)

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded at build time from a directory this package
		// ships -- its absence is a build-time mistake, not a runtime
		// condition a caller can do anything about.
		panic("ui: embedding static/: " + err.Error())
	}
	s.mux.Handle("/", s.apiFallback(spaHandler(static)))
}

// route registers one "METHOD /path" handler, and records the path on
// its own mux so apiFallback can tell a path that does not exist from
// one reached with the wrong method.
func (s *Server) route(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
	_, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return
	}
	if _, matched := s.paths.Handler(&http.Request{Method: http.MethodGet, URL: &url.URL{Path: path}}); matched == path {
		return // a second method on a path already recorded
	}
	s.paths.Handle(path, knownAPIPath{})
}

// knownAPIPath is the sentinel Server.paths is filled with: it is never
// served, only recognised, and being a named type rather than a closure
// is what lets apiFallback tell it apart from the redirect handler a
// ServeMux can return instead.
type knownAPIPath struct{}

func (knownAPIPath) ServeHTTP(http.ResponseWriter, *http.Request) {}

// apiFallback answers an /api/ request no route matched, instead of
// letting it fall through to next -- the SPA, which would answer any
// wrong path or wrong method with 200 and a page of HTML.
//
// That mattered beyond tidiness (found by hand, task 244): every caller
// of this API expects JSON. A CLI a version out of step with its daemon,
// an MCP tool hop like open_pull_request landing on a daemon that has no
// such route, or a typo in a curl -- each got "200 OK" and then failed
// somewhere further on parsing "<!doctype html>", naming a character
// rather than the endpoint.
func (s *Server) apiFallback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if h, _ := s.paths.Handler(r); h != nil {
			if _, known := h.(knownAPIPath); known {
				writeError(w, http.StatusMethodNotAllowed,
					fmt.Errorf("%s is not allowed on %s", r.Method, r.URL.Path))
				return
			}
		}
		writeError(w, http.StatusNotFound,
			fmt.Errorf("no such API endpoint: %s %s", r.Method, r.URL.Path))
	})
}

// spaHandler serves static assets straight out of fsys, the same as
// http.FileServerFS -- except a request for a path that isn't an actual
// file falls back to index.html instead of 404ing. App.jsx (bwsalmon/
// agents#548) now reads a sub-page like "/repos" or "/tasks/42" out of
// the URL itself, but that only works for a client-side navigation that
// starts from "/" already loaded; a direct hit on that URL -- a bookmark,
// a shared link, a hard refresh -- is a real request this server has to
// answer, and the only page it has to answer with is the one App.jsx's
// own routing already knows how to parse.
//
// A //go:embed'd static/ with no built frontend yet (a fresh checkout
// under `go test`, where static/ carries only placeholder.html) has no
// index.html to fall back to; that case is left to fall through to the
// normal file-not-found response rather than serving placeholder.html
// in its place.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServerFS(fsys)
	index, indexErr := fs.ReadFile(fsys, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(r.URL.Path, "/")
		if upath != "" {
			if _, err := fs.Stat(fsys, upath); err != nil && indexErr == nil {
				http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
