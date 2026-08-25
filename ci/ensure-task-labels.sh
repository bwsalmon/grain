#!/usr/bin/env bash
# Create every label the task queue runs on, in the config repo whose
# deploy workflow calls this.
#
# It is called from *that* repo's .github/workflows/deploy.yml, out of the
# grain checkout it already makes -- so a deployment picks up a new label
# by moving grain_ref, never by re-syncing a list into its own workflow.
# `grain/automation/labels.py` holds the list and explains why it lives
# there rather than in the workflow.
#
# Needs `gh` (stock on a GitHub runner) authenticated as something with
# `issues: write` on the repo, via GH_TOKEN, and python3 for the label
# table. Converges rather than creates: a label that already exists is the
# normal case on every deploy after the first, and has its colour and
# description brought back in line with the table (`gh label create
# --force`) rather than being left alone. Without that, the table would
# only ever describe repos that had never seen the label, and a palette
# change could not reach a running deployment at all.
set -euo pipefail

repo="${1:-${GITHUB_REPOSITORY:-}}"
if [ -z "$repo" ]; then
  echo "usage: ${0##*/} <owner/repo>" >&2
  exit 2
fi

# grain's own root, two levels up from this file -- so the caller passes a
# repo and nothing else, wherever it checked grain out to.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Read the whole table before touching GitHub: a failure to produce it at
# all (a broken checkout, a python that cannot import grain) should stop
# here with python's own traceback, not half-create a label set.
labels="$(PYTHONPATH="$root" python3 -m grain.automation.labels)"

# One listing up front, purely so the log can say which labels actually
# moved -- `--force` alone reports a recolour and a no-op identically, and
# "recoloured 4 labels" is the line worth having in the run that follows a
# palette change. Best-effort: if the listing fails, every label just
# reports as created.
before="$(gh label list --repo "$repo" --limit 500 \
            --json name,color,description \
            --jq '.[] | [.name, .color, .description] | @tsv' 2>/dev/null || true)"

was() {  # existing "colour<TAB>description" for a label, empty if it is new
  printf '%s\n' "$before" | awk -F'\t' -v want="$1" \
    '$1 == want { print $2 "\t" $3; exit }'
}

while IFS=$'\t' read -r name color description; do
  [ -n "$name" ] || continue
  existing="$(was "$name")"
  if out="$(gh label create "$name" --repo "$repo" \
              --color "$color" --description "$description" --force 2>&1)"; then
    if [ -z "$existing" ]; then
      echo "created $name"
    elif [ "$existing" = "$color$(printf '\t')$description" ]; then
      echo "$name unchanged"
    else
      echo "updated $name"
    fi
  else
    # Never fatal: the labels are convergent, and a deploy that has
    # already applied its Terraform should not be failed by a label that
    # can be fixed by hand. Surfaced as a warning rather than swallowed,
    # so a real failure (a token without issues: write, say) is visible in
    # the run's annotations instead of reading as "already exists".
    echo "::warning::could not create $name: $out"
  fi
done <<< "$labels"
