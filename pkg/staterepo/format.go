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
// stops. A push that adds a file under .github/workflows is refused
// unless the credential making it may write workflows, which grain's
// own installation token need not be able to do -- and a state
// repository whose sync loop is wedged on a permission is a deployment
// that cannot save its own settings. So the workflow is committed by
// whoever ran the command, from a clone of their own, and grain's timer
// never carries one.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

const workflow = `# Written by ` + "`grain state format`" + `. Checks that grain can still load
# this repository.
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
