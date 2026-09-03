package main

// `grain settings` is the only place a shell on a deployment's own host
// can ask why a capability a task was granted never did anything. These
// cover the line it prints per capability -- in particular that the two
// kinds of gap stay distinguishable, since "this deployment is missing a
// secret" and "grain's own code offers no way to grant this" are fixed
// in different places and reading one as the other is what makes a
// configured-but-ungrantable gcp-key look like a configuration problem.

import (
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCapabilityStatusLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  ui.CapabilityStatus
		want    []string
		wantNot []string
	}{
		{
			name:    "ready and grantable says only that",
			status:  ui.CapabilityStatus{ID: "gemini-key", Ready: true, Grantable: true},
			want:    []string{"gemini-key", "ready"},
			wantNot: []string{"not ready", "NOT GRANTABLE", "needs:", "missing secrets:"},
		},
		{
			name: "an unconfigured capability names what it needs",
			status: ui.CapabilityStatus{
				ID: "gcp-key", Grantable: true,
				MissingConfig:  []string{"GCP project", "GCP service account email"},
				MissingSecrets: []string{"gcp-key-minter"},
			},
			want:    []string{"gcp-key", "not ready", "needs: GCP project, GCP service account email", "missing secrets: gcp-key-minter"},
			wantNot: []string{"NOT GRANTABLE"},
		},
		{
			// The case this exists for: nothing is missing, so every
			// other signal available reads "working".
			name:    "a ready capability nothing can grant still says so",
			status:  ui.CapabilityStatus{ID: "gcp-key", Ready: true, Grantable: false},
			want:    []string{"gcp-key", "ready", "NOT GRANTABLE"},
			wantNot: []string{"needs:", "missing secrets:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilityStatusLine(tc.status)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("line %q does not contain %q", got, want)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("line %q unexpectedly contains %q", got, unwanted)
				}
			}
		})
	}
}
