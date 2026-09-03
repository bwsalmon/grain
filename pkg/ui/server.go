package ui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
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
}

// NewServer builds a Server over a store.
func NewServer(cfg Config, store *model.Store) *Server {
	s := &Server{tasks: NewClient(cfg, store), mux: http.NewServeMux()}
	s.routes()
	return s
}

// NewServerWithClient builds a Server over an already-configured Client
// -- for a caller that needs to set its clock, or share one Client with a
// non-HTTP path.
func NewServerWithClient(client *Client) *Server {
	s := &Server{tasks: client, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	s.mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
	s.mux.HandleFunc("POST /api/capabilities/{id}/check", s.handleCheckCapability)
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	s.mux.HandleFunc("POST /api/tasks/reorder", s.handleReorder)
	s.mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("PATCH /api/tasks/{id}", s.handleUpdateTask)
	s.mux.HandleFunc("POST /api/tasks/{id}/capabilities", s.handleSetCapability)
	s.mux.HandleFunc("POST /api/tasks/{id}/depends-on", s.handleSetDependency)
	s.mux.HandleFunc("POST /api/tasks/{id}/approve", s.handleApprove)
	s.mux.HandleFunc("POST /api/tasks/{id}/withdraw-approval", s.handleWithdrawApproval)
	s.mux.HandleFunc("POST /api/tasks/{id}/submit", s.handleSubmit)
	s.mux.HandleFunc("POST /api/tasks/{id}/comments", s.handleAddComment)
	s.mux.HandleFunc("GET /api/tasks/{id}/attachments/{attachmentId}", s.handleGetAttachment)
	s.mux.HandleFunc("POST /api/tasks/{id}/close", s.handleClose)
	s.mux.HandleFunc("POST /api/tasks/{id}/reopen", s.handleReopen)
	s.mux.HandleFunc("POST /api/tasks/{id}/retry", s.handleRetry)
	s.mux.HandleFunc("POST /api/tasks/{id}/pull-request", s.handleOpenPullRequest)
	s.mux.HandleFunc("POST /api/tasks/{id}/sandbox/recreate", s.handleRecreateSandbox)
	s.mux.HandleFunc("GET /api/tasks/{id}/attempts/{number}/transcript", s.handleGetAttemptTranscript)
	s.mux.HandleFunc("GET /api/tasks/{id}/prompt", s.handleGetTaskPrompt)

	s.mux.HandleFunc("POST /api/repos", s.handleAddTargetRepo)
	s.mux.HandleFunc("DELETE /api/repos/{owner}/{name}", s.handleRemoveTargetRepo)

	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/capabilities", s.handleGetRepoCapabilities)
	s.mux.HandleFunc("PUT /api/repos/{owner}/{name}/capabilities", s.handleSetRepoCapabilities)
	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/prompt-extension", s.handleGetRepoPromptExtension)
	s.mux.HandleFunc("PUT /api/repos/{owner}/{name}/prompt-extension", s.handleSetRepoPromptExtension)
	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/setup-command", s.handleGetRepoSetupCommand)
	s.mux.HandleFunc("PUT /api/repos/{owner}/{name}/setup-command", s.handleSetRepoSetupCommand)

	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/releases", s.handleListReleases)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/releases", s.handleCreateRelease)
	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/releases/{release}", s.handleGetRelease)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/releases/{release}/merge", s.handleRequestReleaseMerge)
	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/releases/{release}/candidates", s.handleListCandidates)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/releases/{release}/candidates", s.handleCutCandidate)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/releases/{release}/candidates/promote", s.handlePromoteCandidate)

	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/branches", s.handleListBranches)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/branches", s.handleCreateBranch)

	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/qualification-plan", s.handleGetQualificationPlan)
	s.mux.HandleFunc("PUT /api/repos/{owner}/{name}/qualification-plan", s.handlePutQualificationPlan)
	s.mux.HandleFunc("GET /api/repos/{owner}/{name}/candidates/{id}/qualification", s.handleGetCandidateQualification)
	s.mux.HandleFunc("POST /api/repos/{owner}/{name}/candidates/{id}/qualification/approve", s.handleApproveQualificationRun)

	s.mux.HandleFunc("GET /api/schedules", s.handleListSchedules)
	s.mux.HandleFunc("POST /api/schedules", s.handleCreateSchedule)
	s.mux.HandleFunc("PATCH /api/schedules/{id}", s.handleUpdateSchedule)
	s.mux.HandleFunc("DELETE /api/schedules/{id}", s.handleDeleteSchedule)

	s.mux.HandleFunc("GET /api/templates", s.handleListTemplates)
	s.mux.HandleFunc("POST /api/templates", s.handleCreateTemplate)
	s.mux.HandleFunc("PATCH /api/templates/{id}", s.handleUpdateTemplate)
	s.mux.HandleFunc("DELETE /api/templates/{id}", s.handleDeleteTemplate)

	s.mux.HandleFunc("GET /api/suites", s.handleListSuites)
	s.mux.HandleFunc("POST /api/suites", s.handleCreateSuite)
	s.mux.HandleFunc("PATCH /api/suites/{id}", s.handleUpdateSuite)
	s.mux.HandleFunc("DELETE /api/suites/{id}", s.handleDeleteSuite)
	s.mux.HandleFunc("GET /api/suite-runs", s.handleListSuiteRuns)
	s.mux.HandleFunc("POST /api/suite-runs", s.handleCreateSuiteRun)
	s.mux.HandleFunc("GET /api/suite-runs/{id}", s.handleGetSuiteRun)

	s.mux.HandleFunc("GET /api/agent-keys", s.handleListAgentKeys)
	s.mux.HandleFunc("PUT /api/agent-keys/{framework}", s.handleSetAgentKey)
	s.mux.HandleFunc("DELETE /api/agent-keys/{framework}", s.handleDeleteAgentKey)

	s.mux.HandleFunc("GET /api/github-tokens", s.handleListGitHubTokens)
	s.mux.HandleFunc("PUT /api/github-tokens/{name}", s.handleSetGitHubToken)
	s.mux.HandleFunc("DELETE /api/github-tokens/{name}", s.handleDeleteGitHubToken)

	s.mux.HandleFunc("GET /api/secrets", s.handleListSecrets)
	s.mux.HandleFunc("PUT /api/secrets/{secret}/{key}", s.handleSetSecret)
	s.mux.HandleFunc("DELETE /api/secrets/{secret}/{key}", s.handleDeleteSecretKey)
	s.mux.HandleFunc("DELETE /api/secrets/{secret}", s.handleDeleteSecret)

	s.mux.HandleFunc("GET /api/state-repo", s.handleGetStateRepo)
	s.mux.HandleFunc("POST /api/state-repo", s.handleSetStateRepo)
	s.mux.HandleFunc("POST /api/state-repo/sync", s.handleSyncStateRepo)

	s.mux.HandleFunc("POST /api/host/reboot", s.handleRebootHost)
	s.mux.HandleFunc("GET /api/host/top", s.handleGetHostTop)
	s.mux.HandleFunc("GET /api/upgrade", s.handleGetUpgradeStatus)
	s.mux.HandleFunc("POST /api/upgrade", s.handleStartUpgrade)

	s.mux.HandleFunc("GET /api/logs", s.handleListLogSources)
	s.mux.HandleFunc("GET /api/logs/{source}", s.handleGetLogLines)

	s.mux.HandleFunc("GET /api/sandboxes", s.handleGetSandboxHealth)

	s.mux.HandleFunc("GET /api/metrics", s.handleGetMetrics)

	s.mux.HandleFunc("GET /api/pause", s.handleGetAgentPause)
	s.mux.HandleFunc("DELETE /api/pause", s.handleLiftAgentPause)

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded at build time from a directory this package
		// ships -- its absence is a build-time mistake, not a runtime
		// condition a caller can do anything about.
		panic("ui: embedding static/: " + err.Error())
	}
	s.mux.Handle("/", spaHandler(static))
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
