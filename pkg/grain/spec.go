package grain

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
	// Contract is the wire version this document is written to. See
	// Contract's own doc comment for what a receiver does with it.
	Contract int `json:"contract"`
	ID       ID  `json:"id"`

	Task      TaskRef  `json:"task"`
	Framework string   `json:"framework,omitempty"` // "claude" | "antigravity" | "codex"; "" is the deployment default
	Repo      RepoSpec `json:"repo"`
	Shape     Shape    `json:"shape"`
	Limits    Limits   `json:"limits"`

	// Setup is the repo's own setup command, run in the guest after the
	// checkout and before the agent's first turn.
	Setup string `json:"setup,omitempty"`
	// Grants names the capability grants whose whole effect is which
	// tools this run gets -- "self-debug", "bootstrap-playbooks". The
	// shim registers the matching tool sets locally; nothing here needs
	// to reach the controller to do it.
	Grants []string `json:"grants,omitempty"`
	// Placements is the credential material this grain was granted,
	// already minted by the controller. Dest is what the current
	// orchestrator cannot express: everything a capability mints today
	// lands in the sandbox, including the model-facing keys the agent
	// itself needs, where the repo's own build can read them. Splitting
	// the destination puts those container-side and leaves only what the
	// checkout genuinely needs in the guest.
	Placements []Placement `json:"placements,omitempty"`
	// GitToken is the git-proxy bearer token identifying this grain,
	// minted by the controller because it is the proxy's to mint, and
	// placed by the shim. It dies with the grain, so a leaked one cannot
	// be replayed by anything dispatched after it.
	GitToken string `json:"gitToken,omitempty"`
}

// TaskRef is enough of the task to reattach to a grain and report on it
// without the store -- what List can show about a grain whose run row the
// controller has not looked up yet.
type TaskRef struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Attempt int    `json:"attempt"`
}

// RepoSpec is where this grain's checkout comes from and where it pushes.
// Branch is model.BranchName's answer for this task: deterministic, and
// deliberately not something the agent can influence.
type RepoSpec struct {
	Target    string   `json:"target,omitempty"` // "owner/name"
	Base      string   `json:"base,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Reads     []string `json:"reads,omitempty"`     // read-only repos; grant nothing
	ProxyBase string   `json:"proxyBase,omitempty"` // the git proxy's base URL, as reached from the guest
}

// Shape is how big a guest this grain asked for. It is a create-time
// argument and nothing resizes it: the VM's root disk is a qcow2 overlay
// made with the VM, and a grain lives exactly as long as one run.
type Shape struct {
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty"`
	DiskGB   int `json:"diskGB,omitempty"`
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
	Dest    Dest   `json:"dest"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

// Limits bound one grain. MaxRuntime is enforced by the shim, which is
// the process that can actually stop the agent; the controller's own
// Policy.ProvisionBudget covers the stretch before there is an agent to
// stop, and Policy.MaxRebuilds backstops MaxRebuilds here in case the
// shim itself is the thing misbehaving.
type Limits struct {
	MaxTurns    int      `json:"maxTurns,omitempty"`
	MaxRuntime  Duration `json:"maxRuntime,omitempty"`
	MaxRebuilds int      `json:"maxRebuilds,omitempty"`
}
