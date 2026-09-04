package grain

import "time"

// SpecVersion is the schema version a controller stamps on every Spec it
// creates, and the one a shim knows how to read.
//
// It exists because the shim now ships in the guest-carrying sandbox
// image rather than in the daemon's own binary, so a deployment can
// genuinely be running a controller and an image that disagree. A shim
// handed a Spec whose Version it does not recognise must refuse it and
// report PhaseFailed with OutcomeSetupFailed naming both versions --
// never interpret it on a best effort. A refusal costs one run and says
// exactly what is wrong; a misread Spec is a run that does the wrong
// thing quietly.
const SpecVersion = 1

// Spec is the whole declaration of one grain's work: everything the shim
// needs to run it, and everything a rebuild needs to put a fresh guest
// back the way this one was. It is written into the container at Create
// and never updated -- what changes after that arrives as a Signal.
//
// The prompt is deliberately not here. It is assembled by the controller,
// out of the store (the task's conversation, its previous attempts, the
// deployment's and the repo's prompt extensions), and delivered by
// Signal once the grain reports PhaseProvisioned -- see Status.Checkout
// for why it cannot be assembled before the checkout exists.
type Spec struct {
	ID      ID
	Version int

	Task      TaskRef
	Framework string // "claude" | "antigravity" | "codex"; "" is the deployment default
	Repo      RepoSpec
	Shape     Shape
	Limits    Limits

	// Setup is the repo's own setup command, run in the guest after the
	// checkout and before the agent's first turn.
	Setup string
	// Grants names the capability grants whose whole effect is which
	// tools this run gets -- "self-debug", "bootstrap-playbooks". The
	// shim registers the matching tool sets locally; nothing here needs
	// to reach the controller to do it.
	Grants []string
	// Placements is the credential material this grain was granted,
	// already minted by the controller. Dest is what the current
	// orchestrator cannot express: everything a capability mints today
	// lands in the sandbox, including the model-facing keys the agent
	// itself needs, where the repo's own build can read them. Splitting
	// the destination puts those container-side and leaves only what the
	// checkout genuinely needs in the guest.
	Placements []Placement
	// GitToken is the git-proxy bearer token identifying this grain,
	// minted by the controller because it is the proxy's to mint, and
	// placed by the shim. It dies with the grain, so a leaked one cannot
	// be replayed by anything dispatched after it.
	GitToken string
}

// TaskRef is enough of the task to reattach to a grain and report on it
// without the store -- what List can show about a grain whose run row the
// controller has not looked up yet.
type TaskRef struct {
	ID      string
	Title   string
	Attempt int
}

// RepoSpec is where this grain's checkout comes from and where it pushes.
// Branch is model.BranchName's answer for this task: deterministic, and
// deliberately not something the agent can influence.
type RepoSpec struct {
	Target    string // "owner/name"
	Base      string
	Branch    string
	Reads     []string // read-only repos; grant nothing
	ProxyBase string   // the git proxy's base URL, as reached from the guest
}

// Shape is how big a guest this grain asked for. It is a create-time
// argument and nothing resizes it: the VM's root disk is a qcow2 overlay
// made with the VM, and a grain lives exactly as long as one run.
type Shape struct {
	CPUs, MemoryMB, DiskGB int
}

// IsZero reports whether a shape asks for nothing in particular, which is
// what a task with no override produces.
func (s Shape) IsZero() bool { return s.CPUs == 0 && s.MemoryMB == 0 && s.DiskGB == 0 }

// Dest is which side of the vsock boundary a placement lands on.
type Dest string

const (
	// DestContainer is beside the agent: the model API credential, its
	// framework config, anything the agent process itself reads. The
	// guest cannot reach it, which is the whole point of the split.
	DestContainer Dest = "container"
	// DestGuest is inside the sandbox: what the checkout, the build or
	// the test suite needs. Everything here is readable by whatever code
	// the repo runs, so it should hold only what that code legitimately
	// needs.
	DestGuest Dest = "guest"
)

// Placement is one piece of credential material and where it goes. Mode
// is an octal string (model.Placement.EffectiveMode's shape) and must
// never be widened in transit.
type Placement struct {
	Dest    Dest
	Path    string
	Content string
	Mode    string
}

// Limits bound one grain. MaxRuntime is enforced by the shim, which is
// the process that can actually stop the agent; the controller's own
// Policy.ProvisionBudget covers the stretch before there is an agent to
// stop, and Policy.MaxRebuilds backstops MaxRebuilds here in case the
// shim itself is the thing misbehaving.
type Limits struct {
	MaxTurns    int
	MaxRuntime  time.Duration
	MaxRebuilds int
}
