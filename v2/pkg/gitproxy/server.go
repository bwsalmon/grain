package gitproxy

// Wires GitProxy's decision logic to a real HTTP server. net/http is
// enough for a proxy whose whole job is decide-then-stream, no routing
// framework needed.

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// NewHandler wraps proxy as an http.Handler: split the path and query,
// read the body, run the decision logic, and stream the response back --
// recomputing Content-Length and dropping the hop-by-hop headers a
// decision about an upstream response could get wrong for this
// transport's own.
func NewHandler(proxy *GitProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.ContentLength != 0 {
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "reading request body", http.StatusBadRequest)
				return
			}
		}
		headers := map[string]string{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}

		resp := proxy.Handle(r.Context(), r.Method, r.URL.Path, r.URL.RawQuery, headers, body)

		for k, v := range resp.Headers {
			switch strings.ToLower(k) {
			case "content-length", "connection", "transfer-encoding":
				continue
			}
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		w.Write(resp.Body)
	})
}

// BuildConfig is what BuildProxy needs to wire the real files under a
// /data-shaped directory (docs/design.md, "secrets on /data") to the live
// model store that now answers what a hand-edited allowlist file used to.
type BuildConfig struct {
	// DataDir holds secrets/github/{credentials.json,*.token} and
	// secrets/sandbox-tokens.json, and receives state/git-proxy/audit.log.
	DataDir string
	// Store answers GitScope for the sandbox a request authenticates as.
	Store *model.Store
	// ForwardHost overrides the real github.com forward target -- the
	// git-transport half of a mock-GitHub live-test seam. Empty means
	// "github.com over TLS".
	ForwardHost string
	ForwardTLS  bool
}

// BuildProxy wires the real files under DataDir plus the live model store
// into a GitProxy ready to serve.
func BuildProxy(cfg BuildConfig) (*GitProxy, error) {
	credentials, err := LoadCredentialSet(filepath.Join(cfg.DataDir, "secrets", "github"))
	if err != nil {
		return nil, err
	}
	tokens, err := LoadSandboxTokens(filepath.Join(cfg.DataDir, "secrets", "sandbox-tokens.json"))
	if err != nil {
		return nil, err
	}
	audit, err := NewFileAuditLog(filepath.Join(cfg.DataDir, "state", "git-proxy", "audit.log"))
	if err != nil {
		return nil, err
	}

	host, useTLS := cfg.ForwardHost, cfg.ForwardTLS
	if host == "" {
		host, useTLS = "github.com", true
	}

	return &GitProxy{
		Authorizer:          &ModelAuthorizer{Store: cfg.Store},
		Credentials:         credentials,
		Tokens:              tokens,
		Forwarder:           &RealForwarder{Host: host, UseTLS: useTLS},
		Audit:               audit,
		CredentialOverrides: cfg.Store,
	}, nil
}
