package ui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bwsalmon/grain/pkg/agent/credcheck"
	"github.com/bwsalmon/grain/pkg/secrets"
)

// The agent credentials this pane manages: one per agent.Framework grain
// can drive a run with, each stored in this deployment's own secrets
// database under the well-known name cmd/grain's daemon resolves it by
// (secrets.GeminiAPIKeySecret/ClaudeOAuthTokenSecret/OpenAIAPIKeySecret)
// before every dispatch.
//
// They are ordinary secrets, so the Secrets pane this predates could
// already set them by hand -- but only by knowing both the exact secret
// name and the key inside it, with nothing anywhere saying which names
// those are or whether the framework an operator just switched to has a
// credential at all. These handlers are that knowledge, moved into the
// one pane where the framework itself is chosen: set a key, clear a key,
// and report which of them are set, never what they hold (the same
// write-only contract secrets.Store.List gives everything else here).
//
// grain/task-110 took the same argument the rest of the way: every
// secret a capability resolves is reported beside that capability
// (CapabilityStatus.Secrets) and set from there, and the flat Secrets
// tab is gone. These two stay their own endpoints rather than joining
// that listing -- an agent credential belongs to a framework, not to a
// capability, and there is no CapabilitySpec.Requires naming it.
//
// Deliberately not model.Config fields: a credential is not
// configuration, and nothing that reaches the store's config row is
// write-only the way this must be.
// The mapping itself lives in pkg/agent/credcheck, which needs the same
// three names to say which secret a refused credential is stored in
// (agent_key_check.go beside this). One copy, so a fourth framework
// cannot be settable here and untestable there -- it normalizes the
// legacy "gemini" spelling on the way in exactly as this always did.
func agentKeySecret(framework string) (string, bool) {
	return credcheck.SecretFor(framework)
}

// agentKeysResponse is GET /api/agent-keys' body, and what setting or
// clearing one returns afterward -- the same respond-with-the-current-
// shape convention the secrets pane's own handlers follow. Enabled is
// false, with every flag false, when this UI has no local secrets
// directory to write to (Config.Secrets nil), so the pane can say so
// rather than offer a control that could only ever 404.
type agentKeysResponse struct {
	Enabled bool `json:"enabled"`
	// These report presence exactly as the daemon will find it: set
	// means the secret exists and resolves (secrets.Store.Resolve's
	// sole-key form), not merely that something of that name is in the
	// database.
	GeminiAPIKeySet     bool `json:"geminiApiKeySet"`
	ClaudeOAuthTokenSet bool `json:"claudeOAuthTokenSet"`
	OpenAIAPIKeySet     bool `json:"openaiApiKeySet"`
}

// agentKeysSet reports which agent credentials this deployment has,
// leaving every flag false when there is no secrets store to ask (`grain
// demo`'s throwaway UI) or when listing it fails -- best-effort, the
// same reading capabilityStatuses gives the same listing, since a
// Settings response has never failed on this and an operator is better
// served by a pane that loads and says "not set" than by one that does
// not load.
func (c *Client) agentKeysSet() (gemini, claude, openai bool) {
	if c.Config.Secrets == nil {
		return false, false, false
	}
	list, err := c.Config.Secrets.List()
	if err != nil {
		return false, false, false
	}
	resolvable := func(secret string) bool {
		return len(missingSecretsFor([]string{secret}, list)) == 0
	}
	return resolvable(secrets.GeminiAPIKeySecret),
		resolvable(secrets.ClaudeOAuthTokenSecret),
		resolvable(secrets.OpenAIAPIKeySecret)
}

func (s *Server) handleListAgentKeys(w http.ResponseWriter, r *http.Request) {
	s.respondWithAgentKeys(w)
}

type setAgentKeyRequest struct {
	Value string `json:"value"`
}

// handleSetAgentKey writes one framework's credential. The value travels
// in the body, never in the path or a query parameter, for the same
// reason claude.WithOAuthToken passes a token through the environment
// rather than argv: neither belongs anywhere it would be logged.
func (s *Server) handleSetAgentKey(w http.ResponseWriter, r *http.Request) {
	store := s.tasks.Config.Secrets
	if store == nil {
		writeError(w, http.StatusNotFound, errSecretsUnavailable)
		return
	}
	secret, ok := agentKeySecret(r.PathValue("framework"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no agent framework named "+r.PathValue("framework")))
		return
	}
	var req setAgentKeyRequest
	if !readJSON(w, r, &req) {
		return
	}
	// Trimmed on the way in, not on the way out: a token pasted out of a
	// terminal or an email carries whatever whitespace came with it, and
	// the daemon trims what it reads back anyway (cmd/grain's
	// agentCredential). Doing it here means "set" and "resolves to
	// something" cannot disagree -- a value of nothing but whitespace
	// would otherwise store as present and fail every run.
	value := strings.TrimSpace(req.Value)
	if value == "" {
		writeError(w, http.StatusBadRequest, errors.New("value is required"))
		return
	}
	if err := store.Set(secret, secrets.AgentCredentialKey, []byte(value)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.respondWithAgentKeys(w)
}

// handleDeleteAgentKey clears one framework's credential. Deleting the
// whole secret, not just AgentCredentialKey inside it, so a secret left
// holding no keys cannot linger and read back as "exists but does not
// resolve" -- the one state agentKeysSet reports as unset and
// secrets.Store.Resolve refuses, which is a confusing way for a cleared
// key to look.
func (s *Server) handleDeleteAgentKey(w http.ResponseWriter, r *http.Request) {
	store := s.tasks.Config.Secrets
	if store == nil {
		writeError(w, http.StatusNotFound, errSecretsUnavailable)
		return
	}
	secret, ok := agentKeySecret(r.PathValue("framework"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no agent framework named "+r.PathValue("framework")))
		return
	}
	if err := store.DeleteSecret(secret); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.respondWithAgentKeys(w)
}

func (s *Server) respondWithAgentKeys(w http.ResponseWriter) {
	gemini, claude, openai := s.tasks.agentKeysSet()
	writeJSON(w, http.StatusOK, agentKeysResponse{
		Enabled:             s.tasks.Config.Secrets != nil,
		GeminiAPIKeySet:     gemini,
		ClaudeOAuthTokenSet: claude,
		OpenAIAPIKeySet:     openai,
	})
}
