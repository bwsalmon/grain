package gitproxy

// The proxy's decision logic -- independent of the HTTP transport.
// Order matters and mirrors docs/design.md's own spec:
//
//	match the four legal smart-HTTP paths,
//	authenticate the caller by per-sandbox token,
//	check the caller's live task scope against (owner, repo), default-deny,
//	select the credential -- the caller's own named override
//	  (bwsalmon/agents#52) if its task carries one, the owner/repo ladder
//	  otherwise -- and set Authorization,
//	stream the body through, and log the tuple.
//
// Authentication is checked before authorization so an unauthenticated
// caller learns nothing about what any sandbox is scoped to -- a 401 with
// nothing else leaked, same as a real git server's own behavior toward an
// unauthenticated client.

import (
	"context"
	"fmt"
	"time"
)

// ProxyResponse is what Handle decided to answer with.
type ProxyResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// GitProxy is the proxy's whole decision logic, wired to its
// dependencies. Now is injected rather than read from time.Now() so
// core_test.go can assert on audit timestamps deterministically.
type GitProxy struct {
	Authorizer  Authorizer
	Credentials *CredentialSet
	Tokens      *SandboxTokens
	Forwarder   Forwarder
	Audit       AuditLog
	Now         func() time.Time
	// CredentialOverrides resolves bwsalmon/agents#52's per-task named
	// credential override, if any. nil (every existing caller/test)
	// leaves every request on the ordinary owner/repo ladder, the same
	// as an override lookup that always answered false.
	CredentialOverrides CredentialOverrideLookup
}

func (p *GitProxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *GitProxy) audit() AuditLog {
	if p.Audit != nil {
		return p.Audit
	}
	return NullAuditLog{}
}

// Handle decides how to answer one request and, if it is allowed,
// forwards it -- the entire proxy, as one call.
func (p *GitProxy) Handle(ctx context.Context, method, path, query string, headers map[string]string, body []byte) ProxyResponse {
	req, ok := ParsePath(path)
	if !ok {
		return ProxyResponse{Status: 404, Body: []byte("not found")}
	}

	if !IsValidGitRequest(headers["User-Agent"], headers["Accept"], req.Action) {
		return ProxyResponse{Status: 404, Body: []byte("not found")}
	}

	token, hasToken := ExtractBasicAuthToken(headers["Authorization"])
	var sandbox string
	if hasToken {
		sandbox, ok = p.Tokens.Authenticate(token)
	}
	if !hasToken || !ok {
		return ProxyResponse{
			Status:  401,
			Headers: map[string]string{"WWW-Authenticate": `Basic realm="grain-git-proxy"`},
			Body:    ErrPkt("authentication required"),
		}
	}

	allowed, err := p.Authorizer.Authorize(ctx, sandbox, req.Owner, req.Repo, req.Action)
	if err != nil {
		p.audit().Record(AuditEntry{
			Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
			Action: req.Action, Outcome: fmt.Sprintf("error: %v", err),
		})
		return ProxyResponse{Status: 500, Body: ErrPkt("authorization check failed")}
	}
	if !allowed {
		p.audit().Record(AuditEntry{
			Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
			Action: req.Action, Outcome: "denied: not in scope",
		})
		return ProxyResponse{
			Status: 403,
			Body:   ErrPkt(fmt.Sprintf("%s/%s is not in scope for this sandbox", req.Owner, req.Repo)),
		}
	}

	credential, notConfigured, err := p.selectCredential(ctx, sandbox, req.Owner, req.Repo)
	if err != nil {
		p.audit().Record(AuditEntry{
			Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
			Action: req.Action, Outcome: fmt.Sprintf("error: %v", err),
		})
		return ProxyResponse{Status: 500, Body: ErrPkt("resolving credential override failed")}
	}
	if notConfigured != "" {
		p.audit().Record(AuditEntry{
			Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
			Action: req.Action, Outcome: "error: " + notConfigured,
		})
		return ProxyResponse{Status: 500, Body: ErrPkt(notConfigured)}
	}

	upstream, err := p.Forwarder.Forward(
		method, fmt.Sprintf("/%s/%s.git/%s", req.Owner, req.Repo, req.Action),
		query, headers, body, credential.Token,
	)
	if err != nil {
		p.audit().Record(AuditEntry{
			Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
			Action: req.Action, Credential: credential.Name,
			Outcome: fmt.Sprintf("error: forwarding failed: %v", err),
		})
		return ProxyResponse{Status: 502, Body: ErrPkt("upstream request failed")}
	}
	p.audit().Record(AuditEntry{
		Time: p.now(), Sandbox: sandbox, Owner: req.Owner, Repo: req.Repo,
		Action: req.Action, Credential: credential.Name,
		Outcome: fmt.Sprintf("forwarded: %d", upstream.Status),
	})
	return ProxyResponse{Status: upstream.Status, Headers: upstream.Headers, Body: upstream.Body}
}

// selectCredential is which credential a request should forward with.
// notConfigured is a non-empty, request-safe message (never err) for the
// two "nothing here" business outcomes -- an override naming a credential
// this deployment has no `<name>.token` for, or no ladder entry covering
// (owner, repo) -- both of which get logged and returned to the caller
// exactly as CredentialSet's own docstring already promises. err is
// reserved for a genuine failure to resolve the override itself.
func (p *GitProxy) selectCredential(ctx context.Context, sandbox, owner, repo string) (credential Credential, notConfigured string, err error) {
	if p.CredentialOverrides != nil {
		name, overridden, err := p.CredentialOverrides.GitCredentialOverride(ctx, sandbox)
		if err != nil {
			return Credential{}, "", fmt.Errorf("resolving credential override for %s: %w", sandbox, err)
		}
		if overridden {
			// A grain-github-<name> label names a credential explicitly --
			// it replaces the owner/repo ladder entirely rather than
			// narrowing it, since the whole point is a scope the ladder's
			// own credentials deliberately don't carry.
			cred, ok := p.Credentials.Get(name)
			if !ok {
				return Credential{}, fmt.Sprintf("no credential named %q configured", name), nil
			}
			return cred, "", nil
		}
	}
	cred, ok := p.Credentials.Select(owner, repo)
	if !ok {
		return Credential{}, "no credential configured for this repository", nil
	}
	return cred, "", nil
}
