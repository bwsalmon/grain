package model

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// This file is docs/data-model.md's "Capabilities are an extension
// point, not a table" section, decided in code: a capability travels
// through four moments -- requested (a Grant already exists by the time
// any of this runs), resolved, materialized, revoked -- and a provider
// is exactly those four methods and nothing else.
//
// Unlike the Python contract that document describes, a provider here
// is handed no Runner. v2 has no host adapter yet (see v2/README.md),
// so there is nothing for one to run commands against -- and Placement
// was already the declarative half of that design, the half a
// containerised provider is restricted to because it has no other
// route to move material. Starting v2's contract there means "what
// crosses" is structural from the first provider, rather than a
// convention an in-process Runner would let slip.

// Side is where a Placement lands.
type Side string

const (
	SideSandbox    Side = "sandbox"
	SideController Side = "controller"
)

// Placement is one piece of material a capability wants written down,
// returned rather than performed so every mover of material stays
// enumerable and auditable, and so a provider is testable against a
// returned value with no sandbox and no cloud involved. Content is
// material: never logged, and never handed to PromptSection, which
// sees only a placement's Path.
type Placement struct {
	Side    Side
	Path    string
	Content string
	Mode    string // see EffectiveMode; empty means the safe default.
	Owner   string
}

// EffectiveMode is Mode, defaulting to "600" -- the safe answer, so a
// provider that leaves Mode unset does not thereby get a wider one.
func (p Placement) EffectiveMode() string {
	if p.Mode == "" {
		return "600"
	}
	return p.Mode
}

// Resolution is Resolve's answer. The zero value is Honoured, because
// most capabilities honour every request -- see BaseCapability.
// Refused parks the task, with Reason posted verbatim as the comment
// explaining why: human-facing text, not a code.
type Resolution struct {
	Refused bool
	Reason  string
}

// Honoured is the resolution a provider returns when it can grant what
// was asked.
func Honoured() Resolution { return Resolution{} }

// RefusedBecause is the resolution a provider returns when it cannot --
// reason is posted to the task verbatim, so it should read as a
// sentence a human can act on.
func RefusedBecause(reason string) Resolution { return Resolution{Refused: true, Reason: reason} }

// Materialization is what Materialize produces. Three fields for three
// kinds of effect: a Lease to record and eventually revoke, Placements
// for the executor to apply, and -- for a SELECT capability, which
// mints nothing and writes nothing into a sandbox -- which standing
// credential to route through instead.
type Materialization struct {
	Lease              *Lease
	Placements         []Placement
	CredentialOverride *CredentialRef
}

// CredentialResolver resolves a credential by name to material. A
// provider is handed one of these, never a store to browse, so what it
// can reach is exactly what it names.
type CredentialResolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// CapabilityContext bounds what a provider can do: this task, this run,
// a resolver for named credentials, per-run scratch, and the clock --
// not arbitrary shell, not a credential store to browse.
type CapabilityContext struct {
	Task        Task
	Run         Run
	Now         time.Time
	Workdir     string
	Credentials CredentialResolver
}

// CapabilitySpec is a capability's fixed identity: what a grant of it
// means, before any particular task holds one.
type CapabilitySpec struct {
	Name        string
	Label       string
	Description string
	Source      GrantSource
	Provision   Provision
	// MaxLease is the unconditional backstop past which a lease is
	// revoked regardless of its own ExpiresAt -- see Lease.Expired. Zero
	// for a GRANT or SELECT capability, which mint nothing to expire.
	MaxLease time.Duration
	// RequiredSecrets names every secret this provider's Materialize/
	// Revoke/Reap resolves through CapabilityContext.Credentials --
	// docs/data-model.md's `requires` (the credential a capability needs),
	// generalised to a list since a provider is free to resolve more than
	// one. Names only, the same "the model holds names, never material"
	// rule CredentialRef already follows: wherever a capability's spec is
	// rendered for a human -- a label description, an issue comment, any
	// future GitHub-facing view of the registry -- this is what lets that
	// view say which secrets an operator must configure without that view
	// ever being handed a secret's value to render. A GRANT capability
	// needs none of this and leaves it nil.
	RequiredSecrets []string
}

// CapabilityProvider is the contract a capability implements.
//
// PromptSection is only ever called, by ResolveGrants/MaterializeGrants/
// PromptSections below, for a grant whose Materialize already succeeded
// -- the prompt is a promise to the agent, and a capability that
// half-applied must not be described as present. It receives the
// placements Materialize returned, not the request that produced them,
// and never their Content -- so leaking material into a prompt is
// structurally impossible rather than carefully avoided.
type CapabilityProvider interface {
	Spec() CapabilitySpec
	Resolve(ctx context.Context, cc CapabilityContext) (Resolution, error)
	Materialize(ctx context.Context, cc CapabilityContext) (Materialization, error)
	PromptSection(ctx context.Context, cc CapabilityContext, placements []Placement) (string, error)
	Revoke(ctx context.Context, cc CapabilityContext, lease Lease) error
}

// BaseCapability is embeddable by a provider that wants the contract's
// default behaviour: resolve always Honoured, mint nothing, no prompt
// text, revoke a no-op. A GRANT capability -- one that needs no
// credential at all -- is most of a file shorter for embedding this and
// writing only Spec and, if it has one, PromptSection.
type BaseCapability struct{}

func (BaseCapability) Resolve(context.Context, CapabilityContext) (Resolution, error) {
	return Honoured(), nil
}

func (BaseCapability) Materialize(context.Context, CapabilityContext) (Materialization, error) {
	return Materialization{}, nil
}

func (BaseCapability) PromptSection(context.Context, CapabilityContext, []Placement) (string, error) {
	return "", nil
}

func (BaseCapability) Revoke(context.Context, CapabilityContext, Lease) error {
	return nil
}

// Reaper is implemented by a capability provider whose minted resource can
// outlive the Lease that recorded it -- a controller crash between mint and
// store write, or an operator-invalidated record. Unlike Revoke, which acts
// on one Lease a task held, Reap consults the resource's own source of
// truth (a cloud API's own listing, never grain's store, which is exactly
// what a lost record has nothing left to say) and deletes anything older
// than the provider's own idea of "too old" -- the same backstop role
// docs/data-model.md gives grain/automation/gcp_keys.py's
// delete_expired_keys, run independent of any task or run.
//
// creds resolves whatever standing credential the provider needs to do
// that: the same kind of resolver a Materialize/Revoke call is handed
// through CapabilityContext, but usable with no live task in hand, since a
// reap is not scoped to one. Reap returns the identifiers of whatever it
// deleted, for a caller to log; it is best-effort per resource, the same
// "one already-gone item must not stop the rest" rule Revoke's own callers
// already apply.
//
// Optional: a provider that mints nothing external to a Lease has nothing
// to check outside its own leases and need not implement this.
type Reaper interface {
	Reap(ctx context.Context, creds CredentialResolver, now time.Time) ([]string, error)
}

// CapabilityRegistry holds providers in registration order --
// deterministic, so two providers that both act on the same task
// compose predictably rather than by map iteration order -- and looks
// them up by name for the grants a particular task actually holds.
type CapabilityRegistry struct {
	order     []string
	providers map[string]CapabilityProvider
}

// NewCapabilityRegistry builds a registry from providers, in the order
// given.
func NewCapabilityRegistry(providers ...CapabilityProvider) *CapabilityRegistry {
	r := &CapabilityRegistry{providers: make(map[string]CapabilityProvider)}
	r.Register(providers...)
	return r
}

// Register adds providers, in the order given, appended after whatever
// is already registered. Registering a name a second time replaces its
// provider in place without changing registration order -- the same
// rule mcp.Registry.Register follows, for the same reason.
func (r *CapabilityRegistry) Register(providers ...CapabilityProvider) {
	for _, p := range providers {
		name := p.Spec().Name
		if _, exists := r.providers[name]; !exists {
			r.order = append(r.order, name)
		}
		r.providers[name] = p
	}
}

// Lookup finds a provider by capability name.
func (r *CapabilityRegistry) Lookup(name string) (CapabilityProvider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Providers returns every registered provider, in registration order.
func (r *CapabilityRegistry) Providers() []CapabilityProvider {
	out := make([]CapabilityProvider, len(r.order))
	for i, name := range r.order {
		out[i] = r.providers[name]
	}
	return out
}

// RequiredSecrets pairs every registered capability's name with
// CapabilitySpec.RequiredSecrets, in registration order, skipping any
// capability that names none. This is the query "the model in github
// should list what secrets are needed by various capabilities, but not
// the values" (bwsalmon/agents#240) reduces to: every value in here is a
// name a Store's own directory listing could equally produce, never
// material, so nothing that renders this needs to be trusted with a
// secret to say which ones a deployment must configure.
func (r *CapabilityRegistry) RequiredSecrets() map[string][]string {
	out := make(map[string][]string)
	for _, p := range r.Providers() {
		spec := p.Spec()
		if len(spec.RequiredSecrets) > 0 {
			out[spec.Name] = spec.RequiredSecrets
		}
	}
	return out
}

// GrantResolution pairs one of a task's Grants with what Resolve
// decided for it.
type GrantResolution struct {
	Grant      Grant
	Resolution Resolution
}

// ResolveGrants resolves every capability cc.Task was granted, calling
// Resolve on each one's registered provider. Order follows the
// registry's own registration order rather than the order Grants
// happen to be stored in, so results are deterministic regardless of
// how the grants were recorded -- the same "providers run in
// registration order" rule docs/data-model.md gives providers that act
// on a sandbox.
//
// A grant naming a capability no provider is registered for is refused
// with a reason that says so, rather than skipped or left to panic
// later in Materialize: the registry not recognising a capability by
// that name is exactly the unhonourable case Resolve exists to report.
// This is also the backstop docs/data-model.md asks for when a
// capability is retired while a lease is still outstanding -- Revoke
// stays reachable through Lookup even once nothing grants the name
// again.
func ResolveGrants(ctx context.Context, reg *CapabilityRegistry, cc CapabilityContext) ([]GrantResolution, error) {
	granted := make(map[string]Grant, len(cc.Task.Grants))
	for _, g := range cc.Task.Grants {
		granted[g.Capability] = g
	}

	var out []GrantResolution
	seen := make(map[string]bool, len(granted))
	for _, p := range reg.Providers() {
		g, ok := granted[p.Spec().Name]
		if !ok {
			continue
		}
		seen[g.Capability] = true
		res, err := p.Resolve(ctx, cc)
		if err != nil {
			return nil, fmt.Errorf("model: resolving capability %q: %w", g.Capability, err)
		}
		out = append(out, GrantResolution{Grant: g, Resolution: res})
	}

	var unregistered []string
	for name := range granted {
		if !seen[name] {
			unregistered = append(unregistered, name)
		}
	}
	sort.Strings(unregistered)
	for _, name := range unregistered {
		out = append(out, GrantResolution{
			Grant:      granted[name],
			Resolution: RefusedBecause(fmt.Sprintf("no provider is registered for capability %q", name)),
		})
	}
	return out, nil
}

// Materialized is one grant's Materialize result, with the provider
// that produced it -- carried alongside so PromptSections below can
// call PromptSection without a second registry lookup.
type Materialized struct {
	Grant           Grant
	Provider        CapabilityProvider
	Materialization Materialization
}

// MaterializeGrants materializes every honoured resolution, in the
// order ResolveGrants returned them, skipping every refused one.
//
// A failed Materialize stops the pass immediately and returns both the
// error and what succeeded before it -- "a failed materialize means no
// dispatch": this package does not decide what a partial
// materialization means for a run (nothing dispatches yet -- see
// v2/README.md), it just refuses to paper over the failure by
// continuing past it or by calling PromptSection for a capability that
// never finished applying.
func MaterializeGrants(ctx context.Context, reg *CapabilityRegistry, cc CapabilityContext, resolved []GrantResolution) ([]Materialized, error) {
	var out []Materialized
	for _, r := range resolved {
		if r.Resolution.Refused {
			continue
		}
		p, ok := reg.Lookup(r.Grant.Capability)
		if !ok {
			return out, fmt.Errorf("model: materializing capability %q: no provider registered", r.Grant.Capability)
		}
		m, err := p.Materialize(ctx, cc)
		if err != nil {
			return out, fmt.Errorf("model: materializing capability %q: %w", r.Grant.Capability, err)
		}
		out = append(out, Materialized{Grant: r.Grant, Provider: p, Materialization: m})
	}
	return out, nil
}

// PromptSections collects prompt text for every successfully
// materialized grant, in order, dropping any that returned no text --
// a GRANT capability with nothing to say, or a SELECT capability that
// only changed which credential the git proxy routes through.
func PromptSections(ctx context.Context, cc CapabilityContext, materialized []Materialized) ([]string, error) {
	var out []string
	for _, m := range materialized {
		text, err := m.Provider.PromptSection(ctx, cc, m.Materialization.Placements)
		if err != nil {
			return nil, fmt.Errorf("model: prompt section for capability %q: %w", m.Grant.Capability, err)
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return out, nil
}
