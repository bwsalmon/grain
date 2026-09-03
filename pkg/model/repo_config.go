package model

// RepoConfig is one repo's own configuration -- the per-repo layer of
// what Config already says for the whole deployment (grain/task-24,
// following task-14's deployment-wide DefaultCapabilities and the ask
// that came with it: "we will also want this to be possible on
// individual repos in the future").
//
// One row per repo, keyed the same (owner, name) way qualification_config
// already is, and read only where a repo is already in hand: a task's own
// target, resolved by ui.CreateTask before it files anything. A task with
// no repo at all (Task.Target nil) has nothing to key on and gets the
// deployment's answer alone.
//
// This is deliberately not docs/data-model.md's folder `offers` tree,
// which is a different mechanism on purpose: an offer is a *floor* --
// unioned in when a task's grants are resolved, not droppable by the task
// -- while everything here is a *seed*, written onto the task at creation
// and untickable on the form that files it. Mixing the two silently would
// be the failure mode worth avoiding: a human unticking a capability and
// having it granted anyway, or an operator setting a floor and watching
// tasks file without it. When the folder tree arrives it composes at
// resolution, alongside these, rather than by feeding into them.
type RepoConfig struct {
	Repo RepoRef
	// DefaultCapabilities is the capability ids a task targeting this
	// repo is filed holding *in addition to* Config.DefaultCapabilities
	// -- union, in that order, resolved in
	// ui.(*Client).defaultCapabilities.
	//
	// Union is the whole composition rule, and the only one: a repo adds
	// to what the deployment defaults and can never subtract from it.
	// "Everything except gcp-key here" is the same "except here" question
	// docs/data-model.md defers for ceilings, and it gets the same answer
	// for the same reason -- a field the resolver ignores is a trap, and
	// the first person who actually needs it is a better trigger than a
	// guess. Until then, an operator who wants a capability on most repos
	// but not one of them lists it per repo rather than deployment-wide,
	// and whoever files a task can always untick it on the form.
	//
	// An id already in Config.DefaultCapabilities is stored here as given
	// rather than folded away: the two layers are chosen by different
	// people at different times, and a repo that names one explicitly
	// keeps it if the deployment-wide entry is later dropped. The union
	// is what a task is filed with either way.
	DefaultCapabilities []string
	// PromptExtension is this repo's own standing instructions for a run
	// working in it, appended *after* Config.PromptExtension for a task
	// that targets this repo -- prompt_extension.go's own doc comment has
	// the composition rule and why it is an append rather than a
	// replacement.
	//
	// Read at dispatch rather than seeded onto the task, unlike
	// DefaultCapabilities above (Config.PromptExtension has why): a task
	// filed today against a repo whose conventions are written down
	// tomorrow is told about them when it runs.
	PromptExtension string
}

// Empty reports whether this config says nothing at all -- the state a
// repo with no row is in. PutRepoConfig deletes rather than writes such a
// row, so "has a row" and "has something of its own to say" stay the same
// fact and ListRepoConfigs never returns a repo that adds nothing. A
// further field here (base and max_concurrent are docs/data-model.md's
// own remaining two) gains a term in this method and nothing else has to
// change -- PromptExtension, the second, is exactly that.
func (c RepoConfig) Empty() bool {
	return len(c.DefaultCapabilities) == 0 && c.PromptExtension == ""
}
