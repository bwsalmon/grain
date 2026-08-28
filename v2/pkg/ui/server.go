package ui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

//go:embed static
var staticFS embed.FS

// Config is what a Server needs to know about the deployment it fronts:
// which repo holds task issues, and what its label taxonomy is. Both are
// operator config, the same way AutomationConfig's fields are in v1 --
// nothing here discovers either by inspecting the repo.
type Config struct {
	TaskRepo     model.RepoRef
	Labels       Labels
	Capabilities []Capability
}

// Server is the JSON API plus the static frontend it serves, over
// GitHub's own REST API through client -- no store, no database of its
// own. See the package doc comment for why: a task issue is the record,
// and this is a view onto it, not a fourth one.
type Server struct {
	cfg    Config
	client github.Client
	mux    *http.ServeMux
}

// NewServer builds a Server. client is deliberately the github.Client
// interface, not *github.RESTClient -- a caller can point this at
// github.DryRunClient (mutations print instead of firing) or
// githubsim.Sim the same way every other v2 package that takes a Client
// can, e.g. for a demo run against no real GitHub token at all.
func NewServer(cfg Config, client github.Client) *Server {
	s := &Server{cfg: cfg, client: client, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /api/tasks/{number}", s.handleGetTask)
	s.mux.HandleFunc("POST /api/tasks/{number}/capabilities", s.handleSetCapability)
	s.mux.HandleFunc("POST /api/tasks/{number}/approve", s.handleApprove)
	s.mux.HandleFunc("POST /api/tasks/{number}/comments", s.handleAddComment)
	s.mux.HandleFunc("POST /api/tasks/{number}/close", s.handleClose)
	s.mux.HandleFunc("POST /api/tasks/{number}/reopen", s.handleReopen)

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded at build time from a directory this package
		// ships -- its absence is a build-time mistake, not a runtime
		// condition a caller can do anything about.
		panic("ui: embedding static/: " + err.Error())
	}
	s.mux.Handle("/", http.FileServerFS(static))
}
