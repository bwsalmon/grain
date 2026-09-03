package ui

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/bwsalmon/grain/pkg/model"
)

// The named GitHub tokens half of Settings (grain/task-137).
//
// grain/task-117 made every named token beyond the deployment default a
// capability of its own, so a task could be told to push through a
// second one -- but *defining* one was still host-only work: write
// <name>.token into $GRAIN_DATA_DIR/secrets/github and restart. These
// handlers are that work, moved into the pane where every other
// deployment-wide credential is already set.
//
// They write files, not secrets-store rows, which is the decision
// pkg/gitproxy/credentials.go's own doc comment records at length: the
// git proxy reads one thing, and this is the one thing. That is also why
// this is its own endpoint rather than a shape over Config.Secrets the
// way agent_keys.go is -- the store behind it is the credential ladder
// itself (Config.Credentials), whose availability is a separate question
// from the secrets database's.
//
// Everything here is write-only in the same sense the rest of the
// Settings pane is: a token goes in, presence and names come back, and
// no response ever carries a value.

// errGitHubTokensUnavailable is what these handlers report when
// Config.Credentials is nil -- this UI was not told where a GitHub
// credential ladder lives, which is the normal case for `grain demo`'s
// throwaway UI and never for `grain daemon`'s own in-process one. Mapped
// to 404, the same as the secrets pane's own unavailability.
var errGitHubTokensUnavailable = errors.New(
	"GitHub tokens are not available: this UI has no local GitHub credential directory to write to")

// gitHubTokenInfo is one credential in this deployment's ladder.
//
// Present and Offered are deliberately two fields rather than one:
// Present is what is on disk right now, Offered is what this *running
// process* turned into a picker row and a capability provider at startup
// (Config.Capabilities), and the whole reason a newly added token needs
// a restart is that those two can disagree. NeedsRestart below is that
// disagreement, named once so the frontend does not have to re-derive
// it.
type gitHubTokenInfo struct {
	Name string `json:"name"`
	// App is a credential backed by <name>.app.json -- a GitHub App
	// installation, whose three values are not something this pane can
	// write, so it is listed and never edited here.
	App bool `json:"app"`
	// Default is the credential credentials.json's "*" entry names: what
	// every repo with no narrower entry is pushed with, and so the one
	// token that is not a capability (a task needs no grant to be using
	// it already).
	Default bool `json:"default"`
	// Patterns is every credentials.json pattern naming this credential,
	// which is what makes deleting one a question rather than a click:
	// removing a credential a pattern still names leaves those repos
	// failing closed on their next push.
	Patterns []string `json:"patterns,omitempty"`
	// Capability is the id a task holds to push through this token,
	// empty for the deployment default (which needs none).
	Capability string `json:"capability,omitempty"`
	// Present is whether a credential file of this name exists.
	Present bool `json:"present"`
	// Offered is whether this process is offering it as a capability.
	Offered bool `json:"offered"`
	// NeedsRestart is Present and Offered disagreeing: a token added
	// since this process started (present, not yet offered) or removed
	// since (offered, no longer present). The ladder is deliberately not
	// hot-reloaded, so this is not a bug to fix, it is a thing to tell
	// whoever just added a token.
	NeedsRestart bool `json:"needsRestart"`
}

// gitHubTokensResponse is GET /api/github-tokens' body, and what setting
// or removing one answers with afterward -- the same
// respond-with-the-current-shape convention every other mutation in this
// package follows.
type gitHubTokensResponse struct {
	Enabled bool `json:"enabled"`
	// Dir is where these files live, for the pane to name when it
	// explains what it just wrote -- a path, never any content of one.
	Dir    string            `json:"dir,omitempty"`
	Tokens []gitHubTokenInfo `json:"tokens"`
	// RestartRequired is any row's NeedsRestart, so the pane can show one
	// banner rather than reading every row to find out whether to.
	RestartRequired bool `json:"restartRequired"`
}

// gitHubTokens is the current ladder as this pane reports it: every
// credential on disk, plus any this process still offers that has since
// been deleted, sorted by name.
func (c *Client) gitHubTokens() []gitHubTokenInfo {
	credentials := c.Config.Credentials
	if credentials == nil {
		return nil
	}
	present := credentials.Names()
	offered := map[string]bool{}
	for _, capability := range c.Config.Capabilities {
		if name, ok := model.GitCredentialName(capability.ID); ok {
			offered[name] = true
		}
	}
	names := slices.Clone(present)
	for name := range offered {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	defaultName := credentials.DefaultName()
	out := make([]gitHubTokenInfo, 0, len(names))
	for _, name := range names {
		info := gitHubTokenInfo{
			Name:     name,
			Default:  name == defaultName,
			Patterns: credentials.PatternsFor(name),
			Present:  slices.Contains(present, name),
			Offered:  offered[name],
		}
		if info.Present {
			info.App = credentials.IsApp(name)
		}
		// The default credential is served by the ladder itself, so it
		// has no capability id and nothing about it waits on a restart
		// to become grantable -- replacing its *value* still does, which
		// is what setGitHubToken's own reply says.
		if !info.Default {
			info.Capability = model.GitCredentialCapability(name)
			info.NeedsRestart = info.Present != info.Offered
		}
		out = append(out, info)
	}
	return out
}

func (s *Server) handleListGitHubTokens(w http.ResponseWriter, r *http.Request) {
	s.respondWithGitHubTokens(w)
}

type setGitHubTokenRequest struct {
	Value string `json:"value"`
}

// handleSetGitHubToken writes one named token. The value travels in the
// body, never in the path or a query parameter, for the same reason
// handleSetAgentKey's does: neither belongs anywhere it would be logged.
func (s *Server) handleSetGitHubToken(w http.ResponseWriter, r *http.Request) {
	credentials := s.tasks.Config.Credentials
	if credentials == nil {
		writeError(w, http.StatusNotFound, errGitHubTokensUnavailable)
		return
	}
	var req setGitHubTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := credentials.SetToken(r.PathValue("name"), req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.respondWithGitHubTokens(w)
}

// handleDeleteGitHubToken removes one named token's files.
//
// A credential credentials.json still names is refused: the ladder would
// go on selecting it and every push it covers would fail closed with "no
// credential configured" (gitproxy.CredentialSet.Select), which is a
// deployment-wide outage bought with one click. The pattern file is not
// editable from here, so the error says where to go instead of quietly
// rewriting a file this pane does not own.
func (s *Server) handleDeleteGitHubToken(w http.ResponseWriter, r *http.Request) {
	credentials := s.tasks.Config.Credentials
	if credentials == nil {
		writeError(w, http.StatusNotFound, errGitHubTokensUnavailable)
		return
	}
	name := r.PathValue("name")
	if patterns := credentials.PatternsFor(name); len(patterns) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"credentials.json still maps %v to %q: remove that entry on the host first, "+
				"or every repo those patterns cover fails its next push", patterns, name))
		return
	}
	if err := credentials.Remove(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.respondWithGitHubTokens(w)
}

func (s *Server) respondWithGitHubTokens(w http.ResponseWriter) {
	credentials := s.tasks.Config.Credentials
	if credentials == nil {
		writeJSON(w, http.StatusOK, gitHubTokensResponse{Enabled: false, Tokens: []gitHubTokenInfo{}})
		return
	}
	tokens := s.tasks.gitHubTokens()
	restart := false
	for _, token := range tokens {
		restart = restart || token.NeedsRestart
	}
	writeJSON(w, http.StatusOK, gitHubTokensResponse{
		Enabled:         true,
		Dir:             credentials.Dir(),
		Tokens:          tokens,
		RestartRequired: restart,
	})
}
