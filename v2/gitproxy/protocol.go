// Package gitproxy is the only path from a sandbox to GitHub: docs/design.md's
// git proxy, ported from grain/proxy (Python) to sit alongside the rest of
// v2. The decision logic is unchanged in spirit -- match one of the four
// legal smart-HTTP paths, authenticate the caller by its per-sandbox
// bearer token, decide whether it may touch (owner, repo), select a
// credential, stream the request through, and audit the tuple -- but one
// piece moves: which repos a sandbox may touch is no longer a hand-edited
// allowlist file. It is read live from v2/model's task store (Authorizer,
// in authorize.go), since a running task's Target and Reads already say
// exactly that, and a file an operator maintains by hand can only drift
// from it.
//
// Ref-level policy (no push to main, no force-push) is still left to
// GitHub rulesets, for the same reason docs/design.md gives: a hand-rolled
// pack parser subtly wrong fails open, so this package stays confined to
// path matching and the error encoding a git client already understands.
package gitproxy

import (
	"fmt"
	"regexp"
	"strings"
)

// pathRE matches the four legal smart-HTTP paths, e.g.
// /owner/repo.git/info/refs.
var pathRE = regexp.MustCompile(
	`^/(?P<owner>[^/]+)/(?P<repo>[^/]+?)(?:\.git)?/` +
		`(?P<action>info/refs|git-upload-pack|git-receive-pack)$`)

// GitRequest is one of the four legal smart-HTTP paths, decomposed.
type GitRequest struct {
	Owner  string
	Repo   string
	Action string // "info/refs", "git-upload-pack", or "git-receive-pack"
}

// ParsePath matches path against the four legal smart-HTTP shapes. The
// second return is false if it isn't one of them.
func ParsePath(path string) (GitRequest, bool) {
	m := pathRE.FindStringSubmatch(path)
	if m == nil {
		return GitRequest{}, false
	}
	req := GitRequest{}
	for i, name := range pathRE.SubexpNames() {
		switch name {
		case "owner":
			req.Owner = m[i]
		case "repo":
			req.Repo = m[i]
		case "action":
			req.Action = m[i]
		}
	}
	return req, true
}

// IsValidGitRequest is a tight allowlist of the client shapes a real git
// client produces -- mirrors what FINOS Git Proxy's validGitRequest()
// checks (docs/design.md says it's worth stealing): a git/ user agent,
// and, for the pack endpoints, an Accept header naming the matching
// x-git-* result type. Rejecting anything else means a browser poking at
// these paths gets a plain 404 rather than a confusing partial response.
func IsValidGitRequest(userAgent, accept, action string) bool {
	if !strings.HasPrefix(userAgent, "git/") {
		return false
	}
	if action == "info/refs" {
		return true // git sends Accept: */* here; nothing more to check
	}
	expected := fmt.Sprintf("application/x-%s-result", action)
	return strings.Contains(accept, expected)
}

// PktLine encodes one pkt-line: a 4-hex-digit length prefix, then the data.
func PktLine(data []byte) []byte {
	length := len(data) + 4
	return append([]byte(fmt.Sprintf("%04x", length)), data...)
}

// FlushPkt is the pkt-line flush marker.
var FlushPkt = []byte("0000")

// ErrPkt is a single ERR pkt-line -- the encoding every git client already
// knows how to surface as an abort message, instead of the opaque "the
// remote end hung up unexpectedly" a plain connection drop produces.
func ErrPkt(message string) []byte {
	return PktLine([]byte("ERR " + message + "\n"))
}
