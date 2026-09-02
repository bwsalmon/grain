package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// LogSource is one named stream of recent log lines a debugging page can
// show, oldest first -- journalctl for the daemon's own unit, a plain
// file for the git proxy's audit log, or anything else a deployment
// wants to surface, each behind whatever mechanism actually fits it
// (see pkg/systemlog).
type LogSource interface {
	Tail(ctx context.Context, lines int) ([]string, error)
}

// defaultLogLines is how many lines GET /api/logs/{source} returns when
// the caller does not say -- enough to see recent activity without
// pulling an unbounded journal through a JSON response by default.
const defaultLogLines = 500

// maxLogLines caps what a caller can ask for in one request, regardless
// of ?lines=.
const maxLogLines = 5000

// logSourcesResponse is GET /api/logs' whole body. Enabled is false,
// with no sources listed, when this deployment's UI has no log sources
// configured -- the frontend uses that to hide the debugging page's log
// pane entirely, the same convention upgradeStatusResponse's own Enabled
// already establishes.
type logSourcesResponse struct {
	Enabled bool     `json:"enabled"`
	Sources []string `json:"sources,omitempty"`
}

func (s *Server) handleListLogSources(w http.ResponseWriter, r *http.Request) {
	if len(s.tasks.Config.Logs) == 0 {
		writeJSON(w, http.StatusOK, logSourcesResponse{Enabled: false})
		return
	}
	names := make([]string, 0, len(s.tasks.Config.Logs))
	for name := range s.tasks.Config.Logs {
		names = append(names, name)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, logSourcesResponse{Enabled: true, Sources: names})
}

// logLinesResponse is GET /api/logs/{source}'s whole body.
type logLinesResponse struct {
	Lines []string `json:"lines"`
}

func (s *Server) handleGetLogLines(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("source")
	source, ok := s.tasks.Config.Logs[name]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such log source: %q", name))
		return
	}
	n := defaultLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("lines must be a positive integer"))
			return
		}
		n = parsed
	}
	if n > maxLogLines {
		n = maxLogLines
	}
	lines, err := source.Tail(r.Context(), n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, logLinesResponse{Lines: lines})
}
