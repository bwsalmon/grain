package main

// grain/task-172: the daemon's half of "test this credential" -- the
// adapter Settings' Capabilities tab reaches a real provider through.
// What matters here is which questions it answers itself and which it
// passes on: it decides whether this deployment has a provider for the
// capability at all, and everything past that is the provider's own
// sentence carried back unchanged.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/capability/gcpkey"
)

// noCredentials is a resolver with nothing in it -- a deployment where
// the secret was never pasted. It fails before any provider reaches a
// network, which is what keeps these tests offline.
type noCredentials struct{}

func (noCredentials) Resolve(ctx context.Context, name string) (string, error) {
	return "", errors.New("no such credential: " + name)
}

func testLiveConfig(cfg config) *liveConfig {
	return newLiveConfig(nil, nil, cfg, nil)
}

// A capability this deployment never configured has no provider
// registered for it (capabilityProviders gates gcp-key on both GCP
// fields), and the honest answer is that there is nothing here to test
// -- not a fault, and not silence.
func TestCapabilityCheckSaysWhenNothingIsConfigured(t *testing.T) {
	adapter := capabilityCheckAdapter{live: testLiveConfig(config{}), creds: noCredentials{}}

	_, err := adapter.CheckCapability(context.Background(), "gcp-key")
	if err == nil {
		t.Fatal("expected a check against an unconfigured capability to say so")
	}
	if !strings.Contains(err.Error(), "gcp-key") || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q does not say which capability is unconfigured", err)
	}
}

// Once it is configured, the provider answers -- and its answer is
// carried back as it stands, including the name of the credential it
// tried to authenticate as, which is what the pane reporting a refusal
// tells an operator to replace.
func TestCapabilityCheckCarriesTheProvidersOwnAnswer(t *testing.T) {
	adapter := capabilityCheckAdapter{
		live: testLiveConfig(config{
			gcpProject:             "example-project",
			gcpServiceAccountEmail: "agent@example-project.iam.gserviceaccount.com",
		}),
		creds: noCredentials{},
	}

	result, err := adapter.CheckCapability(context.Background(), "gcp-key")
	if err == nil {
		t.Fatal("expected a check with no minter secret stored to fail")
	}
	if !strings.Contains(err.Error(), gcpkey.DefaultMinterCredential) {
		t.Errorf("error %q does not name the credential that could not be resolved", err)
	}
	if len(result.Credentials) != 1 || result.Credentials[0] != gcpkey.DefaultMinterCredential {
		t.Errorf("Credentials = %v, want the credential named even on the failure path", result.Credentials)
	}
}

// The registry is rebuilt per call out of the live configuration, so a
// project set in Settings a moment ago is the project the next check
// runs against -- a registry captured at startup would go on checking
// one an operator has already replaced.
func TestCapabilityCheckFollowsALaterSettingsChange(t *testing.T) {
	live := testLiveConfig(config{})
	adapter := capabilityCheckAdapter{live: live, creds: noCredentials{}}

	if _, err := adapter.CheckCapability(context.Background(), "gemini-key"); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("with no project set, err = %v, want the unconfigured answer", err)
	}

	live.mu.Lock()
	live.applied.gcpProject = "example-project"
	live.mu.Unlock()

	_, err := adapter.CheckCapability(context.Background(), "gemini-key")
	if err == nil {
		t.Fatal("expected the check to reach gemini-key's provider once a project is set")
	}
	if strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q still says the capability is unconfigured after the project was set", err)
	}
	if !strings.Contains(err.Error(), gcpkey.DefaultMinterCredential) {
		t.Errorf("error %q is not the provider's own answer about its minter credential", err)
	}
}

// Capabilities that hold no standing credential are refused here as well
// as in pkg/ui: nothing to authenticate as means nothing a check could
// report on.
func TestCapabilityCheckRefusesACapabilityWithNoCredential(t *testing.T) {
	adapter := capabilityCheckAdapter{live: testLiveConfig(config{}), creds: noCredentials{}}
	if _, err := adapter.CheckCapability(context.Background(), "self-debug"); err == nil {
		t.Fatal("expected self-debug, which holds no credential, to be refused")
	}
}
