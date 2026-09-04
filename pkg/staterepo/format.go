package staterepo

// format.go lays out a state repository that does not have one yet, and
// writes the CI step that keeps changes to it honest.
//
// Everything else in this package answers "what does a running
// deployment do with a repository". This file answers the question that
// comes before that one: an operator has just created an empty
// repository on GitHub and has nothing in it, and the parts of a state
// repository that are not the dump -- the README that says what the
// tree is, the .gitignore that keeps a stray key out of it, and the
// workflow that runs `grain state check` against a proposed change --
// have until now existed only once a deployment had seeded it, and the
// workflow has not existed at all.
//
// Two things are deliberately not here.
//
// No dump. Formatting writes no tables/ and no schema-version stamp, so
// HasDump stays false and the repository stays the "empty repository
// grain seeds from what it has" of the bootstrap. Writing an empty dump
// would be easy -- an empty database at this build's schema exports one
// file per table -- and would quietly turn adopting into the *other*
// case, the one where the repository's contents replace the database.
// An operator who formatted a new repository and then adopted it would
// have emptied their deployment, and would have been told they were
// adopting an existing repository, which by then would have been true.
//
// No commit, and no push. Format writes files into a directory and
// stops, because it runs in a clone somebody made and has no business
// making commits there.
//
// installWorkflow, at the bottom of this file, is the other half of the
// same job and the one that runs on the deployment: Seed and every Sync
// call it, so a repository grain was pointed at ends up with the check
// whether or not anybody ran a command, and one whose workflow a merge
// dropped gets it back on the next tick. It carries the risk `format`
// was written to avoid -- a push that adds a file under
// .github/workflows is refused unless the credential making it may
// write workflows, and grain's own installation token need not be able
// to -- and it carries it in the one shape where that is survivable: the
// workflow is a commit of its own, pushed on its own, and undone in full
// whenever that push does not land -- for whatever reason, since a
// commit left behind is a commit the *export's* next push carries, which
// is where a refusal would take the export down with it. A deployment
// whose credential says no loses the CI step and nothing else, and is
// told in its journal how to install the file by hand.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCheckImage is the container the generated CI step runs
// `grain state check` out of.
//
// grain publishes no bare binaries -- one image per commit is the whole
// of what CI releases (../../.github/workflows/build-artifacts.yml), and
// its entrypoint is the grain binary, so `docker run <image> state check
// /state` is the command. `:latest` is main's, and public, which is what
// makes the generated workflow need no credential of any kind: a state
// repository's own GITHUB_TOKEN cannot read another repository's private
// packages, and a step that needed one would be a step an operator has
// to configure before it works.
//
// It is a default rather than a constant in the workflow because the
// check is only meaningful against a build that knows the same schema as
// the deployment (Check refuses any other), so a deployment pinned to an
// older tag wants that tag here.
const DefaultCheckImage = "ghcr.io/bwsalmon/grain/grain:latest"

// Formatted is what Format did, so its caller can print it: the files it
// wrote, and the files it found already there and left alone.
type Formatted struct {
	// Wrote names the paths, relative to the directory, that Format put
	// there or brought up to date.
	Wrote []string
	// Left names the paths Format found and did not touch. Only the
	// workflow can end up here: the README is grain's own file and is
	// rewritten on every sync anyway, and the .gitignore is merged into
	// rather than replaced.
	Left []string
}

// Format writes the parts of a state repository that are not the dump
// into dir, creating it if it is not there: the README, the .gitignore,
// and the workflow that validates a pull request against this
// repository.
//
// image names the container the workflow runs the check from;
// DefaultCheckImage when empty. force replaces a workflow file that is
// already there, which is otherwise left exactly as it is -- an operator
// who has edited the runner, the trigger or the image has said something
// this command does not know better than.
//
// Safe to run twice: nothing here is destructive, and a directory that
// is already formatted comes out byte-identical.
func Format(dir, image string, force bool) (Formatted, error) {
	var out Formatted
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, fmt.Errorf("staterepo: preparing %s: %w", dir, err)
	}
	if err := writeReadme(dir); err != nil {
		return out, err
	}
	out.Wrote = append(out.Wrote, ReadmeFile)
	if err := EnsureIgnored(dir); err != nil {
		return out, err
	}
	out.Wrote = append(out.Wrote, IgnoreFile)
	wrote, err := EnsureWorkflow(dir, image, force)
	if err != nil {
		return out, err
	}
	if wrote {
		out.Wrote = append(out.Wrote, WorkflowFile)
	} else {
		out.Left = append(out.Left, WorkflowFile)
	}
	return out, nil
}

// EnsureWorkflow writes the validation step into dir, reporting whether
// it wrote anything. A workflow that is already there is left alone
// unless force, and reported as (false, nil) rather than as an error:
// "the CI step is already installed" is an answer, not a failure.
//
// Exported on its own because the two cases are different commands. A
// repository being formatted has no CI step yet; a repository a
// deployment has been pushing to for months has no CI step either, and
// nothing about adding one to that involves formatting anything.
func EnsureWorkflow(dir, image string, force bool) (bool, error) {
	if image == "" {
		image = DefaultCheckImage
	}
	path := filepath.Join(dir, filepath.FromSlash(WorkflowFile))
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("staterepo: preparing %s: %w", filepath.Dir(path), err)
	}
	if err := writeFileIfChanged(path, Workflow(image)); err != nil {
		return false, err
	}
	return true, nil
}

// Workflow renders the GitHub Actions workflow that runs
// `grain state check` against a proposed change to a state repository.
//
// Rendered from a template with the image substituted in, rather than
// assembled out of a YAML library, because what this produces is a file
// a human is going to read and edit in their own repository: the
// comments, the order and the blank lines are most of what makes it
// reviewable, and a marshaller would keep none of them.
func Workflow(image string) []byte {
	if image == "" {
		image = DefaultCheckImage
	}
	return []byte(strings.ReplaceAll(workflow, workflowImagePlaceholder, image))
}

const workflowImagePlaceholder = "@IMAGE@"

const workflow = `# Written by grain. Checks that grain can still load this repository.
#
# grain writes this file whenever it is not here -- on the sync after a
# merge dropped it, or the first sync after a deployment adopted this
# repository -- and never rewrites one that is. So an edit to it is
# safe: pin the image below, change the runner, add a step of your own,
# and grain leaves what you wrote alone. To have the check run somewhere
# else instead, put your own file here; to stop grain offering it at
# all, set "noWorkflow": true in the deployment's state-repo.json.
#
# This repository is a grain deployment's database, written out as text
# (see README.md), so a pull request against it is a change to what that
# deployment runs. The one question a reviewer cannot answer by reading
# the diff is whether the result will load: a file that is not valid
# JSON, a row missing a column the schema insists on, or a link naming a
# row that is no longer in the dump all read as perfectly plausible text
# and fail at import time -- which, without this step, means after the
# merge, on the deployment, in front of whoever is on call rather than
# whoever wrote the change.
#
# ` + "`grain state check`" + ` is that import, run against a database it throws
# away afterwards: the same single transaction, the same constraints, the
# same rollback on any inconsistency. It needs no credential, no data
# directory and no daemon, only a checkout -- which is why this whole job
# is a checkout and one ` + "`docker run`" + `.
#
# On pull requests only, deliberately. grain commits to this repository
# itself, on a timer, straight to its branch, every time its database
# changes; validating those pushes would spend a CI run each time a task
# changed state, on a dump grain had just written out of the very
# database the check would import it back into.
#
# The image is grain's own published container -- its entrypoint is the
# grain binary, and the package is public, so this step needs no
# credential and no secret. It does have to be a build that knows the
# same schema as the deployment: ` + "`grain state check`" + ` refuses a dump
# stamped with any other, so a failure reading "repository is at schema
# N, this build knows M" means this tag and the deployment have drifted
# apart. Point IMAGE at the tag that deployment runs --
# ` + "`grain state status`" + ` on its host prints both numbers.
name: grain state check

on:
  pull_request:
  # So it can be run by hand as well: after changing IMAGE below, or
  # after the deployment has been upgraded.
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

jobs:
  check:
    name: grain can load this
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v4

      - name: grain state check
        env:
          IMAGE: "` + workflowImagePlaceholder + `"
        run: |
          set -euo pipefail
          # A repository that has been formatted but that no deployment
          # has seeded yet holds no dump at all, and there is nothing to
          # validate until one arrives. ` + "`grain state check`" + ` calls that an
          # error, and for a CI step pointed at the wrong directory it is
          # one, so the empty case is answered here -- where a reader of
          # this file can see it -- rather than by making the check itself
          # laxer for everyone.
          if [ ! -d tables ]; then
            echo "No tables/ directory here: no grain deployment has written its database"
            echo "to this repository yet, so there is nothing to check."
            exit 0
          fi
          docker run --rm -v "$PWD:/state:ro" "$IMAGE" state check /state
`

// workflowCommitMessage is what the commit that installs the CI step
// says. Its own commit, and not folded into the export beside it, for
// the reason installWorkflow gives: a remote that refuses this file has
// to be able to refuse it without taking a database export down with it.
const workflowCommitMessage = "Add the check that runs on pull requests against this repository\n\n" +
	"Written by grain. `grain state check` imports this repository into a\n" +
	"throwaway database, so a change that would not load fails here rather\n" +
	"than on the deployment. Edit it freely -- grain writes this file only\n" +
	"when it is missing, and never over one that is already here."

// workflowRefusedFile records that this host's credential was refused a
// push carrying the workflow.
//
// Inside the git directory, beside grain-loaded-head and
// grain-churn-exported and for the same reason: it is a fact about this
// host's credential rather than about the repository, so it must not be
// committed and must not travel to a clone on a machine whose credential
// may be a different one.
const workflowRefusedFile = "grain-workflow-refused"

// workflowRetryInterval is how long grain leaves a credential alone
// after it has refused the workflow once.
//
// Not "never again", because a permission is exactly the sort of thing
// an operator grants the day after they read the journal line below, and
// a grain that only ever tried once would need a restart to notice. Not
// every thirty seconds either: the attempt costs a commit and a rejected
// push, and repeating that on every tick would fill a deployment's
// journal with a refusal nobody has had a chance to act on yet.
const workflowRetryInterval = 24 * time.Hour

// installWorkflow makes sure the repository carries the CI step, and
// reports whether it committed one.
//
// This is what turns "grain state check exists" into "grain state check
// runs". Seed calls it once the repository has its first commit, and
// every Sync calls it after that, on the same terms the README is
// rewritten on: a repository an operator adopted, or one a merged pull
// request dropped the file out of, ends up with the workflow anyway
// rather than only if somebody remembered.
//
// It writes the file only when it is missing, and there the README and
// the workflow part company. The README is grain's own text and is
// rewritten wholesale; this file is one an operator is entitled to own
// -- pinning the image to the tag their deployment runs is the obvious
// case, and running the check on their own infrastructure is another --
// and a file grain rewrote on every tick would be a file whose editor is
// fighting a timer. So an edit made here, or merged in through a pull
// request, is final: grain notices there is a workflow and stops.
//
// Nothing at all happens without a remote. A workflow is GitHub's to
// run, so a local-only repository has no use for one; more to the point,
// a workflow commit sitting in a local-only history is a commit the
// *first* push after that repository is later published would carry, and
// that push either succeeds whole or is refused whole. Contained to a
// deployment that already has a remote, a refusal costs one commit that
// is undone on the spot.
func (r *Repo) installWorkflow(ctx context.Context) (bool, error) {
	if r.cfg.NoWorkflow || r.cfg.Remote == "" {
		return false, nil
	}
	// A repository with no commits is Seed's business, and the undo below
	// needs a commit to come back to. Seed calls this once it has made
	// one.
	if empty, _ := r.isEmpty(ctx); empty {
		return false, nil
	}
	path := filepath.Join(r.cfg.Dir, filepath.FromSlash(WorkflowFile))
	switch _, err := os.Stat(path); {
	case err == nil:
		return false, nil
	case !os.IsNotExist(err):
		return false, fmt.Errorf("staterepo: looking for %s in %s: %w", WorkflowFile, r.cfg.Dir, err)
	}
	if !r.workflowDue(ctx, r.now()) {
		return false, nil
	}
	before, err := r.Head(ctx)
	if err != nil {
		return false, err
	}
	if _, err := EnsureWorkflow(r.cfg.Dir, r.cfg.CheckImage, false); err != nil {
		return false, err
	}
	// Staged and committed by path, so this commit holds the workflow and
	// nothing but the workflow whatever else happens to be in the working
	// tree -- which is what makes undoing it a matter of dropping one
	// commit rather than a judgement about what else went with it.
	if _, err := r.git(ctx, "add", "--", WorkflowFile); err != nil {
		return false, err
	}
	if _, err := r.git(ctx, "commit", "--quiet", "-m", workflowCommitMessage, "--", WorkflowFile); err != nil {
		return false, err
	}
	// The loaded-head marker moves with HEAD, here as everywhere else
	// that commits. It records the commit this host's database is up to
	// date with, and this commit changes nothing the database holds -- so
	// leaving it behind would tell the next Apply that a pull request had
	// been merged, and have it import the repository's settings back over
	// every one changed since the last export.
	if err := r.recordLoadedHead(ctx); err != nil {
		return false, err
	}
	pushErr := r.Push(ctx)
	if pushErr == nil {
		return true, nil
	}
	// Undone whenever the push does not land, and not only when grain
	// recognises GitHub's refusal in it. What is at stake is the sentence
	// at the top of this file: a remote that will not take this file must
	// not be able to take a database export down with it.
	//
	// Keeping the commit is what broke that. "The next push that works
	// carries it" is true and is the problem: the next push is the
	// export's, so the commit rides along on it, and a remote that
	// refuses workflows refuses *that* push -- on a path with no undo, on
	// a tick where installWorkflow does nothing because the file is
	// already in the tree. The deployment's settings then stop reaching
	// its remote entirely, permanently, over a file worth one CI step. It
	// takes only two ordinary things in sequence: one push that failed
	// for its own reasons, and a credential that may not write workflows
	// -- which is the case this whole shape was built for.
	//
	// What the undo costs instead is one commit that never left this host
	// and is made again on a later tick. The one case it is not free is a
	// push the remote accepted and git reported as failed anyway, where
	// the remote now holds a commit this tree has reset away: the next
	// pull fast-forwards onto it, which is a merge arriving as far as
	// Apply is concerned, and the dump it imports is the one it exported.
	if err := r.undoWorkflowCommit(ctx, before, path); err != nil {
		return false, err
	}
	// Back where the marker was, for the reason it was moved above: HEAD
	// is the commit it names again.
	if err := r.recordLoadedHead(ctx); err != nil {
		return false, err
	}
	if !workflowRefused(pushErr) {
		// Not the permission, so not a day's wait: an unreachable remote
		// or an expired credential says nothing about whether this
		// credential may write workflows, and the next tick asks again.
		// Reported rather than returned, too. The export below is about
		// to push through the same remote and will say what it makes of
		// it in its own words; failing the cycle here would only mean
		// this tick's rows never got committed at all, over a step that
		// has already put everything back as it found it.
		log.Printf("staterepo: could not push %s to %s (%v). The commit that carried it has been "+
			"undone; grain offers it again on the next sync.", WorkflowFile, r.cfg.Remote, pushErr)
		return false, nil
	}
	log.Printf("staterepo: this deployment's credential may not push %s to %s, so grain cannot install "+
		"the check that runs on pull requests against its own state (%v). Run `grain state ci` in a "+
		"clone and commit the file with a credential that may, or set \"noWorkflow\": true in %s to "+
		"stop grain offering it. grain will try again in %s.",
		WorkflowFile, r.cfg.Remote, pushErr, SettingsFileName, workflowRetryInterval)
	return false, r.recordWorkflowRefused(ctx, r.now())
}

// undoWorkflowCommit takes the working tree back to before the workflow
// was written, leaving no trace of a commit that can never be pushed.
//
// --soft, and the path unstaged by hand afterwards, rather than a --hard
// reset: a --hard would also throw away whatever else is in the working
// tree, and the caller's own export may have written half a dump into it
// already. What is undone here is exactly the file this step added.
func (r *Repo) undoWorkflowCommit(ctx context.Context, before, path string) error {
	if _, err := r.git(ctx, "reset", "--soft", before); err != nil {
		return fmt.Errorf("staterepo: undoing the workflow commit in %s: %w", r.cfg.Dir, err)
	}
	if _, err := r.git(ctx, "reset", "-q", "--", WorkflowFile); err != nil {
		return fmt.Errorf("staterepo: unstaging %s in %s: %w", WorkflowFile, r.cfg.Dir, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("staterepo: removing %s: %w", path, err)
	}
	return nil
}

// workflowRefused reports whether a failed push was GitHub declining a
// change under .github/workflows for want of the permission to make one.
//
// Matched on the message because that is the only thing git gives us: a
// refusal is a non-zero exit and the remote's own text, and GitHub's
// text is stable and specific -- "refusing to allow a GitHub App / an
// OAuth App / a Personal Access Token to create or update workflow ...
// without `workflows` permission" (or "`workflow` scope"). Read too
// narrowly this costs a deployment one undone commit a day; read too
// widely it would undo a commit over some other push failure, which is
// why both halves have to be there.
func workflowRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "refusing to allow") && strings.Contains(msg, "workflow")
}

// workflowDue reports whether grain should offer the workflow to this
// host's credential now. A marker that is missing, empty or unreadable
// means yes: the cost of trying is one commit that is undone again, and
// the cost of not trying is a repository with no CI step forever.
func (r *Repo) workflowDue(ctx context.Context, now time.Time) bool {
	path, err := r.gitDirFile(ctx, workflowRefusedFile)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	return !now.Before(at.Add(workflowRetryInterval))
}

func (r *Repo) recordWorkflowRefused(ctx context.Context, at time.Time) error {
	path, err := r.gitDirFile(ctx, workflowRefusedFile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("staterepo: writing %s: %w", path, err)
	}
	return nil
}
