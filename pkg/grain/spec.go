package grain

// Spec is the whole declaration of one grain's work, written into the
// container once at create and never updated -- what changes after that
// arrives as a Signal.
//
// It is deliberately small, and what it leaves out is the point: **a
// grain knows how to run an agent in a sandbox, and nothing about why.**
// There is no task here, no repository, no branch, no git credential and
// no capability model, because a grain has no use for any of them. Every
// one of those reaches it in one of three shapes instead:
//
//   - in the prompt, which the controller assembles from its store and
//     delivers by Signal once the sandbox is real;
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
	// Contract is the wire version this document is written to. See
	// Contract's own doc comment for what a receiver does with it.
	Contract int `json:"contract"`

	// There is no id here. The container is the identity: a controller
	// execs into one specific container to configure it, and the shim
	// never needs to be told a name it makes no use of.

	// Framework names an agent profile the sandbox image provides --
	// "claude", "antigravity", "codex". It is a name and not a
	// configuration on purpose: how that CLI is actually launched, which
	// flags it takes, where its MCP config has to live and whether it
	// needs a private HOME are facts about the binary, and the binary
	// ships in this image.
	//
	// Today the daemon owns all of that (pkg/agent/claude, /antigravity,
	// /codex -- and see antigravity's own doc comment on agy having no
	// --mcp-config, so each run gets a private HOME holding one file).
	// That knowledge sits in a different artifact from the CLI it
	// describes, so upgrading the CLI can require upgrading the daemon.
	// Moving it into the image versions the two together, and makes
	// adding a framework an image change rather than a controller
	// release.
	//
	// A controller can ask which profiles an image has before dispatching
	// to one -- ContractReport.Frameworks -- so a task naming a framework
	// this image lacks fails at create, naming it, rather than at launch.
	Framework string `json:"framework"`

	// Shape is the guest this grain gets. A create-time argument and
	// nothing resizes it: the root disk is a qcow2 overlay made with the
	// VM, and a grain lives exactly as long as one run.
	Shape Shape `json:"shape"`

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
	// Its output is also how the two-phase start works: the controller
	// wrote the script, so it can end it with whatever it needs to read
	// back -- `git rev-parse HEAD`, a log of what earlier attempts pushed
	// -- and parse its own output. The shim stays ignorant of all of it.
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

	// MaxRuntime is how long the agent may run before the shim stops it
	// and reports a terminal Phase. Zero means no bound of the grain's
	// own.
	//
	// It is the only limit left here. Turns are gone: they are a
	// framework's own flag, and Config.MaxAgentTurns' doc comment already
	// concedes that both frameworks default to no cap and "what actually
	// bounds a runaway run is MaxRunRuntime". Rebuilds are gone too --
	// the controller has the whole view and decides when self-repair has
	// stopped converging (Policy.MaxRebuilds), and two enforcement points
	// for one rule is one more than it needs.
	//
	// This one stays despite the controller also being able to Release a
	// grain that has overrun, for two reasons: stopping the agent is how
	// a run ends with a Result rather than being destroyed mid-thought,
	// and a runaway agent spends money, so it should not depend on a
	// controller being up to notice.
	MaxRuntime Duration `json:"maxRuntime,omitempty"`
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
