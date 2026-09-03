package deploy

// The deployment path for the state repository (pkg/staterepo) and the
// secrets key it carries: a Terraform variable, an instance-metadata
// attribute, a `cfg` read, an environment variable and a file write, in
// five different files, none of which can see the other four. Exactly
// what this package exists to check -- a variable declared and never
// passed on, or an attribute pushed under a name nothing reads back,
// fails silently and looks like a working deploy.

import (
	"strings"
	"testing"
)

// A host should not have to be visited to find out where its own state
// lives.
func TestTheStateRepositoryIsConfiguredByTheDeployRatherThanByHand(t *testing.T) {
	variables := read(t, "terraform", "gcp", "variables.tf")
	for _, v := range []string{`variable "state_repo_url"`, `variable "state_repo_branch"`} {
		contains(t, variables, v)
	}

	// Non-secret configuration travels in grain-config, like every other
	// knob -- a URL and a branch name are neither of them secrets.
	instance := read(t, "terraform", "gcp", "instance.tf")
	contains(t, instance, "state_repo_url            = var.state_repo_url")
	contains(t, instance, "state_repo_branch         = var.state_repo_branch")

	deploy := stripComments(read(t, "terraform", "gcp", "files", "deploy.sh"))
	contains(t, deploy, `STATE_REPO_URL="$(cfg state_repo_url)"`)
	contains(t, deploy, `STATE_REPO_BRANCH="$(cfg state_repo_branch)"`)
	contains(t, deploy, `GRAIN_STATE_REPO_URL="$STATE_REPO_URL"`)
	contains(t, deploy, `GRAIN_STATE_REPO_BRANCH="$STATE_REPO_BRANCH"`)

	// And the far end: the file the daemon actually reads, which is the
	// one piece of configuration that cannot live in the repository
	// because it is what says where the repository is.
	setup := setupCode(t)
	contains(t, setup, "state-repo.json")
	contains(t, setup, `if [ -n "$GRAIN_STATE_REPO_URL" ]; then`)
}

// The secrets key is seeded the way GRAIN_GITHUB_TOKEN and the minter
// key already are -- an attribute pushed after the apply, read back off
// the metadata server, and written once.
func TestTheSecretsKeyIsSeededTheWayEveryOtherCredentialIs(t *testing.T) {
	push := stripComments(read(t, "terraform", "gcp", "deploy", "push-secrets.sh"))
	contains(t, push, `push_secret "grain-secrets-key" "${GRAIN_SECRETS_KEY:-}"`)

	// Without this an apply would read the pushed key back as drift and
	// erase it, leaving the next deploy seeding nothing.
	instance := read(t, "terraform", "gcp", "instance.tf")
	contains(t, instance, `metadata["grain-secrets-key"],`)

	deploy := stripComments(read(t, "terraform", "gcp", "files", "deploy.sh"))
	contains(t, deploy, `SECRETS_KEY="$(md_optional instance/attributes/grain-secrets-key)"`)
	contains(t, deploy, `GRAIN_SECRETS_KEY="$SECRETS_KEY"`)

	// seed_secret, not a bare write: a key already on this host is the
	// key that host's own secrets file was encrypted to, so it always
	// wins over whatever a deploy carries.
	contains(t, setupCode(t),
		`seed_secret "$GRAIN_DATA_DIR/secrets/secrets.key" "$GRAIN_SECRETS_KEY"`)
}

// Ordering is the whole of this: both of these steps have to happen
// before anything runs the `grain` CLI against the store.
//
// Opening a store with no key mints one (pkg/secrets.Open), so a host
// rebuilt with a key to seed would otherwise end up holding one it
// generated a moment earlier -- and the encrypted secrets file lives
// inside the state repository's working tree, so the tree has to be the
// cloned one before seed_gcp_minter_key writes this deploy's freshly
// rotated minter key into it.
func TestTheSecretsKeyAndTheStateRepositoryComeBeforeAnythingOpensTheStore(t *testing.T) {
	code := setupCode(t)
	// "\n  configure_state_repo\n" is the call rather than the definition,
	// which a shell file necessarily carries above it.
	before(t, code,
		`seed_secret "$GRAIN_DATA_DIR/secrets/secrets.key"`, "\n  configure_state_repo\n",
		"the secrets key must be seeded before the state repository is opened")
	before(t, code,
		"\n  configure_state_repo\n", "seed_gcp_minter_key\n",
		"the state repository must be opened before a secret is written into its working tree")
	before(t, code,
		`seed_secret "$GRAIN_DATA_DIR/secrets/secrets.key"`, "seed_gcp_minter_key\n",
		"the secrets key must be seeded before `grain secrets` mints one of its own")
}

// "You have not backed this up" belongs where an operator is already
// looking, which is the readiness summary at the end of a run.
func TestTheReadinessSummaryNamesTheSecretsKeyAndTheStateRepository(t *testing.T) {
	readiness := body(t, setupText(t), "report_readiness() {")
	contains(t, readiness, "secrets key:")
	contains(t, readiness, "state repository:")
	if !strings.Contains(readiness, "back up") {
		t.Error("the readiness summary never says to back the secrets key up")
	}
	// Presence asked of the file, because `grain state key show` would
	// mint a key on a host that has none -- a readiness report must not
	// create what it reports on.
	contains(t, readiness, `[ -s "$GRAIN_DATA_DIR/secrets/secrets.key" ]`)

	// And the operator's own handle on the same thing, for a host that is
	// long past its deploy.
	contains(t, read(t, "cmd", "grain", "state.go"), "GRAIN_SECRETS_KEY")
}
