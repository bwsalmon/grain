package grain

import "fmt"

// Spec is the whole declaration of one grain's work, delivered as the
// container's environment and mount at create and never updated: a grain
// is configured once and then only observed, answered, or destroyed. See
// Env, Files and SpecFromEnv for how it
// crosses, and env.go's own comment for why it crosses that way rather
// than as a document on some stdin.
//
// It is deliberately small, and what it leaves out is the point: **a
// grain knows how to run an agent in a sandbox, and nothing about why.**
// There is no task here, no repository, no branch, no git credential and
// no capability model, because a grain has no use for any of them. Every
// one of those reaches it in one of three shapes instead:
//
//   - in the prompt, which the controller assembles from its store and
//     writes to FilePrompt at create;
//   - in Setup, a script the controller composes -- the clone included,
//     since a clone is git commands in the guest and nothing more;
//   - in Placements, which is where a credential goes, git's among them.
//
// That boundary is worth more than the fields it saves. A shim that
// understood repositories would have to agree with the controller about
// branch naming, proxy URLs, retry on a half-made checkout and what a
// task is -- an interface between two separately released artifacts,
// carrying grain's whole task model across it. A shim that runs a script
// and places files has no opinions to keep in sync.
//
// It is also what makes a rebuild need no controller: everything a fresh
// guest has to be given back is right here, beside the thing being
// rebuilt.
type Spec struct {
	// Version is the wire format this Spec is written to. See Version's
	// own doc comment for what a receiver does with one it does not
	// recognise.
	Version string `json:"version"`

	// There is no id here. The container is the identity, and a grain is
	// never told a name it makes no use of.

	// Framework is which agent to run, and the credential it runs as.
	Framework FrameworkSpec `json:"framework"`

	// Shape is the guest this grain gets. A create-time argument and
	// nothing resizes it: the root disk is a qcow2 overlay made with the
	// VM, and a grain lives exactly as long as one run.
	//
	// Grain never interprets these numbers. Env renders them as kontur's
	// own CHV_CPUS/CHV_MEMORY_MB/CHV_DISK_SIZE_MB and the VMM reads them
	// itself, so they pass through in kontur's vocabulary; SpecFromEnv
	// does not read them back, because a second opinion about numbers
	// this side does not act on is worth nothing.
	Shape Shape `json:"shape"`

	// Prompt is the agent's opening prompt, assembled by the controller
	// out of its store: the task, its conversation, the previous
	// attempts, the deployment's and the repo's prompt extensions. A
	// grain neither reads nor understands it -- it hands it to whichever
	// CLI Framework names.
	//
	// This is where everything task-shaped ends up that is not a file or
	// a tool, and it is why a Spec needs no task, no repo and no branch.
	Prompt string `json:"prompt,omitempty"`

	// Setup is a script run in the guest once it answers, before the
	// agent starts. It is opaque to the shim, which runs it and reports
	// its exit code and output (Status.Setup) without reading either.
	//
	// The controller composes it, so it carries whatever that run needs
	// doing: the clone, the branch checkout, the repo's own setup
	// command. That is why there is no repo in this Spec -- a clone is
	// git commands, and the controller already knows the proxy URL and
	// the branch (model.BranchName's answer, deterministic and
	// deliberately not something the agent influences).
	//
	// It must never embed a credential. A clone reaches the proxy with a
	// plain URL and git finds its token in the placement beside it, which
	// is why that token is a placement rather than something interpolated
	// into this string. Redacted blanks material and deliberately leaves
	// this alone -- a failed run is diagnosed by reading exactly what its
	// setup tried to do -- so a secret in here is a secret in every log
	// that quotes it.
	//
	// Its exit code is what gates starting the agent, which is why a
	// failed checkout costs no model tokens; its output is the diagnosis
	// for a grain that failed before its agent ever ran. Both reach the
	// controller as Status.Setup, uninterpreted -- the controller wrote
	// the script, so it is the one that knows what its output means.
	Setup string `json:"setup,omitempty"`

	// Placements is everything written into the guest before the agent
	// starts: credentials, and any other file a run needs that the image
	// does not already carry.
	//
	// Into the guest, with no other side to choose. Every capability
	// grain has that places anything places it in the sandbox --
	// githubsandbox, gcpkey and geminikey, all model.SideSandbox -- and
	// each is material the *work* needs: geminikey's own doc comment
	// mints its key for a task and names the path in the prompt so the
	// work can find it. model.SideController exists and nothing produces
	// one; orchestrator/run.go skips it, "not written anywhere". A
	// discriminator whose second value has never occurred is not worth
	// carrying across a versioned wire.
	//
	// The agent's own credential is the case that looks like it wants the
	// other side, and does not: it is deployment-wide rather than
	// per-run -- a Claude Code OAuth token or a Gemini key set once in
	// Settings, which is why Deps.Framework can fail for want of it -- so
	// it reaches the container as configuration at create, beside the
	// framework profile that reads it, and never travels in a Spec at
	// all. The sandbox still cannot read it. That property comes from
	// where the agent runs, not from a field here.
	//
	// Git's credential is one of these, rather than a field of its own.
	// It is the same work pkg/orchestrator does today in
	// ConfigureGitCredentials -- write git's config with a token -- said
	// uniformly, which takes a method off the sandbox interface and a
	// special case out of the setup path. Guest-side, because the clone
	// runs there.
	Placements []Placement `json:"placements,omitempty"`

	// ControllerURL is the controller's MCP endpoint, which the agent
	// reaches directly over Streamable HTTP for every tool that is not a
	// built-in. The shim writes it into its framework's MCP config and
	// otherwise takes no part: an MCP client talks to several servers as
	// a matter of course, so the agent simply has two.
	//
	// Empty leaves a grain with its six sandbox tools and no way to reach
	// anything outside -- which is right for a HostGrains test and wrong
	// for a real run.
	ControllerURL string `json:"controllerURL,omitempty"`

	// Token authenticates this grain to that endpoint. It is the *same*
	// per-grain token the git proxy already mints
	// (gitproxy.SandboxTokenStore.EnsureToken), revoked by the same
	// Revoke at reap and resolved to a live run by the same
	// model.Store.GitScope -- one more consumer of machinery that exists,
	// rather than a second authorization surface to build and get wrong.
	//
	// An exec pipe authenticated by construction: the controller chose
	// which container to exec into. An address does not, so something has
	// to say which grain is calling, and this is it. It is also what
	// spares the controller a server instance per grain for identity
	// alone.
	//
	// Container-side, at FileToken -- unlike the git credential, which is
	// the same secret placed in the guest because git runs there. Same
	// value, two consumers, two sides of the vsock boundary.
	Token string `json:"token,omitempty"`

	// MaxRuntime is how long the agent may run before the shim stops it
	// and reports a terminal Phase. Zero means no bound of the grain's
	// own.
	//
	// Decided by the controller, enforced by the grain, and that split is
	// the point rather than an inconsistency with Policy.ProvisionBudget
	// next door. Before there is an agent, only the controller can act --
	// a grain wedged in provisioning is precisely the one that cannot
	// report being wedged, so its budget is enforced from outside. Once
	// there is an agent, the grain can stop it without depending on
	// anybody, and that is worth having: a running agent is spending
	// money, and money should not keep leaving while a controller is
	// down. The two budgets sit at opposite ends of the same run for that
	// reason.
	//
	// The controller does not also enforce it. One rule, one enforcement
	// point -- and the concern Config.MaxRunRuntime's own doc comment
	// names, a stuck run "tying up its share of the concurrency limit",
	// is served anyway: the grain goes terminal, the next poll sees it,
	// and the ordinary finish path frees the slot.
	//
	// It is the only limit here. Turns are a framework's own flag, and
	// Config.MaxAgentTurns' doc comment already concedes both frameworks
	// default to no cap and "what actually bounds a runaway run is
	// MaxRunRuntime". Rebuilds are Policy.MaxRebuilds' alone -- the
	// controller has the view of whether repair is converging.
	//
	// A stopped run reports OutcomeCancelled with the limit named, which
	// is the vocabulary orchestrator already uses for this: run.go
	// records a timed-out run as "cancelled" with model.RuntimeCapDetail,
	// "the run did not fail".
	//
	// What it does not cover, recorded so nobody later assumes it does: a
	// controller that dies five minutes into a two-hour budget still
	// leaves an hour fifty-five of spending. Only a lease -- stop if
	// nobody has polled in a while -- bounds that under *any* controller
	// failure, and it is declined as more mechanism than the failure
	// justifies, with a failure mode of its own in killing healthy grains
	// over a slow controller restart.
	MaxRuntime Duration `json:"maxRuntime,omitempty"`
}

// FrameworkSpec selects an agent profile the sandbox image provides and
// hands it the credential it runs as.
//
// A name and not a configuration, on purpose: how that CLI is launched,
// which flags it takes, where its MCP config has to live, whether it needs
// a private HOME, and -- the reason this type exists rather than a bare
// string -- *where its credential goes* are all facts about the binary,
// and the binary ships in this image.
//
// Today the daemon owns all of it (pkg/agent/claude, /antigravity,
// /codex; see antigravity's own doc comment on agy having no
// --mcp-config, so each run gets a private HOME holding one file). That
// knowledge sits in a different artifact from the CLI it describes, so
// upgrading the CLI can require upgrading the daemon. Moving it into the
// image versions the two together, and makes adding a framework an image
// change rather than a controller release.
//
// A controller can ask which profiles an image has before dispatching to
// one -- VersionReport.Frameworks -- so a task naming a framework this
// image lacks fails at create, naming it, rather than at launch.
type FrameworkSpec struct {
	// Name is the profile: "claude", "antigravity", "codex".
	Name string `json:"name"`
	// Credential is what that agent authenticates to its model API with
	// -- a Claude Code OAuth token, a Gemini API key -- opaque here, and
	// placed by the profile, which is the only thing that knows whether
	// this CLI wants a file at a particular path, an environment
	// variable, or a login already performed.
	//
	// Per-grain rather than baked into the image or set once on the
	// deployment, because which credential a grain needs follows from the
	// framework its *task* chose (model.Task.AgentFramework, and
	// Deps.Framework resolving it): static configuration would mean
	// shipping every deployment's every credential into every container.
	//
	// It reaches the container as its own environment variable
	// (EnvCredential), which on Kubernetes a deployment points at a
	// Secret key with valueFrom.secretKeyRef -- so the pod spec holds a
	// reference and the value keeps the Secret's own RBAC and encryption
	// at rest. Its own variable rather than a key inside the placements
	// blob, so it can be rotated and scoped by itself.
	//
	// It is not a Placement, and that is why removing Placement.Dest cost
	// nothing: a placement is path-addressed, written where the
	// controller says because something else -- a prompt naming
	// geminikey's KeyPath, a git command -- expects it there. This has no
	// path the controller could name, because only the profile knows one.
	//
	// Material, with model.Placement's rule: never logged, never in a
	// prompt, never in an error.
	Credential string `json:"credential,omitempty"`
}

// Shape is how big a guest this grain asked for.
type Shape struct {
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty"`
	DiskGB   int `json:"diskGB,omitempty"`
}

// IsZero reports whether a shape asks for nothing in particular, which is
// what a task with no override produces.
func (s Shape) IsZero() bool { return s.CPUs == 0 && s.MemoryMB == 0 && s.DiskGB == 0 }

// Placement is one file written into the guest. Mode is an octal string
// (model.Placement.EffectiveMode's shape) and must never be widened in
// transit.
//
// Everything placed here is readable by whatever code the repo runs, so
// it should hold only what that code legitimately needs. That is not a
// weakening: it is the same rule model.Placement already implies by
// having no destination but the sandbox that any provider uses.
type Placement struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

// Redacted returns this Spec with every piece of material blanked --
// the framework credential, the controller token, and every placement's
// content -- so that a
// spec can be logged, echoed into an error, or written beside a failed
// run without carrying secrets there.
//
// It exists because "never logged" is otherwise a rule enforced by
// everyone remembering it, in a type whose whole purpose is to move
// credentials. The lengths are kept: a spec that failed to apply is
// routinely one whose credential arrived empty, and "credential: 0 bytes"
// is the diagnosis where a blank string is a mystery.
func (s Spec) Redacted() Spec {
	s.Framework.Credential = redact(s.Framework.Credential)
	s.Token = redact(s.Token)
	if len(s.Placements) > 0 {
		out := make([]Placement, len(s.Placements))
		for i, p := range s.Placements {
			p.Content = redact(p.Content)
			out[i] = p
		}
		s.Placements = out
	}
	return s
}

func redact(v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf("[redacted, %d bytes]", len(v))
}
