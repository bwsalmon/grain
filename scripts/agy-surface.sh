#!/usr/bin/env bash
# agy-surface.sh -- read agy's surface off the installed binary and write
# it down as Markdown: every flag, subcommand, changelog entry and model
# name it will admit to, what it does with a HOME it has never seen, which
# of several candidate paths it actually reads its customizations from,
# and the config keys sitting in its string table.
#
# Why this exists. agy is a ~200MB stripped Go binary whose only
# documentation is what it prints, it is installed unpinned (the
# Dockerfile fetches whatever antigravity.google/cli/install.sh is serving
# that day), and pkg/agent/antigravity depends on a dozen facts about it
# -- three config paths, a hook contract, a flag set, a model catalog --
# that nothing in this repository can check for itself. Every task that
# has had to re-answer "what can agy be configured to do?" has re-derived
# it by hand, and a grain sandbox cannot: no network beyond the git proxy,
# and no agy in the image. So the derivation lives here instead, as a
# script CI runs on demand (.github/workflows/agy-surface.yml), and its
# output is committed to docs/agy-surface.md -- where the next question
# costs one dispatch, and the next agy release shows up as a diff.
#
# Usage:
#   scripts/agy-surface.sh [-o docs/agy-surface.md]
#
# The binary is $AGY or $GRAIN_AGY_PATH, defaulting to `agy` on $PATH --
# the same resolution order the daemon's own -agy-path flag has.
#
# Two rules shape what goes in the output.
#
# Nothing here may fail the run. Every probe is a question this script
# does not know the answer to -- that is the point of asking the binary --
# so a command that exits non-zero, times out or prints nothing records
# exactly that and the script carries on. A probe that stopped answering
# (`agy -p /permissions` wanting a credential, say) is a finding, and it
# belongs in the diff rather than in a red job.
#
# And the output has to be stable enough that a diff means drift. Nothing
# run-specific goes in it -- no dates, no run numbers, no temporary paths:
# every capture is scrubbed of the throwaway HOMEs it was taken in and
# every extracted list is sorted, so that two runs against the same agy
# produce the same bytes and a run against a different agy produces a diff
# that is entirely about agy.

# Not -e: see the first rule above. A probe's failure is an answer.
set -uo pipefail

AGY="${AGY:-${GRAIN_AGY_PATH:-agy}}"

# Every agy invocation is wrapped in this. A binary that decides to wait
# for a terminal, open a browser or serve something (agy has both
# `mic-serve` and `remote-control`) must not hang a CI job.
TIMEOUT="${AGY_SURFACE_TIMEOUT:-60}"

# Per-capture caps, so one verbose probe cannot turn a reviewable document
# into a megabyte nobody reads. A capture that hits either says so in
# place, rather than ending mid-answer.
MAX_LINES="${AGY_SURFACE_MAX_LINES:-400}"
MAX_BYTES="${AGY_SURFACE_MAX_BYTES:-65536}"

# Six backticks, because much of what is captured here is itself Markdown
# -- agy unpacks its own customization guides, fenced code blocks and all,
# into any fresh HOME -- and a three-backtick fence would be closed by the
# first fence inside one of them.
FENCE='``````'

# Byte-wise sorting and matching everywhere, so the document does not
# depend on the locale of whoever ran it.
export LC_ALL=C

# Defaulted rather than assumed: scrub below rewrites $HOME out of every
# capture, and a HOME-less environment would abort the script under -u
# instead.
HOME="${HOME:-/root}"

out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o|--output) out="${2:-}"; shift 2 ;;
    -h|--help)   sed -n '2,26p' "$0"; exit 0 ;;
    *)
      echo "agy-surface.sh: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v "$AGY" >/dev/null 2>&1 && [[ ! -x "$AGY" ]]; then
  echo "agy-surface.sh: no agy at '$AGY'. Set AGY or GRAIN_AGY_PATH, or install it the way the Dockerfile does (antigravity.google/cli/install.sh)." >&2
  exit 2
fi

probe_root="$(mktemp -d "${TMPDIR:-/tmp}/agy-surface.XXXXXX")"
trap 'rm -rf "$probe_root"' EXIT

if [[ -n "$out" ]]; then
  exec >"$out"
fi

# ---------------------------------------------------------------- helpers

# scrub removes everything about *this* run from a capture: terminal
# colour, the throwaway HOME the probe ran in, the real HOME agy was
# installed into, and the checkout it was run from. What is left should be
# the same bytes on a laptop and on a runner.
scrub() {
  sed -e 's/\x1b\[[0-9;?]*[a-zA-Z]//g' \
      -e 's/\r$//' \
      -e "s#${probe_root}#\$PROBE#g" \
      -e "s#${PWD}#\$PWD#g" \
      -e "s#${HOME}#\$HOME#g"
}

# limit applies the two caps, announcing either in the document.
limit() {
  awk -v max="$MAX_LINES" -v maxb="$MAX_BYTES" '
    NR > max { print "... [truncated at " max " lines by scripts/agy-surface.sh]"; exit }
    {
      bytes += length($0) + 1
      if (bytes > maxb) { print "... [truncated at " maxb " bytes by scripts/agy-surface.sh]"; exit }
      print
    }'
}

# block writes one capture: the command as a reader would type it, its
# output, and its exit status when that status was not zero. An empty
# capture says so, because "printed nothing" and "was never run" have to
# look different in a document read as evidence.
block() {
  local label="$1" status="$2" output="$3"
  # Trailing blank lines only ever come from a summary this script built
  # itself; dropping them keeps a regenerated document from differing in
  # whitespace nobody changed.
  while [[ "$output" == *$'\n' ]]; do output="${output%$'\n'}"; done
  printf '%sconsole\n$ %s\n' "$FENCE" "$label"
  if [[ -n "$output" ]]; then
    printf '%s\n' "$output" | scrub | limit
  else
    printf '[no output]\n'
  fi
  if [[ "$status" -ne 0 ]]; then
    printf '[exit %s]\n' "$status"
  fi
  printf '%s\n\n' "$FENCE"
}

# agy_run runs agy in a given HOME and leaves its combined output in
# agy_output and its status in agy_status. Globals rather than a captured
# stdout because `x="$(f)"` runs f in a subshell, where the exit status of
# the binary -- half of what this script is recording -- would be lost.
agy_output=""
agy_status=0
agy_run() {
  local home="$1"; shift
  agy_output="$(HOME="$home" NO_COLOR=1 TERM=dumb CI=1 timeout "$TIMEOUT" "$AGY" "$@" 2>&1)"
  agy_status=$?
}

# agy_capture is the pair of them: run, and write the block.
agy_capture() {
  local home="$1"; shift
  agy_run "$home" "$@"
  block "agy $*" "$agy_status" "$agy_output"
}

# fresh_home is a HOME agy has never seen, which is the only condition
# under which several of these questions have an answer at all: what it
# unpacks, what it discovers, and what it reads when nothing else is
# there.
fresh_home() {
  mktemp -d "$probe_root/home.XXXXXX"
}

# plant writes a file and every directory above it.
plant() {
  local path="$1" content="$2"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" >"$path"
}

# subcommands_of pulls command names out of a --help listing: the indented
# rows under a "Commands:" heading, with `a, b` and `a/b` aliases split
# apart. Deliberately generous about the heading, since the shape of that
# listing is agy's to change -- and a discovery that finds nothing is
# reported where it happens rather than leaving an empty section that
# reads like "agy has no subcommands".
subcommands_of() {
  awk '
    /^[A-Za-z ]*Commands:/ { inside = 1; next }
    /^[^[:space:]]/        { inside = 0 }
    inside && /^[[:space:]]+[a-z][a-z0-9-]*/ {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      split(line, fields, /[[:space:]]+/)
      names = fields[1]
      gsub(/[,\/]/, " ", names)
      split(names, each, / /)
      for (i in each) if (each[i] != "") print each[i]
    }' | sort -u
}

# in_binary greps the binary's own string table. It is how the config
# schema is read: agy is stripped, but a Go binary still carries every
# struct tag, every JSON-schema description and every literal path its own
# code names, and those are the keys a settings file may use.
binary_path=""
in_binary() {
  [[ -n "$binary_path" ]] || return 0
  strings -n 4 -- "$binary_path" 2>/dev/null | grep -aoE "$1" | sort -u
}

# extract writes one string-table finding as its own block, under what it
# is looking for in words and above the command that looked -- which is
# the real pattern, so that a reader who doubts a finding can run it.
extract() {
  local label="$1" pattern="$2"
  printf '**%s**\n\n' "$label"
  block "strings -n 4 agy | grep -aoE '$pattern' | sort -u" 0 "$(in_binary "$pattern")"
}

# ----------------------------------------------------------------- header

cat <<'MARKDOWN'
<!-- Generated by scripts/agy-surface.sh. Do not edit by hand: the next
     dispatch of .github/workflows/agy-surface.yml overwrites it. -->

# agy's surface, read off the binary

Everything the installed `agy` will say about itself, captured by
`scripts/agy-surface.sh` and regenerated on demand by
`.github/workflows/agy-surface.yml` (`workflow_dispatch` only, no
schedule, no credential). `pkg/agent/antigravity` depends on a dozen facts
about a binary nothing in this repository can check -- its flags, its
config paths, its hook contract, its model catalog -- and that binary is
installed unpinned, so what a freshly built image carries can change on a
day nobody touched this repository. This file is the binary's own answer,
in the tree, where a re-derivation costs one dispatch and a new agy
release arrives as a diff.

Read it as evidence rather than as documentation: every section below is a
command and its output, the commands that failed included. A probe that
stopped answering is itself a finding. What was made of the first such
capture is README's "agy 1.1.26 has no denylist for its own native tools",
and the caveat recorded there governs everything here -- agy ignores an
unknown key in silence, so a key appearing below is not proof that writing
it does anything.

MARKDOWN

# ------------------------------------------------------------ the binary

printf '## The binary\n\n'

agy_run "$(fresh_home)" --version
block "agy --version" "$agy_status" "$agy_output"

# The install symlinks agy onto $PATH from wherever its installer put it
# (the Dockerfile and live-agent.yml both *find* it rather than assuming a
# path), so resolve it: the string-table reads below need the real file,
# and its digest is what says "the same binary as last time" on a day the
# version string alone would not.
resolved="$(command -v "$AGY" 2>/dev/null || printf '%s' "$AGY")"
binary_path="$(readlink -f "$resolved" 2>/dev/null || printf '%s' "$resolved")"
[[ -r "$binary_path" ]] || binary_path=""

printf '%sconsole\n' "$FENCE"
if [[ -n "$binary_path" ]]; then
  printf 'path   %s\n' "$(printf '%s' "$binary_path" | scrub)"
  printf 'size   %s bytes\n' "$(stat -c %s "$binary_path" 2>/dev/null || echo unknown)"
  printf 'sha256 %s\n' "$(sha256sum "$binary_path" 2>/dev/null | cut -d' ' -f1)"
else
  printf '[the binary could not be resolved or read; the string-table section below is empty]\n'
fi
printf '%s\n\n' "$FENCE"

# ---------------------------------------------------- commands and flags

printf '## Commands and flags\n\n'

help_home="$(fresh_home)"
agy_run "$help_home" --help
help_out="$agy_output"
block "agy --help" "$agy_status" "$help_out"

subs="$(printf '%s\n' "$help_out" | scrub | subcommands_of)"
if [[ -z "$subs" ]]; then
  printf 'No subcommands could be read out of that listing: its shape has changed, and `subcommands_of` in `scripts/agy-surface.sh` is what needs updating.\n\n'
fi

for sub in $subs; do
  # `help` is the listing above under another name, and `install`/`update`
  # would replace the very binary this document is about.
  case "$sub" in
    help|install|update) continue ;;
  esac
  printf '### `agy %s`\n\n' "$sub"
  agy_run "$help_home" "$sub" --help
  sub_out="$agy_output"
  block "agy $sub --help" "$agy_status" "$sub_out"

  # One level deeper, which is where the interesting ones live: `agy mcp`
  # is a group, and its add/list/remove are what write and read the file
  # pkg/agent/antigravity depends on.
  for inner in $(printf '%s\n' "$sub_out" | scrub | subcommands_of); do
    [[ "$inner" != "help" ]] || continue
    agy_capture "$help_home" "$sub" "$inner" --help
  done
done

# --------------------------------------------- what agy documents

printf '## What agy documents about itself\n\n'

printf 'The changelog ships *in* the binary, which makes it the closest thing to release notes there is: a key or a flag appearing here is the announcement of it.\n\n'
agy_capture "$(fresh_home)" changelog

printf 'The model catalog, whose names carry their reasoning effort. `antigravity.DefaultModel` has to be one of these, and so does anything a deployment puts in Settings.\n\n'
agy_capture "$(fresh_home)" models

# ----------------------------------------------------------- a fresh HOME

printf '## A HOME agy has never seen\n\n'

printf 'What the binary unpacks into an empty `HOME` the first time it runs there -- the layout `writeAgyHome` builds against, and where agy'"'"'s own customization guides come from.\n\n'

layout_home="$(fresh_home)"
agy_run "$layout_home" --version
layout="$(cd "$layout_home" && find . -mindepth 1 -printf '%y %P\n' 2>/dev/null | sort)"
block "find \$HOME -mindepth 1 -printf '%y %P' | sort" 0 "$layout"

# Those guides are the only documentation agy publishes for the things
# this repository configures -- hooks.md is where hookConfigJSON's
# contract is quoted from -- so they are carried here in full rather than
# linked to, since a sandbox that needs them can fetch nothing.
printf '### The guides it unpacks\n\n'
guides="$(cd "$layout_home" && find . -type f -name '*.md' -size -128k 2>/dev/null | sort)"
if [[ -z "$guides" ]]; then
  printf 'It unpacked no Markdown at all into a fresh HOME.\n\n'
else
  while IFS= read -r guide; do
    [[ -n "$guide" ]] || continue
    printf '#### `%s`\n\n' "${guide#./}"
    printf '%smarkdown\n' "$FENCE"
    scrub <"$layout_home/${guide#./}" | limit
    printf '%s\n\n' "$FENCE"
  done <<<"$guides"
fi

# --------------------------------------------------- what it reads back

printf '## What it reads back\n\n'

printf 'Print mode answers `/permissions` and `/hooks` without an agent turn or a credential, which is the only way this repository can check that the files `pkg/agent/antigravity` writes are files agy actually loads. All three are planted below in the shape that package writes them.\n\n'

read_home="$(fresh_home)"
plant "$read_home/.gemini/antigravity-cli/settings.json" \
  '{"permissions":{"allow":["mcp_grain-sandbox_run_command"],"deny":["run_command","write_to_file"]},"modelProvider":"gemini"}'
plant "$read_home/.gemini/config/hooks.json" \
  '{"agy-surface-probe":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/bin/true","timeout":30}]}]}}'
plant "$read_home/.gemini/config/mcp_config.json" \
  '{"mcpServers":{"agy-surface-probe":{"command":"/bin/true","args":[],"timeoutSeconds":7200,"tools":[]}}}'

printf '### The permission rules it loaded\n\n'
agy_capture "$read_home" -p /permissions

printf '### The hooks it loaded\n\n'
agy_capture "$read_home" -p /hooks

printf '### The MCP servers it loaded\n\n'
agy_capture "$read_home" mcp list

printf '### The session it opens\n\n'
printf 'The same argv `Framework.Run` builds, against the same private-HOME shape, with a throwaway prompt. Without a credential the run itself fails -- what is wanted is the `init` event agy emits first, which carries the permission mode and the *native* tool roster that `withheldNativeTools` has to keep up with.\n\n'

init_raw="$(printf '%s\n' '{"event":"user","message":{"role":"user","content":"hello"}}' |
  HOME="$read_home" NO_COLOR=1 TERM=dumb CI=1 timeout "$TIMEOUT" "$AGY" \
    --input-format stream-json --output-format stream-json \
    --dangerously-skip-permissions --disable-slash-commands \
    --print-timeout 20s 2>&1)"
init_status=$?
block "agy --input-format stream-json --output-format stream-json --dangerously-skip-permissions --disable-slash-commands --print-timeout 20s" \
  "$init_status" "$init_raw"

if command -v jq >/dev/null 2>&1; then
  roster="$(printf '%s\n' "$init_raw" | jq -r 'select(.event=="init") | "permission_mode: " + (.init.permission_mode // "-"), (.init.tools // [] | sort | .[])' 2>/dev/null)"
  block "that init event's own roster, sorted" 0 "$roster"
fi

# ------------------------------------------------------ which paths win

printf '## Which paths it reads\n\n'

printf 'Candidate locations, planted all at once in one HOME, each holding a marker named after the directory it sits in: whichever markers come back are the paths agy reads, and the rest are places this repository must not write. The mistake this catches is `~/.gemini/settings.json`, which Gemini CLI read and agy ignores in silence.\n\n'

agent_dirs=(
  ".gemini/antigravity-cli/agents"
  ".gemini/agents"
  ".gemini/config/agents"
  ".agy/agents"
  ".config/agy/agents"
  ".antigravity/agents"
)
paths_home="$(fresh_home)"
agent_names=()
for dir in "${agent_dirs[@]}"; do
  name="probe-$(printf '%s' "$dir" | tr './' '--')"
  agent_names+=("$name")
  plant "$paths_home/$dir/$name.md" "---
name: $name
description: a marker planted by scripts/agy-surface.sh in $dir
---

Nothing. This agent exists so that \`agy agents\` listing it proves the directory is read."
done

printf '### Custom agents\n\n'
agy_run "$paths_home" agents
agents_out="$agy_output"
block "agy agents" "$agy_status" "$agents_out"

summary=""
for i in "${!agent_dirs[@]}"; do
  verdict="not listed"
  [[ "$agents_out" == *"${agent_names[$i]}"* ]] && verdict="READ"
  summary+="$(printf '%-10s ~/%s' "$verdict" "${agent_dirs[$i]}")"$'\n'
done
block "which planted agent directories were read" 0 "$summary"

mcp_files=(
  ".gemini/config/mcp_config.json"
  ".gemini/settings.json"
  ".gemini/antigravity-cli/mcp_config.json"
  ".gemini/antigravity-cli/settings.json"
  ".config/agy/mcp_config.json"
  ".agy/mcp_config.json"
)
mcp_home="$(fresh_home)"
mcp_names=()
for file in "${mcp_files[@]}"; do
  name="probe-$(printf '%s' "$file" | tr './' '--')"
  mcp_names+=("$name")
  plant "$mcp_home/$file" "{\"mcpServers\":{\"$name\":{\"command\":\"/bin/true\",\"args\":[]}}}"
done

printf '### MCP servers\n\n'
agy_run "$mcp_home" mcp list
mcp_out="$agy_output"
block "agy mcp list" "$agy_status" "$mcp_out"

summary=""
for i in "${!mcp_files[@]}"; do
  verdict="not listed"
  [[ "$mcp_out" == *"${mcp_names[$i]}"* ]] && verdict="READ"
  summary+="$(printf '%-10s ~/%s' "$verdict" "${mcp_files[$i]}")"$'\n'
done
block "which planted MCP config files were read" 0 "$summary"

# ---------------------------------------------------- the string table

printf '## The config schema, out of the string table\n\n'

printf 'agy is stripped, but a Go binary carries every struct tag, every JSON-schema description and every literal path its own code names. This is where the keys a settings file may use come from -- with the caveat this file opens with: an unknown key is ignored in silence, so a key appearing here is not proof that writing it does anything, and a *known* key given a value that does not parse can drop a whole agent without a word.\n\n'

extract 'json:"..."'                   'json:"[A-Za-z0-9_.,-]+"'
extract 'yaml:"..."'                   'yaml:"[A-Za-z0-9_.,-]+"'
extract 'mapstructure:"..."'           'mapstructure:"[A-Za-z0-9_.,-]+"'
extract 'jsonschema_description:"..."' 'jsonschema_description:"[^"]{1,120}"'
extract 'paths under .gemini'          '\.gemini/[A-Za-z0-9_./-]+'
extract 'settings, config and hook file names' '[A-Za-z0-9_-]*(settings|config|hooks|agents|permissions)[A-Za-z0-9_-]*\.(json|yaml|yml|md|toml)'
extract 'names that read like a tool roster'   '(enabled|disabled|allowed|denied|withheld)[A-Z][A-Za-z]{2,30}'
extract 'names that read like a permission'    '(permission|decision|hook|sandbox)[A-Z][A-Za-z]{2,30}'
