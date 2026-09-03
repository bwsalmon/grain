package main

// `grain repo` is the CLI's per-repo surface (grain/task-36): the
// allowlist a task's repo may name, and the default capability set a repo
// adds on top of the deployment's (grain/task-24, README's "The same set,
// per repo").
//
// Why this exists at all, when schedules, templates, suites and
// qualification plans are all still UI-only: those are *authored
// content*, written once in a form built for writing them, and their
// absence here is an open gap nobody has needed closed (docs/schedules.md
// says so in as many words). This is *deployment configuration*, which is
// the category the CLI already covers end to end -- `grain settings`,
// `grain secrets`, `grain config` -- because "why did this deployment do
// that" gets asked from a shell on the host at least as often as from a
// browser. A repo's own defaults were the one member of that category
// with no spelling here at all, and the asymmetry was visible: `grain
// settings` already *prints* them ("default in: owner/name",
// capabilityStatusLine) with no way to act on what it just showed.
//
// -target-repos stays on `grain settings` rather than moving here. The
// repos *pane* dropped its own copy of that field when
// bwsalmon/agents#473 moved add/remove onto the repo rows, but what it
// dropped was a comma-separated text box, which is a bad control for a
// human and a perfectly good flag for a script. The field itself is
// still deployment-wide configuration (model.Config.TargetRepos), and it
// is the whole-list form `grain sync`'s own "settings" section already
// speaks verbatim (ui.UpdateSettingsRequest.TargetRepos) -- so removing
// the flag would leave the CLI unable to say declaratively what a config
// file next to it can, and would break existing scripts to buy nothing.
// `grain repo add`/`remove` is the incremental form, the one a human at a
// shell wants and the one the pane has; both write the same field, and
// neither is a second source of truth for it.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bwsalmon/grain/pkg/ui"
)

// Flags precede the positional argument here, as they do on every other
// verb that takes both (`grain comment [-attach path]... <id>`): Go's
// flag package stops parsing at the first non-flag argument, so `grain
// repo capabilities acme/widgets -set x` would silently read instead of
// write. Spelled out in the usage line rather than worked around, since
// working around it would make this the one verb whose arguments may
// come in any order.
const repoUsage = `usage: grain repo <subcommand> [args]

  list                                  one line per repo this deployment knows about
  capabilities [-set a,b] <owner/name>  show, or replace, a repo's own default capability set
  prompt-extension [-set text] <owner/name>
                                        show, or replace, a repo's own standing instructions for a run
  add <owner/name>                      add a repo to the allowlist a task's repo may name
  remove <owner/name>                   remove a repo from that allowlist
`

func cmdRepo(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, repoUsage)
		return errors.New("a repo subcommand is required")
	}
	sub, subArgs := args[0], args[1:]
	switch sub {
	case "list":
		return cmdRepoList(ctx, c, out, subArgs)
	case "capabilities":
		return cmdRepoCapabilities(ctx, c, out, subArgs)
	case "prompt-extension":
		return cmdRepoPromptExtension(ctx, c, out, subArgs)
	case "add":
		return cmdRepoAdd(ctx, c, out, subArgs)
	case "remove":
		return cmdRepoRemove(ctx, c, out, subArgs)
	default:
		fmt.Fprint(os.Stderr, repoUsage)
		return fmt.Errorf("unknown repo subcommand %q", sub)
	}
}

func cmdRepoList(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain repo list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return err
	}
	out.repos(repos)
	return nil
}

// cmdRepoCapabilities shows a repo's own default capability set with no
// -set given, and replaces it with any -- including an empty one, which
// is how a repo goes back to adding nothing. See parseRepoCapabilities
// for why "not given" and "given empty" have to stay distinguishable.
func cmdRepoCapabilities(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	repo, set, err := parseRepoCapabilities(args)
	if err != nil {
		return err
	}
	if set != nil {
		defaults, err := c.SetRepoDefaultCapabilities(ctx, repo, *set)
		if err != nil {
			return err
		}
		out.repoDefaults(defaults)
		return nil
	}
	defaults, err := c.RepoDefaults(ctx, repo)
	if err != nil {
		return err
	}
	out.repoDefaults(defaults)
	return nil
}

// parseRepoCapabilities parses `grain repo capabilities`' own arguments:
// the repo, and the new set if one was named at all.
//
// A nil set means -set was not given and this is a read; a non-nil empty
// set means it was given empty, which is the only way to clear a repo's
// own defaults and must not be confused with not having asked to change
// them -- the same fs.Visit distinction cmdUpdate and cmdSettings make
// for every field they can set to a zero value. Split out from the
// command itself so that distinction is testable without a server to
// talk to.
func parseRepoCapabilities(args []string) (repo string, set *[]string, err error) {
	fs := flag.NewFlagSet("grain repo capabilities", flag.ContinueOnError)
	ids := fs.String("set", "",
		"comma-separated capability IDs this repo adds to the deployment's own default set -- empty clears the repo's own")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if fs.NArg() == 0 {
		return "", nil, errors.New("usage: grain repo capabilities [-set a,b] <owner/name>")
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "set" {
			// splitRepoList, not strings.Split: "" has to mean the empty
			// set rather than [""], exactly as it does for
			// -target-repos and -default-capabilities.
			v := splitRepoList(*ids)
			if v == nil {
				v = []string{}
			}
			set = &v
		}
	})
	return fs.Arg(0), set, nil
}

// cmdRepoPromptExtension shows a repo's own standing instructions with
// no -set given, and replaces them with any -- including an empty one,
// which is how a repo goes back to adding nothing of its own
// (grain/task-114). The deployment-wide layer these are appended to is
// `grain settings -prompt-extension`; a repo can only add to it.
func cmdRepoPromptExtension(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	repo, set, err := parseRepoPromptExtension(args)
	if err != nil {
		return err
	}
	if set != nil {
		defaults, err := c.SetRepoPromptExtension(ctx, repo, *set)
		if err != nil {
			return err
		}
		out.repoPromptExtension(defaults)
		return nil
	}
	defaults, err := c.RepoDefaults(ctx, repo)
	if err != nil {
		return err
	}
	out.repoPromptExtension(defaults)
	return nil
}

// parseRepoPromptExtension is parseRepoCapabilities' counterpart for the
// text, and makes the same distinction for the same reason: a nil set
// means -set was not given and this is a read, where a non-nil empty one
// was given empty and is the only way to clear what a repo says.
func parseRepoPromptExtension(args []string) (repo string, set *string, err error) {
	fs := flag.NewFlagSet("grain repo prompt-extension", flag.ContinueOnError)
	text := fs.String("set", "",
		"standing instructions appended to the deployment's for a run against this repo -- empty clears the repo's own")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if fs.NArg() == 0 {
		return "", nil, errors.New("usage: grain repo prompt-extension [-set text] <owner/name>")
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "set" {
			v := *text
			set = &v
		}
	})
	return fs.Arg(0), set, nil
}

func cmdRepoAdd(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain repo add", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: grain repo add <owner/name>")
	}
	settings, err := c.AddTargetRepo(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	out.targetRepos(settings)
	// An empty allowlist means unrestricted everywhere it appears, so a
	// list of exactly one is a deployment that now allows this repo and
	// nothing else -- worth saying out loud, since "add" reads as though
	// it could only ever widen. Keyed on the list that came back rather
	// than on having just flipped it from empty: that would take a
	// second read to know, and the sentence is true either way.
	if !out.json && len(settings.TargetRepos) == 1 {
		fmt.Println("this deployment now allows only this repo -- an empty allowlist is what means unrestricted")
	}
	return nil
}

func cmdRepoRemove(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain repo remove", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: grain repo remove <owner/name>")
	}
	settings, err := c.RemoveTargetRepo(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	out.targetRepos(settings)
	return nil
}

func (p *printer) repos(repos []ui.RepoSummary) {
	if p.json {
		p.encode(repos)
		return
	}
	if len(repos) == 0 {
		fmt.Println("no repos yet -- add one with \"grain repo add owner/name\", or file a task against one")
		return
	}
	for _, r := range repos {
		fmt.Println(repoLine(r))
	}
}

// repoLine renders one repo as a line of `grain repo list`. "allowlisted"
// appears only on a repo Config.TargetRepos actually names, and nothing
// at all appears in its place otherwise: on an unrestricted deployment
// every row is unlisted by definition, and a column reading "not
// allowlisted" against all of them would look like a finding.
func repoLine(r ui.RepoSummary) string {
	allowlisted := ""
	if r.Configured {
		allowlisted = "allowlisted"
	}
	var notes []string
	if r.Tasks == 0 {
		notes = append(notes, "no tasks")
	} else {
		notes = append(notes, fmt.Sprintf("%d tasks (%s)", r.Tasks, repoStateCounts(r)))
	}
	if r.Blocked > 0 {
		notes = append(notes, fmt.Sprintf("%d blocked", r.Blocked))
	}
	if len(r.DefaultCapabilities) > 0 {
		notes = append(notes, "defaults: "+strings.Join(r.DefaultCapabilities, ", "))
	}
	return strings.TrimRight(fmt.Sprintf("%-30s %-12s %s", r.Repo, allowlisted, strings.Join(notes, "; ")), " ")
}

// repoStateCounts breaks a repo's task count down by state, in
// ui.RepoStateOrder -- the repos page's own order, so the two read the
// same way.
//
// Anything the count includes that that order does not name is reported
// as "other" rather than dropped: a breakdown printed beside a total it
// does not add up to is worse than one that admits it has a leftover,
// and a state added to model without being added to RepoStateOrder is
// exactly how that would otherwise happen.
func repoStateCounts(r ui.RepoSummary) string {
	var parts []string
	named := 0
	for _, state := range ui.RepoStateOrder {
		n := r.States[state]
		if n == 0 {
			continue
		}
		named += n
		parts = append(parts, fmt.Sprintf("%d %s", n, state))
	}
	if other := r.Tasks - named; other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", other))
	}
	return strings.Join(parts, ", ")
}

func (p *printer) repoDefaults(d ui.RepoDefaults) {
	if p.json {
		p.encode(d)
		return
	}
	fmt.Println(d.Repo)
	// All three sets, because the editable one says nothing useful alone
	// (ui.RepoDefaults' own doc comment): a repo listing gcp-key looks
	// identical whether that is the only thing putting gcp-key on tasks
	// here or a restatement of what every task in the deployment already
	// gets.
	fmt.Printf("own defaults:        %s\n", capabilityList(d.DefaultCapabilities))
	fmt.Printf("deployment defaults: %s\n", capabilityList(d.DeploymentDefaultCapabilities))
	fmt.Printf("what a task gets:    %s\n", capabilityList(d.EffectiveDefaultCapabilities))
	fmt.Println("\n-set replaces own defaults only; the deployment-wide set is \"grain settings -default-capabilities\", and a repo can only add to it")
}

// repoPromptExtension prints the prompt-extension half of the same
// RepoDefaults document repoDefaults above prints the capability half
// of -- all three layers, for the same reason it prints all three sets:
// a repo's own text says nothing useful without what it is appended to,
// and what a run here is actually told is the composition rather than
// either one.
func (p *printer) repoPromptExtension(d ui.RepoDefaults) {
	if p.json {
		p.encode(d)
		return
	}
	fmt.Println(d.Repo)
	fmt.Print(promptExtensionBlock("own", d.PromptExtension))
	fmt.Print(promptExtensionBlock("deployment", d.DeploymentPromptExtension))
	fmt.Print(promptExtensionBlock("what a run here is told", d.EffectivePromptExtension))
	fmt.Println("\n-set replaces this repo's own text only; the deployment-wide one is \"grain settings -prompt-extension\", and a repo can only add to it. A task can override both (New task -> Advanced options)")
}

func capabilityList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// targetRepos prints just the allowlist out of the settings `grain repo
// add`/`remove` get back, rather than the whole settings block: the
// deployment's poll interval is not what somebody who just added a repo
// asked about. -json still gets the whole response, which is what the
// endpoint actually returned and what a script would want to read
// another field out of.
func (p *printer) targetRepos(s ui.Settings) {
	if p.json {
		p.encode(s)
		return
	}
	if len(s.TargetRepos) == 0 {
		fmt.Println("target repos: unrestricted -- a task may name any repo")
		return
	}
	fmt.Printf("target repos: %s\n", strings.Join(s.TargetRepos, ", "))
}
