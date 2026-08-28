package ui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/bwsalmon/grain/v2/pkg/model"
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
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("POST /api/tasks/{id}/capabilities", s.handleSetCapability)
	s.mux.HandleFunc("POST /api/tasks/{id}/approve", s.handleApprove)
	s.mux.HandleFunc("POST /api/tasks/{id}/submit", s.handleSubmit)
	s.mux.HandleFunc("POST /api/tasks/{id}/comments", s.handleAddComment)
	s.mux.HandleFunc("POST /api/tasks/{id}/close", s.handleClose)
	s.mux.HandleFunc("POST /api/tasks/{id}/reopen", s.handleReopen)

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is embedded at build time from a directory this package
		// ships -- its absence is a build-time mistake, not a runtime
		// condition a caller can do anything about.
		panic("ui: embedding static/: " + err.Error())
	}
	s.mux.Handle("/", http.FileServerFS(static))
}
