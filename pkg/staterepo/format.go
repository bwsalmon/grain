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
// `grain state check` out of when nothing names a better one.
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
// older tag wants that tag here. A deployment does not have to be told
// which one it is: cmd/grain stamps its own image reference in at link
// time and passes it as Config.CheckImage, exactly as it does for the
// sandbox (cmd/grain/grainimage.go), and this value is what an unstamped
// build -- a `make build` on a laptop, a `docker build` with no build arg
// -- falls back to. Naming the tag CI keeps pointed at main is a less
// precise answer than that stamp and a far better one than nothing.
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
// already there, which is otherwise left as it is but for the image line
// of one that is still grain's own rendering (EnsureWorkflow) -- an
// operator who has edited the runner, the trigger or a step has said
// something this command does not know better than.
//
// Safe to run twice: nothing here is destructive, and a directory that
// is already formatted comes out byte-identical.
func Format(dir, image string, force bool) (Formatted, error) {
	var out Formatted
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, fmt.Errorf("staterepo: preparing %s: %w", dir, err)
	}
	// Named from the dump if there is one under dir, since there is no
	// database here to ask: formatting a clone of a deployment's
	// repository has to leave its README saying whose state it is
	// (deploymentNameFromDump), and formatting an empty directory has
	// nothing to name until the deployment that adopts it seeds.
	if err := writeReadme(dir, deploymentNameFromDump(dir)); err != nil {
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
// unless force -- with the one exception below -- and reported as
// (false, nil) rather than as an error: "the CI step is already
// installed" is an answer, not a failure.
//
// The exception is the image. A file that is byte for byte grain's own
// rendering, differing from what this build would write in nothing but
// the image it names, is repointed at image rather than left: see
// StaleImage for why that one line is grain's and the rest of the file
// is not.
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
		switch body, err := os.ReadFile(path); {
		case err == nil:
			if !StaleImage(body, image) {
				return false, nil
			}
		case !os.IsNotExist(err):
			return false, fmt.Errorf("staterepo: reading %s: %w", path, err)
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

// RenderedImage reports the image a workflow file runs the check from,
// and whether that file is one grain rendered rather than one somebody
// has since made their own.
//
// The test is byte equality with this build's own template: everything
// before the image and everything after it has to be exactly what
// Workflow would have written, and what is left in the middle is the
// image. Nothing is parsed as YAML, because the question is not "what
// does this workflow do" -- which grain has no business deciding for a
// file an operator is entitled to own -- but the far narrower "is this
// still grain's text, with only the one substitution in it".
//
// So a file with a runner, a trigger or a step of somebody's own in it
// answers false, and so, deliberately, does one rendered by a grain
// whose template read differently from this one. That direction is the
// safe one: the cost of failing to recognise grain's own older file is
// an image line that stops being maintained, which is where every one of
// these files stood before this build; the cost of the other mistake is
// grain overwriting something a human wrote.
func RenderedImage(body []byte) (string, bool) {
	prefix, suffix, ok := strings.Cut(workflow, workflowImagePlaceholder)
	if !ok {
		return "", false
	}
	s := string(body)
	if len(s) < len(prefix)+len(suffix) || !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return "", false
	}
	image := s[len(prefix) : len(s)-len(suffix)]
	// A rendered image is one token on one line. Anything else means the
	// file only looks like the template around an edit that happens to
	// sit where the image goes.
	if image == "" || strings.ContainsAny(image, "\n\"") {
		return "", false
	}
	return image, true
}

// StaleImage reports whether a workflow file is grain's own rendering
// pointed at some image other than want -- the one edit grain makes to a
// file that is already there.
//
// Which needs saying, because "grain never rewrites this file" is
// otherwise the rule, and it is the rule that keeps an operator's edit
// from fighting a timer. The image is the exception because it is not
// really a fact about the repository at all: it is a fact about the
// deployment, the one that says which build's schema this dump was
// written by, and it goes out of date on its own every time that
// deployment is upgraded. A file written once with the tag of the day
// fails every pull request the moment the schemas part company, for a
// reason that has nothing to do with the change proposed in it -- so
// grain keeps that line, and only that line, in step with the image it
// is running.
//
// An operator who wants a different one says so where the deployment can
// read it, in state-repo.json's "checkImage": grain then writes and
// maintains *that* value, so the pin is stable rather than something a
// later sync takes back. Editing anything else in the file hands the
// whole of it back to whoever edited it, image included.
func StaleImage(body []byte, want string) bool {
	image, ok := RenderedImage(body)
	return ok && image != want
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
# repository -- and afterwards changes nothing in it but IMAGE below,
# which it keeps pointed at the build the deployment is actually
# running. So an edit to it is safe: change the runner, add a step of
# your own, and grain leaves the file alone from then on, image
# included. To pin the image and keep the rest of this file grain's,
# set "checkImage" in the deployment's state-repo.json -- that is what
# IMAGE is written from. To stop grain offering the file at all, set
# "noWorkflow": true there instead.
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
# apart. The deployment writes its own image reference here to keep them
# together -- ` + "`grain image`" + ` on its host prints the tag it runs, and
# ` + "`grain state status`" + ` prints both schema numbers.
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
	"when it is missing, and afterwards maintains nothing in it but the\n" +
	"image the check runs, which is the one line that is about the\n" +
	"deployment rather than about this repository."

// workflowImageCommitMessage is what the commit that repoints an
// already-installed check at this deployment's own image says.
//
// A reviewer of this repository sees a one-line diff arrive out of
// nowhere, so it has to say what moved and why a machine is allowed to
// move it: the check has to run a build that knows the same schema as
// the deployment, the deployment is the only thing that knows which
// build that is, and every other line in the file is still whoever
// edited it last's.
const workflowImageCommitMessage = "Point the check at the image this deployment runs\n\n" +
	"Written by grain, and only this one line of it: `grain state check`\n" +
	"refuses a dump stamped with a schema it does not know, so a check\n" +
	"left pointing at the build of some earlier day fails every pull\n" +
	"request here for a reason that has nothing to do with the change in\n" +
	"it. Set \"checkImage\" in the deployment's state-repo.json to choose\n" +
	"the image yourself, or edit anything else in the file to have grain\n" +
	"leave the whole of it alone."

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
// -- running the check on their own infrastructure is the obvious case
// -- and a file grain rewrote on every tick would be a file whose editor
// is fighting a timer. So an edit made here, or merged in through a pull
// request, is final: grain notices there is a workflow that is not its
// own text and stops.
//
// The image is the exception, and the only one: a file that is still
// grain's rendering word for word gets its IMAGE line moved to whatever
// this deployment is running (StaleImage). That line is a fact about the
// deployment rather than about the repository, and a deployment that has
// been upgraded since the file was written is a deployment whose check
// now runs an older schema than its dump -- which fails every pull
// request against it. Repointing it costs a one-line commit on the sync
// after an upgrade, and nothing at all after that.
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
	image := r.cfg.CheckImage
	if image == "" {
		image = DefaultCheckImage
	}
	path := filepath.Join(r.cfg.Dir, filepath.FromSlash(WorkflowFile))
	// present says which of the two commits this is -- installing a file
	// that is not there, or moving the image line of one that is -- and
	// the undo below needs it too: a file that was already committed is
	// restored rather than deleted.
	var present bool
	switch body, err := os.ReadFile(path); {
	case err == nil:
		if !StaleImage(body, image) {
			// The file is here and points where it should -- grain wrote
			// it, somebody ran `grain state ci` and committed it, or a
			// merge brought it in -- so a refusal recorded earlier is a
			// refusal that has been dealt with, and the marker goes. It is
			// what WorkflowRefusedAt answers the State pane out of, and a
			// pane that went on saying the check is missing after somebody
			// installed it by hand would be telling an operator their own
			// fix did not work.
			//
			// Only on this branch, and not on the stale-image one below:
			// there the marker is still doing its other job, which is
			// holding the retry down to one attempt a day, and a sync that
			// cleared it on the way past would offer the same one-line
			// commit to the same credential on every tick.
			r.clearWorkflowRefused(ctx)
			return false, nil
		}
		present = true
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
	if _, err := EnsureWorkflow(r.cfg.Dir, image, false); err != nil {
		return false, err
	}
	message := workflowCommitMessage
	if present {
		message = workflowImageCommitMessage
	}
	// Staged and committed by path, so this commit holds the workflow and
	// nothing but the workflow whatever else happens to be in the working
	// tree -- which is what makes undoing it a matter of dropping one
	// commit rather than a judgement about what else went with it.
	if _, err := r.git(ctx, "add", "--", WorkflowFile); err != nil {
		return false, err
	}
	if _, err := r.git(ctx, "commit", "--quiet", "-m", message, "--", WorkflowFile); err != nil {
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
	if err := r.undoWorkflowCommit(ctx, before, path, present); err != nil {
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
	// The same refusal, in the words of whichever commit it was: a
	// deployment that has no check at all and one whose check runs the
	// wrong build read very differently to whoever is holding the
	// journal, and `grain state ci` is the answer to both -- with -force
	// and -image for the second, since the file that is in the way there
	// is grain's own.
	if present {
		log.Printf("staterepo: this deployment's credential may not push %s to %s, so grain cannot "+
			"point the check that runs on pull requests against its own state at %s, the image this "+
			"deployment runs (%v). Until it does, that check runs an older build and may fail every "+
			"pull request with a schema mismatch: run `grain state ci -force -image %s` in a clone "+
			"and commit the file with a credential that may. grain will try again in %s.",
			WorkflowFile, r.cfg.Remote, image, pushErr, image, workflowRetryInterval)
		return false, r.recordWorkflowRefused(ctx, r.now())
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
// already. What is undone here is exactly the file this step wrote.
//
// present says the commit moved an image line rather than adding the
// file, and the working tree is put back accordingly: the version at
// `before` is checked back out instead of the file being deleted.
// Deleting it would turn a refused one-line change into the loss of a CI
// step the repository already had -- and, worse, into an offer to
// reinstall the whole file on the next tick, which is a second commit for
// the same credential to refuse.
func (r *Repo) undoWorkflowCommit(ctx context.Context, before, path string, present bool) error {
	if _, err := r.git(ctx, "reset", "--soft", before); err != nil {
		return fmt.Errorf("staterepo: undoing the workflow commit in %s: %w", r.cfg.Dir, err)
	}
	// Resets the index entry to what `before` holds -- the old file, or
	// nothing at all when there was none.
	if _, err := r.git(ctx, "reset", "-q", "--", WorkflowFile); err != nil {
		return fmt.Errorf("staterepo: unstaging %s in %s: %w", WorkflowFile, r.cfg.Dir, err)
	}
	if present {
		if _, err := r.git(ctx, "checkout", "--", WorkflowFile); err != nil {
			return fmt.Errorf("staterepo: restoring %s in %s: %w", WorkflowFile, r.cfg.Dir, err)
		}
		return nil
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

// WorkflowRefusedAt reports whether this deployment is running without
// the CI step because its credential was refused it, and when it was
// last refused.
//
// This is the journal line above read back out, for the State pane. A
// deployment that cannot install the check looks, from that pane,
// exactly like one whose check runs on every pull request: it is syncing
// happily, its remote is up to date, nothing is in error -- because none
// of that is untrue, and the refusal is deliberately not an error, since
// a deployment must not stop syncing over a file worth one CI step. So
// the one place the fact appears is a line in the journal, seen by
// whoever was reading it that minute, and "the check is missing" is
// exactly the sort of thing nobody notices until a change that would not
// load is merged -- which is the failure the check exists to prevent.
//
// False whenever there is nothing to say, which is every ordinary
// deployment: one not offering the workflow at all (no remote, or
// noWorkflow set), one whose repository has the file -- installWorkflow
// clears the marker as soon as it sees one, however it arrived -- and
// one that has never been refused.
//
// The zero time with true is a marker grain wrote and cannot now read.
// The refusal it records still happened, so it is still reported; only
// the date is unknown, and the next sync's attempt writes a fresh one
// either way.
func (r *Repo) WorkflowRefusedAt(ctx context.Context) (time.Time, bool) {
	if r.cfg.NoWorkflow || r.cfg.Remote == "" {
		return time.Time{}, false
	}
	if _, err := os.Stat(filepath.Join(r.cfg.Dir, filepath.FromSlash(WorkflowFile))); err == nil {
		return time.Time{}, false
	}
	return r.readWorkflowRefused(ctx)
}

// readWorkflowRefused reads the marker: when this host's credential was
// last refused the workflow, and whether there is a marker at all. It
// answers about the marker alone -- WorkflowRefusedAt is the question an
// operator is actually asking, and adds the conditions under which a
// marker means nothing any more.
func (r *Repo) readWorkflowRefused(ctx context.Context) (time.Time, bool) {
	path, err := r.gitDirFile(ctx, workflowRefusedFile)
	if err != nil {
		return time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, true
	}
	return at, true
}

// workflowDue reports whether grain should offer the workflow to this
// host's credential now. A marker that is missing, empty or unreadable
// means yes: the cost of trying is one commit that is undone again, and
// the cost of not trying is a repository with no CI step forever.
func (r *Repo) workflowDue(ctx context.Context, now time.Time) bool {
	at, refused := r.readWorkflowRefused(ctx)
	if !refused || at.IsZero() {
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

// clearWorkflowRefused forgets a refusal the repository has outgrown.
//
// Best effort, and deliberately silent: the marker is a note to this
// host about a file that is now in the tree, and a sync that failed
// outright because it could not delete one would be a deployment that
// stopped exporting over bookkeeping. What a marker left behind costs is
// one stale sentence in the State pane until the next sync tries again.
func (r *Repo) clearWorkflowRefused(ctx context.Context) {
	path, err := r.gitDirFile(ctx, workflowRefusedFile)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}
