#!/usr/bin/env bash
# Installer and updater for a v2 grain deployment, run directly on the
# target machine -- bwsalmon/agents#355.
#
# v1's shape (../docs/design.md) was a controller VM plus a pool of
# sandbox VMs, all built by a Python host adapter driving libvirt -- code
# this repository no longer carries. grain has no host adapter
# yet (README.md, "What this does not have yet") and does not need
# one to be useful: its daemon already defaults to running dispatched
# work as plain host directories (orchestrator.HostSandboxes), no VM
# involved. So this script does the simpler thing the issue actually
# asks for -- run the one `grain` binary directly on this machine, as a
# single systemd service, with no controller VM anywhere in the picture.
# Real sandbox isolation (a VM or container per task) is still open and
# out of scope here; see README.md's own "neither sandbox stand-in
# carries any real isolation."
#
# What this script does, every time it runs (safe to re-run -- this is
# the installer AND the updater):
#   1. pulls the deployment image -- $GRAIN_IMAGE:$GRAIN_IMAGE_TAG,
#      published to GHCR by ../.github/workflows/build-artifacts.yml
#      on every commit -- instead of building a binary here
#      (bwsalmon/agents#645). That image carries grain *and* every binary
#      it shells out to: git, curl, the docker CLI, konturctl, and every
#      agent CLI -- claude, agy and codex (Dockerfile) -- plus a copy of the
#      source it was built from. So it is also where the handful of
#      steps here that want more than a shell go looking, rather than at
#      this host: see "What this host has to have" below
#   2. installs /usr/local/bin/grain and /usr/local/bin/konturctl as thin
#      wrappers that run that same image (install_cli_wrappers), so an
#      operator's own shell -- and the rest of this script, which uses
#      `grain schema-version` and `grain secrets` -- reaches the exact
#      build the service runs, with nothing installed on the host to
#      drift out of step with it
#   3. creates an unprivileged system user to run the container as
#   4. installs the two systemd path units that let that account act on
#      the host it cannot reach from inside a container: the UI's
#      reboot-host button (bwsalmon/agents#395) and the restart its
#      Upgrade button needs (bwsalmon/agents#396) each become a touch of
#      a file under $GRAIN_DATA_DIR/control, watched by a unit out here
#      (write_control_units). That replaces the two NOPASSWD sudoers
#      drop-ins this used to install, and grants strictly less: the
#      daemon can restart its own service and reboot this machine, and
#      has no sudo at all
#   5. lays out the rest of $GRAIN_DATA_DIR (secrets, the embedded SQLite
#      store) and seeds secrets from environment variables -- only if
#      they are not already there, so a second run never overwrites a
#      credential placed by the first one or by hand. Also lays out
#      $GRAIN_SANDBOX_DIR, HostSandboxes' own per-slot working
#      directories -- deliberately not under $GRAIN_DATA_DIR
#      (bwsalmon/agents#587): a task's checked-out repo is disposable,
#      unlike the store and secrets, so it belongs on whatever storage a
#      VM wipe or redeploy is free to discard along with the rest of the
#      host, not the one directory meant to survive it
#   6. compares the freshly built binary's schema version (`grain
#      schema-version`) against the version recorded in
#      $GRAIN_DATA_DIR/.schema_version from the last run of this script,
#      and moves $GRAIN_DATA_DIR/store aside -- never deletes it -- if
#      they differ, so grain starts a fresh, empty store rather than an
#      existing one Store.Init cannot safely bring up to date column-wise
#      (bwsalmon/agents#394; see reformat_store_if_schema_changed below).
#      A schema version that has not changed since the last run leaves
#      the store exactly as it was: this is what "preserve state across
#      updates, but reformat on a breaking change" means in practice.
#   7. if $GRAIN_TARGET_REPO is set and has no commits yet, pushes one
#      empty commit so grain has a branch to work from ("format" it --
#      grain always branches off an existing ref, never creates one)
#   8. writes and enables grain-daemon.service, so it comes back on
#      reboot, and restarts it (not just "enable --now") so a second
#      run's new binary actually takes effect -- see
#      v1's own notes on the same failure for why
#      enable-without-restart was already a bug once in v1's own proxy
#      service
#
# What this host has to have: docker and systemd. That is the whole
# list. Everything else this script runs is either a shell builtin or
# part of a base system install -- coreutils, and the `useradd` every
# distribution ships -- so there is no package a minimal cloud image
# lacks standing between an operator and a deployment.
#
# It needed `git` and `jq` until recently, and installed both itself on
# any host without them. Both are gone, and so is the checkout git was
# there to maintain:
#
#   * the source is *in* the image (Dockerfile's own
#     /usr/local/share/grain/src), so the one thing out here that still
#     reads it -- scripts/kontur's guest image build -- unpacks it from
#     the image this run is installing (unpack_image_source) instead of
#     from a checkout tracking a branch. That closes the same drift the
#     self-debug capability was moved into the image to close: a
#     checkout follows a branch and an image tag never moves, so the two
#     disagree on every upgrade and every rollback
#   * the two steps that want a real git -- the `git ls-remote` against
#     GRAIN_TARGET_REPO and the empty commit pushed to it -- run git
#     *inside* that image (image_run), which carries one
#   * so does the one that wanted curl and jq: the GCP metadata token
#     that authenticates docker to an Artifact Registry
#
# One consequence worth naming: this script no longer replaces itself
# mid-run. It used to (sync_repo pulled a new copy over the file this
# process was reading, and reexec_if_updated handed over to it), which
# is only a problem worth solving for a script that updates its own
# source. Nothing rewrites this file underneath it now, so the copy an
# operator ran is the copy that finishes. Keeping that copy current is
# the job of whatever put it there: terraform/gcp/files/deploy.sh on the
# GCP path, a `git pull` in your own checkout by hand.
#
# Every setting is an environment variable, not a flag, so the common
# case is `sudo GRAIN_GITHUB_TOKEN=... GRAIN_GEMINI_API_KEY=... ./setup.sh`
# and a re-run to pick up a newly published image is `sudo ./setup.sh`
# with no arguments at all. Run with -h/--help for the full list.
#
# Most daemon settings below (everything except GRAIN_UI_ADDR,
# GRAIN_TARGET_REPO/GRAIN_TARGET_BRANCH and GRAIN_ENABLE_UI_UPGRADE) are
# only *seeded* from these variables, the first time a deployment's store
# has none (cmd/grain/daemon.go's loadConfig, bwsalmon/agents#320) --
# passing this script a new GRAIN_GITHUB_HOST or GRAIN_MAX_WORKERS on
# a later re-run has no effect on a deployment that already has one, and
# loadConfig now logs a line saying so on every start it happens. Change
# an already-seeded value with `grain settings` (or the UI's Settings
# pane) instead -- see each variable's own note below for which ones this
# applies to (bwsalmon/agents#574).
#
# There used to be a second service (grain-ui.service) and, before
# bwsalmon/agents#366 replaced it with embedded SQLite, a `dolt
# sql-server` container behind it: embedded Dolt was single-writer, and a
# daemon plus a UI on the same store both used to need to write.
# bwsalmon/agents#363 removed the second writer -- the daemon now serves
# the UI/API itself, in-process, over the store it already has open (see
# cmd/grain/daemon.go's own doc comment) -- so this script still
# installs exactly one service and needs no separate store container.
# What did come back is `docker` at runtime: that one service is now a
# `docker run` of the deployment image rather than a binary on this
# host's disk.
#
# What the container is given, and what it deliberately is not, is worth
# reading once (docker_run_args, below). It gets host networking (the
# UI's port, and the git proxy every sandbox reaches), the data and
# sandbox directories bind-mounted at the same paths they have out here,
# and -- only when something actually needs it -- the docker socket. It
# runs as $GRAIN_USER's own uid:gid, never as root, so every file it
# writes into those directories comes out owned exactly as it was before
# any of this ran in a container at all.
#
# The UI is bound to 127.0.0.1 on the plain HTTP port (80) and nowhere
# else -- nothing here opens a firewall hole for it. Reach it by
# forwarding that one port over SSH: `ssh -L 8080:localhost:80
# <this-host>`, then open http://localhost:8080 locally, or put it behind
# Tailscale/IAP (the issue's own framing) instead of an SSH tunnel if the
# deployment already has one of those. See pkg/ui/README's own
# "single-operator tool" framing (README.md, "The UI") for why that is
# the whole access-control story today -- the API and the UI it serves
# carry no auth of their own, so whatever reaches -ui-addr can act as the
# deployment's one configured actor.

set -euo pipefail

# --- configuration (every value overridable via environment) ----------

# The branch this deployment tracks. It names no checkout -- nothing
# here keeps one (see "What this host has to have", above) -- only which
# published image to run, through GRAIN_IMAGE_TAG below.
GRAIN_REF="${GRAIN_REF:-main}"
GRAIN_DATA_DIR="${GRAIN_DATA_DIR:-/var/lib/grain}"
# Root for HostSandboxes' per-slot working directories -- only used
# without kontur sandboxing (GRAIN_KONTUR_ENABLE=0). Deliberately not
# under $GRAIN_DATA_DIR (bwsalmon/agents#587, see this file's own header
# comment, item 5): unlike GRAIN_DATA_DIR, nothing under here needs to
# survive a redeploy, so its default lives outside whatever separate,
# persistent disk an operator mounts at $GRAIN_DATA_DIR (terraform/gcp's
# own data_disk_gb): nothing under here needs to survive a redeploy.
#
# Disposable is not the same as small, though, which is why terraform/gcp
# gives this path a disk of its own too (its sandbox_disk_gb, bind-mounted
# here by files/startup.sh, and holding docker's data root beside it):
# a checkout that fills the boot disk takes the whole host down with it.
GRAIN_SANDBOX_DIR="${GRAIN_SANDBOX_DIR:-/var/lib/grain-sandbox}"
GRAIN_USER="${GRAIN_USER:-grain}"

# --- the deployment image (bwsalmon/agents#645) ------------------------
#
# GRAIN_IMAGE is the repository CI publishes to on every commit, with no
# tag: ../.github/workflows/build-artifacts.yml's own grain-container
# job. GRAIN_IMAGE_TAG picks which one to run, and defaults to the tag
# published for GRAIN_REF -- the branch this checkout tracks -- so a
# deployment pinned to a branch stays pinned to that branch's image with
# nothing extra to set. '/' is not legal in a docker tag and grain's
# branches routinely contain one, so it becomes '-', the same
# substitution CI makes when it pushes and pkg/upgrade's TagForBranch
# makes when the UI's Upgrade button resolves a branch.
#
# Set GRAIN_IMAGE_TAG explicitly to pin a deployment to one immutable
# build -- sha-<short sha>, also published on every commit -- which is
# what a rollback looks like here: re-run this script with the tag of a
# build that worked.
GRAIN_IMAGE="${GRAIN_IMAGE:-ghcr.io/bwsalmon/grain/grain}"
GRAIN_IMAGE_TAG="${GRAIN_IMAGE_TAG:-${GRAIN_REF//\//-}}"
# Credentials for `docker login` against the image's registry, needed
# only if the package is private -- bwsalmon/grain and its packages are
# public, so an ordinary deployment sets neither and pulls anonymously.
# A GitHub PAT with read:packages is what these want when they are
# needed; GRAIN_GITHUB_TOKEN is deliberately *not* reused by default,
# since the credential grain clones repositories with and the credential
# it pulls images with are not the same decision.
GRAIN_IMAGE_PULL_USER="${GRAIN_IMAGE_PULL_USER:-}"
GRAIN_IMAGE_PULL_TOKEN="${GRAIN_IMAGE_PULL_TOKEN:-}"
# Extra arguments appended to grain-daemon.service's own `docker run`,
# word-split as written -- the escape hatch for whatever docker_run_args
# below does not anticipate. An agy install that needs more of its tree
# than the directory around the binary, for instance:
#   GRAIN_EXTRA_DOCKER_ARGS="--volume /opt/gemini:/opt/gemini:ro"
GRAIN_EXTRA_DOCKER_ARGS="${GRAIN_EXTRA_DOCKER_ARGS:-}"

GRAIN_UI_ADDR="${GRAIN_UI_ADDR:-127.0.0.1:80}"
# GRAIN_MAX_CONCURRENT is GRAIN_MAX_WORKERS' former name, from before a
# deployment's concurrency was split into workers and mergers
# (model.Limits): still honoured so a re-run of this script on a host
# whose environment still sets it keeps the same worker count.
GRAIN_MAX_WORKERS="${GRAIN_MAX_WORKERS:-${GRAIN_MAX_CONCURRENT:-1}}"
GRAIN_MAX_MERGERS="${GRAIN_MAX_MERGERS:-1}"
GRAIN_POLL_INTERVAL="${GRAIN_POLL_INTERVAL:-30s}"

GRAIN_GITHUB_HOST="${GRAIN_GITHUB_HOST:-github.com}"
GRAIN_GITHUB_INSECURE_HTTP="${GRAIN_GITHUB_INSECURE_HTTP:-0}"
GRAIN_GITHUB_TOKEN="${GRAIN_GITHUB_TOKEN:-}"
GRAIN_GITHUB_CREDENTIAL_NAME="${GRAIN_GITHUB_CREDENTIAL_NAME:-bot}"
GRAIN_GITHUB_APP_ID="${GRAIN_GITHUB_APP_ID:-}"
GRAIN_GITHUB_APP_INSTALLATION_ID="${GRAIN_GITHUB_APP_INSTALLATION_ID:-}"
GRAIN_GITHUB_APP_PRIVATE_KEY="${GRAIN_GITHUB_APP_PRIVATE_KEY:-}"

GRAIN_GEMINI_API_KEY="${GRAIN_GEMINI_API_KEY:-}"
GRAIN_GEMINI_MODEL="${GRAIN_GEMINI_MODEL:-}"
# The other half of that model selection: agy is given a model and a
# reasoning effort (low|medium|high), and refuses a bare family name
# without one. Empty leaves the daemon's own default
# (antigravity.DefaultEffort), and it is ignored for a GRAIN_GEMINI_MODEL
# whose name already carries an effort -- agy's own gemini-3.1-pro-high
# spelling, which it refuses alongside an --effort that disagrees.
GRAIN_GEMINI_EFFORT="${GRAIN_GEMINI_EFFORT:-}"
# Path to an Antigravity CLI (agy) on *this host* to run instead of the
# one baked into the image -- GRAIN_CLAUDE_PATH's exact counterpart for
# the other agent framework, and empty for the same reason: the image
# carries one (Dockerfile), so an ordinary deployment resolves "agy"
# inside the container with nothing to install and nothing per-host to
# keep in step with the build.
#
# Set, this host's copy is bind-mounted into the container at that same
# path and the daemon is pointed at it (-agy-path).
GRAIN_AGY_PATH="${GRAIN_AGY_PATH:-}"
GRAIN_MAX_AGENT_TURNS="${GRAIN_MAX_AGENT_TURNS:-}"

# The Claude Code OAuth token agent/claude authenticates as, for a
# deployment whose agent-framework setting is (or may be set to)
# "claude" -- the exact counterpart of GRAIN_GEMINI_API_KEY above, seeded
# the same seed-once way into the same secrets directory. Both are
# optional now: whichever is missing can be pasted into the UI instead
# (Settings -> Agent frameworks), which is the only way to set one on a
# host an operator cannot get a shell on.
GRAIN_CLAUDE_CODE_OAUTH_TOKEN="${GRAIN_CLAUDE_CODE_OAUTH_TOKEN:-}"
# Path to a claude CLI on *this host* to run instead of the one baked
# into the image. Empty -- the default, and what an ordinary deployment
# wants -- resolves "claude" inside the container, where Dockerfile
# already installed it: nothing to download at deploy time and nothing
# per-host to keep in step with the build.
#
# Set, this host's copy is bind-mounted into the container at that same
# path and the daemon is pointed at it (-claude-path). That is the escape
# hatch for a deployment that must pin a particular CLI version, and the
# reason it is a *mount* rather than a $PATH entry: a path that resolves
# out here has to resolve identically in there.
GRAIN_CLAUDE_PATH="${GRAIN_CLAUDE_PATH:-}"
# Override the daemon's default Claude model -- the exact counterpart of
# GRAIN_GEMINI_MODEL above.
GRAIN_CLAUDE_MODEL="${GRAIN_CLAUDE_MODEL:-}"

# The OpenAI API key agent/codex authenticates as, for a deployment whose
# agent-framework setting is (or may be set to) "codex" -- the third of
# the same seed-once credentials, into the same secrets directory, and
# optional for the same reason: it can be pasted into the UI instead
# (Settings -> Agent frameworks).
GRAIN_OPENAI_API_KEY="${GRAIN_OPENAI_API_KEY:-}"
# Path to a codex CLI on *this host* to run instead of the one baked into
# the image -- GRAIN_CLAUDE_PATH/GRAIN_AGY_PATH's counterpart for the
# third agent framework, mounted in and passed as -codex-path the same
# way.
GRAIN_CODEX_PATH="${GRAIN_CODEX_PATH:-}"
# Override the daemon's default Codex model -- the exact counterpart of
# GRAIN_GEMINI_MODEL/GRAIN_CLAUDE_MODEL above.
GRAIN_CODEX_MODEL="${GRAIN_CODEX_MODEL:-}"

GRAIN_GCP_PROJECT="${GRAIN_GCP_PROJECT:-}"
GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="${GRAIN_GCP_SERVICE_ACCOUNT_EMAIL:-}"
GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="${GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE:-}"

# --- the state repository (pkg/staterepo) -------------------------------
#
# The git repository this deployment's database lives in, written out as
# text: the daemon imports it at startup and exports back into it on a
# timer, so it -- not the SQLite file -- is what an operator backs up and
# what an agent proposes a settings change against.
#
# Named here so a deployment comes up already pointed at its repository.
# The alternative is the UI's bootstrap pane, which is fine for a laptop
# and wrong for a fleet: a host that has to be visited before it knows
# where its own state lives is a host whose state is wherever the last
# person to open a browser said it was. Left empty (the default) nothing
# is written and whatever the host already decided -- the bootstrap
# pane's answer, or the local-only repository a fresh install gets --
# stands.
GRAIN_STATE_REPO_URL="${GRAIN_STATE_REPO_URL:-}"
GRAIN_STATE_REPO_BRANCH="${GRAIN_STATE_REPO_BRANCH:-main}"

# The private half of this deployment's secrets key (pkg/secrets): the
# one file that cannot be rebuilt from anything.
#
# Everything else under $GRAIN_DATA_DIR either comes back from the state
# repository or is reissued by the deploy that seeded it. This does
# neither, and nothing can stand in for it: the encrypted secrets file
# beside it (grain/task-186 moved it out of the state repository, since
# everything a sandbox can clone is everything a sandbox can read) is
# encrypted to this key alone. A host restored onto a fresh data
# directory therefore needs both halves handed back -- the secrets file
# from a backup, and this key from here -- and one that mints itself a
# fresh key instead cannot read a line of what was put beside it, which
# pkg/secrets reports as the unrecoverable state it is rather than
# starting over silently.
#
# Seeded once, on the same never-overwrite contract as every credential
# below (seed_secret): a key already on this host always wins, since it
# is the key the secrets file on this host was encrypted to.
GRAIN_SECRETS_KEY="${GRAIN_SECRETS_KEY:-}"

GRAIN_TARGET_REPO="${GRAIN_TARGET_REPO:-}"
GRAIN_TARGET_BRANCH="${GRAIN_TARGET_BRANCH:-main}"
GRAIN_TARGET_REPOS="${GRAIN_TARGET_REPOS:-}"

# See "Kontur sandboxing" below (ensure_kontur_images/
# ensure_kontur_kvm_access) and terraform/gcp/README.md's
# own section of the same name. GRAIN_KONTUR_ENABLE=1 (off by default here
# -- terraform/gcp's own enable_kontur_sandboxes variable is what
# actually turns this on for that deployment shape) needs no manual
# build-and-publish step first (bwsalmon/agents#531), and needs nothing
# configured for its sandbox container either (bwsalmon/agents#645).
#
# A kontur deployment runs one artifact and builds none. The sandbox
# image is always pulled: GRAIN_KONTUR_OCI_IMAGE overrides which one, and
# left empty (the default) it is whatever `grain sandbox-image` reports --
# the reference stamped into the grain image this host runs at the time it
# was built, so the two are always from one commit. The guest boots from
# inside that image; there is no separate disk to fetch or build.
GRAIN_KONTUR_ENABLE="${GRAIN_KONTUR_ENABLE:-0}"
# Remembers what was actually asked for, since ensure_kontur_images/
# ensure_kontur_kvm_access overwrite GRAIN_KONTUR_ENABLE
# itself back to 0 on a failure partway through -- report_readiness uses
# this to tell "kontur was never requested" apart from "kontur was
# requested but a prerequisite wasn't ready this run".
GRAIN_KONTUR_REQUESTED="$GRAIN_KONTUR_ENABLE"
# Empty resolves to the sandbox container this build of grain was built
# against (ensure_kontur_oci_image, and cmd/grain/sandboximage.go for the
# stamp itself). Set it to run a different one -- a mirror, a private
# copy, or a sandbox image pinned apart from grain's own.
GRAIN_KONTUR_OCI_IMAGE="${GRAIN_KONTUR_OCI_IMAGE:-}"
# The guest account the daemon execs as, and the account konturctl's
# -guest-user tells kontur to authorize this boot's generated key for.
# There is no key here to configure: kontur generates one per VM boot and
# passes the public half on the kernel command line.
GRAIN_KONTUR_SSH_USER="${GRAIN_KONTUR_SSH_USER:-debian}"
GRAIN_KONTUR_WORKSPACE="${GRAIN_KONTUR_WORKSPACE:-/home/debian}"
# flat: the guest is spliced onto its sandbox container's own segment and
# takes over the address docker assigned it, so nothing here has to assign
# one. nat is kontur's original mode, where each VM needs its own address
# on a shared private bridge and its own forwarded port -- which is all
# GRAIN_KONTUR_BASE_IP/GRAIN_KONTUR_BASE_PORT below exist to derive, and
# which flat mode ignores. Flat mode needs a guest image carrying kontur's
# own guest overlays, which every kontur image carries.
GRAIN_KONTUR_NET="${GRAIN_KONTUR_NET:-flat}"
GRAIN_KONTUR_BASE_IP="${GRAIN_KONTUR_BASE_IP:-169.254.100.10}"
GRAIN_KONTUR_BASE_PORT="${GRAIN_KONTUR_BASE_PORT:-12000}"
# The nameserver every sandbox guest resolves through, comma separated
# for a second one. Left empty, konturctl picks its own default -- a
# public resolver, which is the only answer that is right on an arbitrary
# host: the guest cannot reach this host's own resolver (routinely an
# address that exists only in this host's network namespace) or docker's
# embedded one on 127.0.0.11 (the *namespace's* loopback, not on the
# wire), so a guest pointed at either has open IP egress and hangs on
# every name it looks up.
#
# Set it on a deployment whose network has a resolver of its own, or one
# where reaching a public resolver is not allowed. It reaches the guest
# on its ip= kernel parameter, per boot, so a change here takes effect on
# the next sandbox rather than needing a new guest image.
GRAIN_KONTUR_DNS="${GRAIN_KONTUR_DNS:-}"
GRAIN_KONTUR_GIT_PROXY_HOST="${GRAIN_KONTUR_GIT_PROXY_HOST:-}"

# On by default -- the common case is a single, directly-managed host
# with no rollout mechanism of its own, where the UI's Upgrade button
# (bwsalmon/agents#396) is the only way to ship a new build. Set to 0 by
# terraform/gcp/files/deploy.sh's own invocation of this script: that
# deployment shape already has a separate, Terraform-driven rollout
# (config-sync.sh watching the grain-deploy-generation instance-metadata
# attribute), and leaving both live at once let an operator's UI click
# race, or silently drift out of sync with, a `terraform apply`
# (bwsalmon/agents#405).
GRAIN_ENABLE_UI_UPGRADE="${GRAIN_ENABLE_UI_UPGRADE:-1}"

usage() {
  cat <<'EOF'
Usage: sudo ./setup.sh

Installs or updates a v2 grain deployment on this machine: pulls the
deployment image CI publishes on every commit, lays out /var/lib/grain
(including its embedded SQLite task store), and installs
grain-daemon.service, which runs that image -- the dispatch loop, the
UI/API it serves, and every binary either shells out to. Every setting
is an environment variable; all have defaults, so a bare
`sudo ./setup.sh` re-run is the update path.
Anything marked "Seeded once" below only takes effect the first time this
deployment's store gets a config row (typically this script's very first
run); a later run passing a different value has no effect on an existing
deployment -- grain-daemon.service logs a line saying so on every start it
happens. Change an already-seeded value with `grain settings` (run as
GRAIN_USER) or the UI's Settings pane instead.
Recognized variables:

  GRAIN_REF                branch to deploy (default: main) -- names which
                             published image runs, via GRAIN_IMAGE_TAG below
  GRAIN_DATA_DIR            secrets/store root -- state that must survive a redeploy
                             (default: /var/lib/grain)
  GRAIN_SANDBOX_DIR         HostSandboxes' per-slot working directory root, only used
                             without kontur sandboxing -- state that a redeploy is free
                             to discard (default: /var/lib/grain-sandbox)
  GRAIN_USER                unprivileged account grain runs as (default: grain)

  GRAIN_UI_ADDR             UI/API bind address (default: 127.0.0.1:80 -- loopback
                             only; reach it with `ssh -L 8080:localhost:80 host`,
                             or put it behind Tailscale/IAP instead)
  GRAIN_MAX_WORKERS         maximum number of ordinary tasks dispatched at once
                             (default: 1; GRAIN_MAX_CONCURRENT is its former name and
                             is still honoured). Seeded once, like every setting below
                             marked the same way -- see this file's own header comment
  GRAIN_MAX_MERGERS         agents on top of GRAIN_MAX_WORKERS that only the merge
                             queue may dispatch, to repair a pull request that will
                             not land (default: 1; 0 makes them wait for a worker slot
                             like anything else). Seeded once
  GRAIN_POLL_INTERVAL       daemon reconcile-cycle interval (default: 30s). Seeded once

  GRAIN_GITHUB_HOST         GitHub API host (default: github.com). Seeded once
  GRAIN_GITHUB_INSECURE_HTTP  1 to speak plain HTTP to it (mock servers only). Seeded once
  GRAIN_GITHUB_TOKEN        a token to seed the credential ladder with, once
                             (only written if no credential is configured yet)
  GRAIN_GITHUB_CREDENTIAL_NAME  name to store that token under (default: bot)
  GRAIN_GITHUB_APP_ID       a GitHub App's own ID, together with
  GRAIN_GITHUB_APP_INSTALLATION_ID  its installation ID on test_repos, and
  GRAIN_GITHUB_APP_PRIVATE_KEY  its downloaded PEM private key -- seed all
                             three, once, to store an App credential under
                             GRAIN_GITHUB_CREDENTIAL_NAME instead of a bare
                             token: pkg/gitproxy.CredentialSet mints and
                             refreshes an installation token from it, which
                             (unlike a fine-grained PAT) can read the Checks
                             API -- see terraform/gcp/README.md, "There is
                             no Checks permission to grant". Ignored if
                             GRAIN_GITHUB_CREDENTIAL_NAME already has a
                             credential of either kind on disk

  GRAIN_GEMINI_API_KEY      Gemini API key to seed, once. Not required:
                             grain-daemon.service starts either way, and a
                             deployment with no key set anywhere serves the
                             UI so one can be pasted in there (Settings ->
                             Agent frameworks) -- only a *dispatch* needs
                             one, and a run whose framework has none fails
                             as setup-failed saying so. A deployment with
                             GRAIN_GCP_PROJECT set and a minter credential
                             available (seeded from
                             GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE below) mints
                             its own here instead -- see
                             mint_gemini_operating_key
  GRAIN_CLAUDE_CODE_OAUTH_TOKEN  Claude Code OAuth token to seed, once --
                             GRAIN_GEMINI_API_KEY's counterpart for the
                             "claude" agent framework, and equally optional
                             for the same reasons. Which framework a run is
                             actually driven by is a store-backed setting
                             (the UI's own Agent framework choice, seeded by
                             -agent-framework), overridable per task, so a
                             deployment that might use either wants both
                             credentials
  GRAIN_OPENAI_API_KEY      OpenAI API key to seed, once -- the same
                             counterpart again, for the "codex" agent
                             framework, and optional for the same reasons
  GRAIN_IMAGE               image repository the deployment runs, with no tag
                             (default ghcr.io/bwsalmon/grain/grain -- what CI
                             publishes on every commit)
  GRAIN_IMAGE_TAG           which tag of it to run (default: the tag published
                             for GRAIN_REF, i.e. that branch with "/" replaced
                             by "-"). Set to sha-<short sha> to pin, or to roll
                             back to, one exact build
  GRAIN_IMAGE_PULL_USER     credentials for `docker login` against that
  GRAIN_IMAGE_PULL_TOKEN     registry -- only needed for a private package;
                             bwsalmon/grain's is public and pulls anonymously
  GRAIN_EXTRA_DOCKER_ARGS   extra arguments for grain-daemon.service's own
                             `docker run`, word-split as written (e.g. another
                             --volume the deployment needs)

  GRAIN_CLAUDE_PATH         path on THIS HOST to a claude CLI to run instead
                             of the one the deployment image already carries
                             (default: empty, use the image's). What it names
                             is bind-mounted into the container at the same
                             path
  GRAIN_AGY_PATH            path on THIS HOST to an Antigravity CLI (agy) to run
                             instead of the image's own, for the second agent
                             framework. Bind-mounted the same way (default:
                             empty, use the image's)
  GRAIN_CODEX_PATH          path on THIS HOST to a codex CLI to run instead of
                             the image's own, for the third. Bind-mounted the
                             same way (default: empty, use the image's)
  GRAIN_GEMINI_MODEL        override the daemon's default Gemini model. Seeded once
  GRAIN_GEMINI_EFFORT       override the reasoning effort asked for beside it
                             (low|medium|high). Seeded once; ignored for a model
                             name that already carries one
  GRAIN_CLAUDE_MODEL        override the daemon's default Claude model. Seeded once,
                             the exact counterpart of GRAIN_GEMINI_MODEL above
  GRAIN_CODEX_MODEL         override the daemon's default Codex model. Seeded
                             once, the same counterpart again
  GRAIN_MAX_AGENT_TURNS     cap on model/tool round trips per run. Empty leaves
                             the framework's own default (20), which a real task
                             can exhaust: reading a few files, writing one, running
                             a test and then add/commit/push are each a turn, and
                             the run fails outright rather than finishing short.
                             Seeded once

  GRAIN_GCP_PROJECT                  enables the gcp-key/gemini-key capabilities.
                                      Seeded once
  GRAIN_GCP_SERVICE_ACCOUNT_EMAIL    the narrow agent service account they mint
                                      for. Seeded once
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE a minter key to seed under the gcp-key-minter
                                      secret. Written on *every* run, unlike the
                                      seeded-once values above: the deploy that
                                      supplies this key also rotates it in GCP, so a
                                      host that kept its first copy ends up
                                      authenticating with a key GCP has deleted

  GRAIN_STATE_REPO_URL      git URL of the repository this deployment's database
                             lives in (pkg/staterepo), written to
                             <data-dir>/state-repo.json so the daemon comes up
                             pointed at it instead of waiting for someone to
                             open the UI's bootstrap pane. Empty (the default)
                             leaves whatever this host already decided --
                             including the local-only repository a fresh install
                             gets -- exactly as it is. Pointing an existing
                             deployment at a *different* repository moves its
                             working tree aside first, timestamped, the same way
                             `grain state adopt` does
  GRAIN_STATE_REPO_BRANCH   branch state lives on there (default: main)
  GRAIN_SECRETS_KEY         this deployment's secrets private key
                             (`grain state key path` names the file it is
                             written to). Seeded once, like the credentials
                             above -- and the one value here a redeploy really
                             must carry: nothing else can stand in for it, and a
                             host restored onto a fresh data directory that mints
                             itself a key cannot read the secrets file put back
                             beside it. Back the file up when this script reports
                             it (see "Readiness" at the end of a run)

  GRAIN_TARGET_REPO         owner/name: the UI's default target for a task with
                             no repo of its own, and the repo this script pushes
                             one empty commit to if it has no commits yet
  GRAIN_TARGET_BRANCH       branch to create there if formatting it (default: main)
  GRAIN_TARGET_REPOS        comma-separated owner/name list a task's repo may
                             name (default: empty, meaning unrestricted) -- the
                             daemon's own -target-repos, the allowlist a task
                             naming anything else is parked with a comment
                             rather than dispatched against. Seeded once

  GRAIN_ENABLE_UI_UPGRADE   1 (default) to wire up the UI's own Upgrade
                             button (bwsalmon/agents#396); set to 0 on a
                             deployment shape that already has its own
                             rollout mechanism (e.g. terraform/gcp's
                             config-sync.sh/deploy.sh), so the two cannot
                             race or drift out of sync with each other

  GRAIN_KONTUR_ENABLE        1 to dispatch onto real bwsalmon/kontur-managed
                             VMs over SSH (orchestrator.KonturSandboxes)
                             instead of host directories (default: 0). Pulls
                             the sandbox image this build of grain expects and
                             builds nothing (ensure_kontur_images, below).
                             Needs /dev/kvm on this host (nested
                             virtualization). Left off (with a
                             logged reason) if any prerequisite below is
                             missing, rather than failing the whole run.
  GRAIN_KONTUR_OCI_IMAGE     the sandbox image each task's VM runs -- both the
                             container and the guest inside it.
                             Empty (the default) is the one stamped into this
                             grain build at build time -- `grain sandbox-image`
                             -- so grain and its sandbox always come from one
                             commit and nothing has to be configured. Set it to
                             run a different one; it is pulled either way, and
                             never built here.
  GRAIN_KONTUR_SSH_USER      username to SSH into each kontur VM as, and the
                             account konturctl's -guest-user has kontur
                             authorize this boot's generated key for
                             (default: debian). There is no key to
                             configure: kontur generates one per VM boot.
  GRAIN_KONTUR_WORKSPACE     working directory tools operate in on each kontur
                             VM (default: /home/debian, GRAIN_KONTUR_SSH_USER's own home)
  GRAIN_KONTUR_NET           kontur networking mode: "flat" (default) or "nat".
                             Flat needs a guest carrying kontur's own guest
                             overlays, which every kontur image has; see
                             scripts/kontur/README.md.
  GRAIN_KONTUR_BASE_IP       "-ip" slot 1's kontur VM gets; every later slot's
                             (nat mode only -- ignored under flat)
                             is the next address after it (default: 169.254.100.10)
  GRAIN_KONTUR_BASE_PORT     "-port" slot 1's kontur VM forwards; every later
                             slot's is this plus its own number minus one
                             (default: 12000)
  GRAIN_KONTUR_DNS           nameserver each sandbox guest resolves through,
                             comma separated for a second one. Empty (the
                             default) leaves konturctl's own default, a
                             public resolver -- neither this host's own
                             resolver nor docker's embedded one is
                             reachable from inside a guest. Set it on a
                             network with its own resolver, or one where a
                             public resolver is not reachable; it applies
                             per boot, so no new guest image is needed.
  GRAIN_KONTUR_GIT_PROXY_HOST  host (no port) a kontur VM reaches this
                             daemon's own git proxy through, in place of the
                             loopback address it otherwise binds to -- a
                             kontur VM's guest has its own unrelated
                             127.0.0.1, with no route to this host's
                             (bwsalmon/agents#567). Defaults to docker's own
                             "bridge" network gateway address, detected via
                             `docker network inspect bridge` or, failing
                             that, the default route a container on that
                             network has; set
                             explicitly if this host's kontur VM containers
                             join a different docker network.
EOF
}

for arg in "$@"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    *) echo "setup.sh: unrecognized argument: $arg (see --help)" >&2; exit 2 ;;
  esac
done

log() { echo "==> $*"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "setup.sh: must run as root (it creates a system user, systemd units, and /usr/local/bin/grain) -- try sudo" >&2
  exit 1
fi

# The base-system commands every step below assumes: systemd's own
# systemctl, and two that any distribution's base install carries.
# Checked rather than installed -- a host missing one of these is not a
# host this script can repair -- and deliberately a short list: see this
# file's own header, "What this host has to have".
for cmd in systemctl install useradd; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "setup.sh: required command not found: $cmd" >&2
    exit 1
  fi
done

# docker is installed rather than only reported missing
# (bwsalmon/agents#617): it is the one package this script needs that a
# vanilla Debian cloud image does not carry, and until #617 it was only
# ever guaranteed by terraform/gcp/files/deploy.sh's own
# install_prerequisites -- which runs *before* this script but is no
# help to anyone reaching it the way this file's header says it should
# be reachable: put setup.sh on a bare VM and run it. git and jq had
# helpers of exactly this shape here too, and are gone -- nothing on
# this host needs either any more (see the header, "What this host has
# to have").
#
# The install is one attempt to make the `docker info` check a few lines
# down pass on its own rather than hand the operator a cryptic failure
# for a one-line fix; that check, not this, is what gates the rest of
# the script. It also enables the daemon, since a fresh install's
# postinst usually starts it but this does not rely on that.
#
# docker is what grain *runs in* now, not merely what it was once built
# in (bwsalmon/agents#645): grain-daemon.service is a `docker run` of the
# deployment image, so this is a hard runtime dependency of the service
# and not just of this script.
ensure_docker() {
  if ! command -v docker >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
    log "installing docker (grain-daemon.service runs the deployment image with it; no Debian cloud image carries it)"
    apt-get update -qq || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends docker.io || true
  fi
  if ! command -v docker >/dev/null 2>&1; then
    echo "setup.sh: required command not found: docker, and it could not be installed automatically -- install it (e.g. 'apt-get install docker.io') and re-run" >&2
    exit 1
  fi
  # enable, not just start: the daemon has to come back on reboot, or
  # grain-daemon.service comes up to no container engine at all.
  systemctl enable --now docker >/dev/null 2>&1 || true
  # Resolved once, here, rather than written as a literal path: the unit
  # written below is read by systemd, which resolves no $PATH of its own,
  # and `docker` is /usr/bin/docker on Debian's own package but not
  # everywhere.
  DOCKER_BIN="$(command -v docker)"
}
ensure_docker

if ! docker info >/dev/null 2>&1; then
  echo "setup.sh: 'docker info' failed -- is the Docker daemon running? grain-daemon.service runs the deployment image with it, so this is a runtime dependency, not only a deploy-time one" >&2
  exit 1
fi

# --- 1/2. pull the deployment image, and install the CLI wrappers -------
#
# This is where a v2 deploy used to spend its minutes: `make -C v2
# container-build`, a pinned Go toolchain image, a cold module cache, a
# `npm ci` for the frontend, and a binary at the end of it. None of that
# happens on a deployed host any more (bwsalmon/agents#645) -- CI built
# and published this exact image when the commit landed, so a deploy is a
# `docker pull` and a service restart.
#
# GRAIN_IMAGE_REF is the one name everything below agrees on; the ref
# *file* is the indirection the service actually reads, so that the UI's
# Upgrade button can repoint a deployment by writing one line rather than
# by rewriting a systemd unit (see write_image_ref and
# pkg/upgrade/image.go).
GRAIN_IMAGE_REF="${GRAIN_IMAGE}:${GRAIN_IMAGE_TAG}"
IMAGE_REF_FILE="$GRAIN_DATA_DIR/image.env"

# image_run runs one command inside the deployment image: docker
# arguments before "--", the command line for the image after it --
#
#   image_run --network host --entrypoint curl -- -fsS "$url"
#
# This is how every step below that needs more than a shell gets it, and
# it is why this host needs nothing but docker (see this file's header,
# "What this host has to have"). The image already carries git, curl and
# grain's own source, and this host is already pulling and running it --
# so borrowing a tool out of it costs a deployment nothing, while
# requiring the same tool out here would add a package every host must
# have before it can deploy at all.
#
# --entrypoint is not optional in any call: this image's own entrypoint
# is `grain` (Dockerfile), so a caller that forgets it runs a grain
# subcommand instead of the command it named.
#
# Deliberately no --network host by default: only the callers that must
# reach an address on this host itself -- the metadata server, a
# GRAIN_GITHUB_HOST that is a mock on loopback -- ask for it.
image_run() {
  local docker_args=()
  while [ "$#" -gt 0 ]; do
    [ "$1" = "--" ] && { shift; break; }
    docker_args+=("$1")
    shift
  done
  docker run --rm "${docker_args[@]}" "$GRAIN_IMAGE_REF" "$@"
}

# registry_login authenticates to the image's registry, if this
# deployment was given a credential for it. Nothing to do in the ordinary
# case: bwsalmon/grain's GHCR package is public, and an anonymous pull
# works. Never fatal on its own -- a failed login is reported here and
# then simply becomes whatever the pull below reports.
registry_login() {
  [ -n "$GRAIN_IMAGE_PULL_TOKEN" ] || return 0
  local registry="${GRAIN_IMAGE%%/*}"
  log "Logging in to $registry as ${GRAIN_IMAGE_PULL_USER:-<token>}"
  if ! printf '%s' "$GRAIN_IMAGE_PULL_TOKEN" \
     | docker login "$registry" -u "${GRAIN_IMAGE_PULL_USER:-x-access-token}" --password-stdin >/dev/null 2>&1; then
    log "  docker login against $registry failed -- the pull below will say whether it mattered"
  fi
}

# pull_image fetches the build this deploy is meant to run.
#
# A pull that fails is fatal *unless* this host already has that exact
# ref in its local image store: a re-run over a transient network failure
# (or a registry outage) then converges on the image it was already asked
# for rather than refusing to deploy at all -- the same "converge with
# what is ready" trade the kontur steps below make. It is not a fallback
# to some *other* image: an unknown tag with nothing local still stops
# here, loudly, instead of leaving grain-daemon.service pointed at
# something that does not exist.
pull_image() {
  registry_login
  log "Pulling $GRAIN_IMAGE_REF"
  if docker pull "$GRAIN_IMAGE_REF"; then
    return 0
  fi
  if docker image inspect "$GRAIN_IMAGE_REF" >/dev/null 2>&1; then
    log "  pull failed, but this host already has $GRAIN_IMAGE_REF locally -- deploying that"
    return 0
  fi
  echo "setup.sh: could not pull $GRAIN_IMAGE_REF and this host has no local copy of it." >&2
  echo "  Check that the tag exists (CI publishes one per branch and one per commit --" >&2
  echo "  see .github/workflows/build-artifacts.yml), or set GRAIN_IMAGE_TAG to one that" >&2
  echo "  does. A private package also needs GRAIN_IMAGE_PULL_USER/GRAIN_IMAGE_PULL_TOKEN." >&2
  exit 1
}

# install_cli_wrappers puts `grain` and `konturctl` on this host's PATH
# without installing either binary on it: each is a one-line script over
# a shared runner that runs the deployment image, so an operator's
# `grain list` and
# scripts/kontur-diag.sh's `konturctl vm list` both reach exactly the
# build grain-daemon.service is running, and neither can drift from it.
# This script uses the `grain` one itself, for `grain schema-version`
# (reformat_store_if_schema_changed) and `grain secrets`
# (seed_gcp_minter_key, mint_gemini_operating_key, report_readiness).
#
# The wrapper decides its own mounts at invocation time rather than
# baking in what was true when this ran: it is written early (the rest of
# this script needs it) and the kontur decisions below are not final
# until much later, so an existence guard per optional path is what keeps
# one file correct in both cases -- and keeps it correct after a later
# re-run changes them.
#
# It runs the image as $GRAIN_USER, exactly as the service does, so a
# store or secrets database a CLI invocation creates comes out owned by
# the account the daemon reads it with rather than by root.
install_cli_wrappers() {
  install -d -m0755 /usr/local/lib/grain
  # The port the daemon serves on, baked into the wrapper as the CLI's
  # default -server. /etc/profile.d/grain.sh (write_cli_profile, below)
  # exports the same value for an operator's shell, but that export
  # cannot reach a process inside a container -- so without this a
  # `grain list` would fall back to cmd/grain/main.go's own
  # http://127.0.0.1:8420, which is only ever right by coincidence. A
  # GRAIN_SERVER already set in the caller's environment still wins.
  local port="${GRAIN_UI_ADDR##*:}"
  # GRAIN_DATA_DIR goes in the same way, for the two subcommands that
  # read this deployment's files rather than its API: `grain state` and
  # `grain secrets` take a -data-dir and default it to this variable, so
  # without it the `grain state status` this script's own closing report
  # tells the operator to run fails with "-data-dir is required"
  # (grain/task-303). Baked rather than let through from the caller like
  # GRAIN_SERVER is: the only data directory that exists inside this
  # container is the one mounted just below, so any other value names a
  # path that is not there.

  # Removed, not overwritten: on a host deployed before
  # bwsalmon/agents#645 both of these are *symlinks* into
  # $GRAIN_DATA_DIR/bin, and `cat >` follows a symlink -- which would
  # write this wrapper over the binary at the far end and leave
  # /usr/local/bin/grain still pointing at it, so `grain` would run a
  # shell script the kernel was asked to exec as a binary.
  rm -f /usr/local/bin/grain /usr/local/bin/konturctl
  local uid gid
  uid="$(id -u "$GRAIN_USER")"
  gid="$(id -g "$GRAIN_USER")"

  cat > /usr/local/lib/grain/run-image.sh <<WRAPPER
#!/usr/bin/env bash
# Written by scripts/setup.sh (install_cli_wrappers). Runs one command
# in the deployment image, with the same identity and the same view of
# this host's directories that grain-daemon.service's own container has.
# Called by /usr/local/bin/grain and /usr/local/bin/konturctl, which
# differ only in the entrypoint they ask for.
#
# Arguments before "--" are passed to docker run; those after it are the
# command line for the image itself, which this appends the image ref in
# front of.
set -euo pipefail

extra=()
while [ "\$#" -gt 0 ]; do
  [ "\$1" = "--" ] && { shift; break; }
  extra+=("\$1")
  shift
done

# The image the service is running right now, not the one this wrapper
# was written for: an upgrade through the UI rewrites that file (its
# GRAIN_IMAGE line is what grain-daemon.service reads too), and a CLI
# that kept answering out of the old image would be reporting on a
# deployment that no longer exists.
image="${GRAIN_IMAGE_REF}"
if [ -s "${IMAGE_REF_FILE}" ]; then
  . "${IMAGE_REF_FILE}"
  image="\${GRAIN_IMAGE:-\$image}"
fi

args=(
  --rm --interactive
  --network host
  --user ${uid}:${gid}
  --env HOME=${GRAIN_DATA_DIR}/home
  --env "GRAIN_SERVER=\${GRAIN_SERVER:-http://127.0.0.1:${port}}"
  --env GRAIN_DATA_DIR=${GRAIN_DATA_DIR}
  --volume ${GRAIN_DATA_DIR}:${GRAIN_DATA_DIR}
)
[ -d "${GRAIN_SANDBOX_DIR}" ] && args+=(--volume ${GRAIN_SANDBOX_DIR}:${GRAIN_SANDBOX_DIR})
# konturctl talks to this host's docker daemon and keeps its VM records
# out here; the image paths it hands docker are host paths, so each has
# to be mounted at the very path it already has.
if [ -S /var/run/docker.sock ]; then
  args+=(--volume /var/run/docker.sock:/var/run/docker.sock)
  docker_gid="\$(getent group docker | cut -d: -f3)"
  [ -n "\$docker_gid" ] && args+=(--group-add "\$docker_gid")
fi
[ -d /var/lib/kontur ] && args+=(--volume /var/lib/kontur:/var/lib/kontur)

exec docker run "\${args[@]}" "\${extra[@]}" "\$image" "\$@"
WRAPPER
  chmod 0755 /usr/local/lib/grain/run-image.sh

  cat > /usr/local/bin/grain <<'CLI'
#!/usr/bin/env bash
# Written by scripts/setup.sh: the grain CLI, out of the deployment
# image. `grain <args>` reaches the image's own entrypoint.
exec /usr/local/lib/grain/run-image.sh -- "$@"
CLI
  chmod 0755 /usr/local/bin/grain

  cat > /usr/local/bin/konturctl <<'CLI'
#!/usr/bin/env bash
# Written by scripts/setup.sh: konturctl, out of the deployment image
# -- the same copy pkg/kontur runs inside grain-daemon.service's own
# container, so a diagnostic run out here (scripts/kontur-diag.sh)
# and the daemon's own calls cannot be different builds.
exec /usr/local/lib/grain/run-image.sh --entrypoint konturctl -- "$@"
CLI
  chmod 0755 /usr/local/bin/konturctl

  log "Installed /usr/local/bin/grain and /usr/local/bin/konturctl (both run $GRAIN_IMAGE_REF)"
  write_cli_profile
}

# write_image_ref records which image grain-daemon.service should run, in
# the file the unit reads as an EnvironmentFile. Rewritten on every run of
# this script, so re-running with a different GRAIN_IMAGE_TAG (or a
# different GRAIN_REF) is what pins or rolls back a deployment -- and
# deliberately the same file pkg/upgrade's image path writes, so the
# UI's Upgrade button and this script are two ways of doing one thing
# rather than two mechanisms that can disagree.
# Runs after setup_data_dir, which is what creates the directory this
# writes into -- deliberately not creating it here, since an `install -d`
# would also re-apply a mode, and $GRAIN_DATA_DIR's is 0750 for reasons
# that have nothing to do with this file.
write_image_ref() {
  printf 'GRAIN_IMAGE=%s\n' "$GRAIN_IMAGE_REF" > "$IMAGE_REF_FILE"
  chmod 0644 "$IMAGE_REF_FILE"
  chown "$GRAIN_USER:$GRAIN_USER" "$IMAGE_REF_FILE"
}

# write_cli_profile points this host's `grain` CLI at this host's own
# daemon, and at that daemon's own data directory.
#
# The CLI talks to the daemon over REST and defaults to
# http://127.0.0.1:8420 (cmd/grain/main.go's defaultServerURL), which is
# only ever right by coincidence -- a deployment setting GRAIN_UI_ADDR to
# anything else made every invocation on its own host need an explicit
# -server, forever, with a connection refused as the only hint.
#
# The bind address is not reusable as-is: it is what the daemon listens
# on, so a deployment behind a load balancer or a tunnel binds 0.0.0.0,
# which is not an address to connect *to*. Only the port carries over;
# the host is always loopback, since this file is for shells on the same
# machine.
#
# GRAIN_DATA_DIR is the same favour for `grain state` and `grain secrets`,
# which read this deployment's files rather than its API and default
# their -data-dir to it (grain/task-303). It is exported even when the
# port cannot be worked out: the two are unrelated, and a shell that
# cannot be told where the daemon listens can still be told where its
# state lives.
write_cli_profile() {
  local port="${GRAIN_UI_ADDR##*:}"
  local server=""
  if [ -z "$port" ] || [ "$port" = "$GRAIN_UI_ADDR" ]; then
    log "  GRAIN_UI_ADDR ($GRAIN_UI_ADDR) has no port; /etc/profile.d/grain.sh will not set GRAIN_SERVER"
  else
    server="export GRAIN_SERVER=\"http://127.0.0.1:${port}\""
  fi
  cat > /etc/profile.d/grain.sh <<PROFILE
# Written by scripts/setup.sh. Points the grain CLI at the daemon this
# host runs, whose port comes from the -ui-addr it was started with, and
# at the data directory that daemon was started with. An explicit
# -server or -data-dir flag still overrides either.
${server}
export GRAIN_DATA_DIR="${GRAIN_DATA_DIR}"
PROFILE
  chmod 0644 /etc/profile.d/grain.sh
  if [ -n "$server" ]; then
    log "  grain CLI on this host defaults to http://127.0.0.1:${port} (/etc/profile.d/grain.sh)"
  fi
  log "  \`grain state\` and \`grain secrets\` on this host default to ${GRAIN_DATA_DIR} (/etc/profile.d/grain.sh)"
}

# --- 3. the unprivileged account grain runs as --------------------------

# ensure_user creates $GRAIN_USER.
#
# This user used to also be added to the systemd-journal group here, back
# when the daemon ran directly on the host as $GRAIN_USER and journalctl
# checked the invoking process's own supplementary groups. Now that
# grain-daemon.service's ExecStart is a `docker run --user uid:gid`
# (bwsalmon/agents#645), the process journalctl runs as inside the
# container never sees this host account's group memberships at all --
# `--user` there is two bare numbers, not a login, and Docker does not
# consult this host's /etc/group for it. Granting the container
# permission to read the journal files docker_run_args bind-mounts in is
# instead done the same way docker socket access already is
# (grant_docker_group's own comment): a --group-add of the group's numeric
# GID on the `docker run` itself (docker_run_args, below).
ensure_user() {
  if ! id -u "$GRAIN_USER" >/dev/null 2>&1; then
    log "Creating system user $GRAIN_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$GRAIN_USER"
  fi
}

# --- 4. the control channel: acting on the host from inside the container -
#
# Two UI buttons ask grain to do something to the machine it runs on:
# "reboot host" (pkg/ui/host.go, bwsalmon/agents#395) and the restart
# at the end of an Upgrade (bwsalmon/agents#396). Both used to be a
# NOPASSWD sudoers line each, granting $GRAIN_USER exactly one command --
# and neither can work from inside a container, where `systemctl` reaches
# no systemd that matters and sudo grants nothing that crosses the
# boundary.
#
# So the container asks instead of acting. The daemon touches a file
# under $GRAIN_DATA_DIR/control -- a directory it already has mounted --
# and a systemd .path unit out here notices and runs the real command as
# root. write_systemd_units points -reboot-cmd and -upgrade-restart-cmd
# at exactly those two files.
#
# What this grants is the same pair of actions the two sudoers files
# granted, by a mechanism with less reach: there is no sudo rule left at
# all, so nothing can be widened by editing one, and the only thing the
# daemon can cause is whichever command these two units name. Anything
# able to write into $GRAIN_DATA_DIR/control can trigger them -- which is
# $GRAIN_USER and root, exactly who could invoke the old sudo rules.
#
# Each service removes the request file before acting, so a request is
# consumed once. PathModified (rather than PathExists) is what watches
# them: a leftover file must not turn into a reboot the next time this
# host boots.
CONTROL_DIR="$GRAIN_DATA_DIR/control"

write_control_units() {
  log "Writing grain-reboot.path/.service and grain-restart.path/.service"
  install -d -m0770 -o "$GRAIN_USER" -g "$GRAIN_USER" "$CONTROL_DIR"
  # A host deployed before bwsalmon/agents#645 still carries the two
  # sudoers drop-ins these units replace. Nothing invokes them any more,
  # but a standing NOPASSWD grant that no longer has a reason to exist is
  # exactly the kind of thing nobody notices again -- so a re-run removes
  # them rather than leaving them behind.
  rm -f /etc/sudoers.d/grain-daemon-reboot /etc/sudoers.d/grain-daemon-upgrade

  cat > /etc/systemd/system/grain-reboot.path <<UNIT
[Unit]
Description=Watch for grain's reboot-host request

[Path]
PathModified=${CONTROL_DIR}/reboot
Unit=grain-reboot.service

[Install]
WantedBy=multi-user.target
UNIT

  cat > /etc/systemd/system/grain-reboot.service <<UNIT
[Unit]
Description=Reboot this host, at the grain daemon's request

[Service]
Type=oneshot
ExecStart=/bin/rm -f ${CONTROL_DIR}/reboot
ExecStart=/usr/bin/systemctl reboot
UNIT

  cat > /etc/systemd/system/grain-restart.path <<UNIT
[Unit]
Description=Watch for grain's restart request (the UI's Upgrade button)

[Path]
PathModified=${CONTROL_DIR}/restart
Unit=grain-restart.service

[Install]
WantedBy=multi-user.target
UNIT

  cat > /etc/systemd/system/grain-restart.service <<UNIT
[Unit]
Description=Restart grain-daemon.service, at the grain daemon's request

[Service]
Type=oneshot
ExecStart=/bin/rm -f ${CONTROL_DIR}/restart
ExecStart=/usr/bin/systemctl restart grain-daemon.service
UNIT
}

# grant_docker_group adds $GRAIN_USER to the docker group if it exists.
#
# Nothing in the *container* needs this: grain-daemon.service's own
# `docker run` is executed by systemd as root, and the socket the
# container itself gets is reached through a --group-add of the docker
# gid (docker_run_args). This is for the account an operator uses on this
# host -- `grain`/`konturctl` are wrappers around `docker run` now
# (install_cli_wrappers), so a shell as $GRAIN_USER needs to be able to
# talk to the daemon for either of them to work at all.
grant_docker_group() {
  if getent group docker >/dev/null 2>&1; then
    usermod -aG docker "$GRAIN_USER"
  fi
}

# --- kontur sandboxing ---------------------------------------------------
#
# Everything below is skipped, with a logged reason, unless
# GRAIN_KONTUR_ENABLE=1 -- turning it off after a prior successful run
# leaves whatever it already did in place (a fetched guest image, a
# pulled OCI image, group memberships); it just stops write_systemd_units
# from wiring up the -kontur-* flags that would use them. A failure in
# any step below also flips GRAIN_KONTUR_ENABLE back to 0 for the rest of
# this run, the same "converge with what's ready, don't fail the whole
# install" choice mint_gemini_operating_key already makes for a missing
# Gemini key: a deployment whose kontur image is not ready yet still gets
# a working host-directory-backed daemon out of this run rather than
# nothing at all.

# kontur_gcp_access_token fetches a short-lived OAuth2 access token for
# this host's own attached service account from the metadata server --
# enough to authenticate docker to an Artifact Registry the sandbox image
# might live in, without installing the whole gcloud SDK just for that.
# iam.tf's own host_reads_kontur_registry is what makes the token itself
# actually able to. Not needed at all for the default, GHCR-hosted image
# -- pull_image's own registry_login has already covered that host.
#
# curl and the JSON parse were both host tools until recently -- curl,
# and a jq this script installed for these two lines and nothing else.
# curl runs inside the deployment image now (image_run), and the one
# field wanted out of the response is read with bash's own regex match.
# That match is also what keeps a response *without* the field from
# travelling on as the literal string "null" in an Authorization header:
# no match is a non-zero return, exactly as `jq -er '.access_token //
# empty'` was.
#
# The metadata server is addressed by IP rather than as
# metadata.google.internal: that name resolves on a GCE host through an
# /etc/hosts entry, and a container gets docker's own /etc/hosts even
# under --network host, so the name is not reliably there. The address
# is the documented one that entry points at.
kontur_gcp_access_token() {
  local json
  json="$(image_run --network host --entrypoint curl -- \
    -fsS -H "Metadata-Flavor: Google" \
    "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token" \
    2>/dev/null)" || return 1
  [[ "$json" =~ \"access_token\"[[:space:]]*:[[:space:]]*\"([^\"]+)\" ]] || return 1
  printf '%s\n' "${BASH_REMATCH[1]}"
}

ensure_kontur_images() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi

  # One artifact, and nothing built here.
  #
  # This used to be two decisions. The sandbox *container* was pulled,
  # and the guest *disk* was built on every host -- not an oversight but
  # a consequence: guest-setup.sh baked this deployment's own SSH public
  # key into the image, so no generic published disk could exist, and
  # GRAIN_KONTUR_IMAGE_BUCKET was there for an operator who built one
  # centrally and shared it across a fleet on one keypair.
  #
  # Both halves of that are gone. kontur generates the exec keypair per
  # VM boot, so nothing deployment-specific reaches the disk; and a guest
  # is now derived from a published kontur image by booting it,
  # provisioning it and committing the result (scripts/kontur/
  # build-guest.sh), which produces an image that is itself runnable. The
  # guest and the container it runs in are the same artifact, published
  # per commit by CI, and this host pulls it.
  ensure_kontur_oci_image
}

# ensure_kontur_oci_image resolves and pulls the sandbox image -- the
# kontur image carrying grain's own provisioned guest, which is both the
# container a VM runs in and the guest it boots.
#
# Unset (the default), GRAIN_KONTUR_OCI_IMAGE comes from the grain image
# itself: `grain sandbox-image` prints the reference stamped into this
# build at build time (cmd/grain/sandboximage.go), which is the sandbox
# built from the very same commit. That is what makes the two halves of a
# kontur deployment impossible to get out of step -- including on a
# rollback, where an older grain names its own older sandbox rather than
# whatever is newest.
#
# A pull that fails is not fatal to the whole install, the same trade
# every other kontur step here makes: kontur sandboxing goes off for this
# run, with a reason logged, and the deployment comes up dispatching into
# host directories instead of failing to deploy at all.
ensure_kontur_oci_image() {
  if [ -z "$GRAIN_KONTUR_OCI_IMAGE" ]; then
    # `|| true` so the guard below is the thing that reports a CLI that
    # could not answer -- without it `set -e` aborts the whole deploy on
    # the assignment, and this deployment never learns it could have
    # carried on with kontur simply switched off.
    GRAIN_KONTUR_OCI_IMAGE="$(/usr/local/bin/grain sandbox-image 2>/dev/null | tr -d '\r' | head -n1 || true)"
    if [ -z "$GRAIN_KONTUR_OCI_IMAGE" ]; then
      log "could not ask $GRAIN_IMAGE_REF which sandbox image it expects, and GRAIN_KONTUR_OCI_IMAGE names none -- leaving kontur sandboxing off this run"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
    log "sandbox container: $GRAIN_KONTUR_OCI_IMAGE (stamped into $GRAIN_IMAGE_REF at build time)"
  else
    log "sandbox container: $GRAIN_KONTUR_OCI_IMAGE (GRAIN_KONTUR_OCI_IMAGE overrides the built-in default)"
  fi

  # An Artifact Registry in this host's own project needs the metadata
  # server's token; GHCR's public package needs nothing, and a *private*
  # one is already covered -- pull_image's own registry_login ran earlier
  # against the same ghcr.io host with GRAIN_IMAGE_PULL_TOKEN, and docker
  # keeps that session for every later pull from it. So this is attempted
  # only for Artifact Registry, and is never fatal on its own: the pull
  # below is what actually decides.
  case "$GRAIN_KONTUR_OCI_IMAGE" in
    *-docker.pkg.dev/*|gcr.io/*)
      local registry_host="${GRAIN_KONTUR_OCI_IMAGE%%/*}"
      if ! docker login -u oauth2accesstoken -p "$(kontur_gcp_access_token)" "https://${registry_host}" >/dev/null 2>&1; then
        log "  could not authenticate docker to ${registry_host}; trying the pull anyway"
      fi
      ;;
  esac

  log "Pulling the sandbox container $GRAIN_KONTUR_OCI_IMAGE"
  if ! docker pull "$GRAIN_KONTUR_OCI_IMAGE"; then
    if docker image inspect "$GRAIN_KONTUR_OCI_IMAGE" >/dev/null 2>&1; then
      log "  pull failed, but this host already has it locally -- using that"
    else
      log "  could not pull $GRAIN_KONTUR_OCI_IMAGE and this host has no local copy -- leaving kontur sandboxing off this run"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
  fi
}

# konturctl itself is no longer built or installed here: it ships inside
# the deployment image (Dockerfile builds it from the same vendored
# third_party/kontur this checkout carries), which is where pkg/kontur
# execs it from, and install_cli_wrappers puts a wrapper for that same
# copy on this host's PATH for kontur-diag.sh and an operator's own use.
#
# That is what bwsalmon/agents#645 replaced ensure_konturctl with. It had
# to build the binary in the grain-builder image `make container-build`
# left behind, cache it by the vendored tree's git object id, and rebuild
# it whenever that changed -- all of it work to keep a host-installed
# binary in step with the vendored source, which an image built from that
# source in one CI job cannot fall out of step with in the first place.

# ensure_kontur_kvm_access grants $GRAIN_USER /dev/kvm (for the nested
# cloud-hypervisor guest itself) and the docker group (for the docker
# kontur backend's own `docker run`) -- the same grant grant_docker_group
# gives an operator's own shell, and safe to grant twice: usermod -aG
# only ever adds.
#
# It used to also create and chown the host directory konturctl put each
# VM's writable disk overlay in. There is no such directory any more: the
# overlay is created inside the VM's own container, against the disk the
# sandbox image carries (bwsalmon/kontur#37). What is left below is
# konturctl's state directory, which it still writes out here.
ensure_kontur_kvm_access() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ ! -e /dev/kvm ]; then
    log "GRAIN_KONTUR_ENABLE=1 but /dev/kvm does not exist on this host -- terraform/gcp's enable_nested_virtualization must be on and machine_type must support it (see that variable's own doc); leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  if getent group kvm >/dev/null 2>&1; then
    usermod -aG kvm "$GRAIN_USER"
  fi
  grant_docker_group
  # konturctl's own defaultStateDir (third_party/kontur/internal/cli/
  # vm.go) -- where it records each VM it creates. Never
  # overridden by a -kontur-create-arg -state-dir below, so this is the
  # exact path konturctl -- run unprivileged, as $GRAIN_USER -- actually
  # tries to create on its first "vm create": without this, that mkdir
  # fails closed with "permission denied" and grain-daemon.service dies
  # on its very first dispatched task.
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" /var/lib/kontur/vms
}

# ensure_kontur_git_proxy_host resolves GRAIN_KONTUR_GIT_PROXY_HOST -- the
# address startGitProxy (cmd/grain/daemon.go) advertises to a kontur VM in
# place of the loopback address it binds to by default, since a kontur VM's
# guest runs in its own network namespace with its own unrelated 127.0.0.1
# that this host's daemon is never listening behind (bwsalmon/agents#567:
# "Failed to connect to 127.0.0.1 ... Couldn't connect to server") -- to
# docker's own "bridge" network gateway address when an operator hasn't
# already set one explicitly. That address is what a kontur VM's outbound
# NAT (third_party/kontur/internal/netshim's own masqueradeExprs, leaving
# via the VM container's docker-assigned interface) routes through to reach
# this host, the same way any other container on that network would.
#
# Like ensure_kontur_kvm_access, resets GRAIN_KONTUR_ENABLE to 0 (with a
# log line explaining why) rather than installing a daemon whose every
# dispatched task would fail its very first git clone.
#
# `docker network inspect bridge`'s own .IPAM.Config only carries a
# "Gateway" key when something (a custom network, or an operator's own
# `docker network create --gateway`) set one explicitly -- the Debian
# `docker.io` package's bundled daemon (bwsalmon/agents#572: seen with
# docker.io 20.10.24 on the staging host image) never fills it
# in for the default bridge network's own auto-allocated pool, even
# after a container has actually attached to it, so `gw` here came back
# empty on every real install and permanently disabled kontur sandboxing.
#
# The fallback asks a container on that network what its own default
# route is, which *is* the address containers reach this host through --
# the thing being looked for, rather than a proxy for it. It used to
# read the bridge device's address with `ip` out here instead; a host
# without iproute2 is not one this script is willing to fail on any more
# (this file's header, "What this host has to have"), and the image
# carries no iproute2 either, hence /proc/net/route rather than `ip
# route` inside it. That file's gateway column is a little-endian hex
# word, which is why it is reassembled backwards below.
bridge_gateway() {
  local route dest gateway hex=""
  route="$(image_run --network bridge --entrypoint sh -- -c 'cat /proc/net/route' 2>/dev/null || true)"
  [ -n "$route" ] || return 0
  while read -r _ dest gateway _; do
    if [ "$dest" = "00000000" ] && [ -n "$gateway" ]; then
      hex="$gateway"
      break
    fi
  done <<< "$route"
  [ -n "$hex" ] || return 0
  printf '%d.%d.%d.%d\n' "0x${hex:6:2}" "0x${hex:4:2}" "0x${hex:2:2}" "0x${hex:0:2}"
}

ensure_kontur_git_proxy_host() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ -n "$GRAIN_KONTUR_GIT_PROXY_HOST" ]; then
    return
  fi
  local gw
  gw="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null)"
  if [ -z "$gw" ]; then
    gw="$(bridge_gateway)"
  fi
  if [ -z "$gw" ]; then
    log "GRAIN_KONTUR_ENABLE=1 but GRAIN_KONTUR_GIT_PROXY_HOST is unset and docker's own \"bridge\" network has no gateway address to default it to -- set GRAIN_KONTUR_GIT_PROXY_HOST explicitly (see this script's own -h); leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  GRAIN_KONTUR_GIT_PROXY_HOST="$gw"
  log "kontur git proxy host defaulted to docker bridge gateway $GRAIN_KONTUR_GIT_PROXY_HOST"
}

# --- 5. data directory and secrets --------------------------------------

seed_secret() {
  # Writes $2 to file $1 only if it is missing or empty, and only if a
  # value was actually given -- never overwrites a credential a previous
  # run (or an operator by hand) already placed, and never writes an
  # empty file for a value nobody supplied this time.
  #
  # Trailing newline appended deliberately: $2 reaches every caller
  # through at least one layer of "$(...)" command substitution (here,
  # in files/deploy.sh reading metadata -- bash strips every trailing
  # newline doing that), so without adding one back a multi-line
  # PEM/OpenSSH-format value is written one newline short of what it
  # started as. ssh-keygen/ssh both refuse to parse an OpenSSH private
  # key missing its final newline ("error in libcrypto", no clearer
  # message), which is what made the kontur SSH key this script used to
  # generate and seed invalid on every later run (bwsalmon/agents#543).
  # That key is gone -- kontur generates its own per boot now -- but the
  # GCP minter key is written through here the same way and would hit
  # exactly the same thing. Harmless for every other seed_secret caller
  # (github token, gemini-api-key): both are read with strings.TrimSpace
  # (pkg/gitproxy/credentials.go, cmd/grain/daemon.go's own
  # readTrimmedFile).
  local path="$1" value="$2"
  if [ -s "$path" ]; then
    return
  fi
  if [ -z "$value" ]; then
    return
  fi
  ( umask 077; printf '%s\n' "$value" > "$path" )
  chown "$GRAIN_USER:$GRAIN_USER" "$path"
}

# json_escape backslash-escapes $1 for embedding in a double-quoted JSON
# string -- just enough for seed_github_app_credential below, whose three
# inputs are a numeric App ID, a numeric installation ID, and a PEM
# private key (printable ASCII with embedded newlines, no literal quotes
# or backslashes of its own), so backslash, double-quote and newline are
# the only characters that ever need it.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\r'/}"
  s="${s//$'\n'/\\n}"
  printf '%s' "$s"
}

# seed_github_app_credential writes ${GRAIN_GITHUB_CREDENTIAL_NAME}.app.json
# from GRAIN_GITHUB_APP_ID/INSTALLATION_ID/PRIVATE_KEY, the same
# never-overwrite contract seed_secret gives every plain-token credential
# above: an App credential already on disk, placed by hand or by an
# earlier run, is left alone. pkg/gitproxy.CredentialSet.loadAppCredential
# reads this file's app_id/installation_id/private_key fields and mints a
# refreshing installation token from them -- both the git proxy's own
# push/fetch auth and the daemon's REST client (checks, merges,
# cmd/grain/daemon.go's credentialTokenSource) read through that same
# ladder, so no separate wiring is needed for either to pick it up.
seed_github_app_credential() {
  local path="$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.app.json"
  if [ -s "$path" ]; then
    return
  fi
  if [ -z "$GRAIN_GITHUB_APP_ID" ] && [ -z "$GRAIN_GITHUB_APP_INSTALLATION_ID" ] && [ -z "$GRAIN_GITHUB_APP_PRIVATE_KEY" ]; then
    return
  fi
  if [ -z "$GRAIN_GITHUB_APP_ID" ] || [ -z "$GRAIN_GITHUB_APP_INSTALLATION_ID" ] || [ -z "$GRAIN_GITHUB_APP_PRIVATE_KEY" ]; then
    log "  GRAIN_GITHUB_APP_ID/INSTALLATION_ID/PRIVATE_KEY: only some are set -- need all three to seed an App credential; ignoring"
    return
  fi
  ( umask 077
    printf '{"app_id":"%s","installation_id":"%s","private_key":"%s"}\n' \
      "$(json_escape "$GRAIN_GITHUB_APP_ID")" \
      "$(json_escape "$GRAIN_GITHUB_APP_INSTALLATION_ID")" \
      "$(json_escape "$GRAIN_GITHUB_APP_PRIVATE_KEY")" > "$path" )
  chown "$GRAIN_USER:$GRAIN_USER" "$path"
}

# state_repo_field prints one field of <data-dir>/state-repo.json, or
# nothing when the file does not exist or does not carry it.
#
# Read with sed rather than jq because this script's whole dependency
# list is docker and systemd (see its header), and jq is deploy.sh's
# dependency rather than this one's. That is honest here specifically:
# pkg/staterepo writes this file itself, with encoding/json's
# MarshalIndent, so it is one field per line with no surprises -- and the
# only cost of misreading it is that configure_state_repo below rewrites
# a file it did not have to.
state_repo_field() {
  local file="$GRAIN_DATA_DIR/state-repo.json"
  [ -s "$file" ] || return 0
  sed -n "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*\$/\1/p" "$file" | head -n1
}

# archive_state_repo moves the state repository's working tree aside
# under a name saying why, and carries the encrypted secrets file into
# the fresh one.
#
# Moved, never deleted: a working tree is a clone of one repository's
# history and cannot become a clone of another by changing a URL, but it
# may be the only copy of commits a remote never got, so the recovery
# path has to stay open. `grain state adopt` makes the same trade for the
# same reason.
#
# The secrets file is the exception, and travels: it is the one thing in
# there that is not regenerable from the database, the key it is
# encrypted to has not changed, and an operator who kept their key keeps
# their credentials across this.
archive_state_repo() {
  local suffix="$1" repo_dir="$GRAIN_DATA_DIR/state-repo"
  [ -d "$repo_dir" ] || return 0
  local backup="${repo_dir}.${suffix}"
  mv "$repo_dir" "$backup"
  log "  moved the previous working tree to $backup -- nothing here is deleted"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$repo_dir"
  if [ -s "$backup/secrets.enc" ]; then
    cp -p "$backup/secrets.enc" "$repo_dir/secrets.enc"
    chown "$GRAIN_USER:$GRAIN_USER" "$repo_dir/secrets.enc"
    log "  carried this deployment's encrypted secrets across to the fresh tree"
  fi
}

# configure_state_repo points this deployment at the repository its state
# lives in, and makes sure the working tree for it actually exists.
#
# <data-dir>/state-repo.json is the one piece of grain's configuration
# that cannot live in the state repository, because it is what says where
# that repository is (pkg/staterepo's Settings). The UI's bootstrap pane
# writes it too; this is the other way in, for a deployment that is
# applied rather than visited.
#
# It converges rather than seeding once -- GRAIN_STATE_REPO_URL set is
# Terraform's answer, and a fleet host that ignored it after the first
# run would drift -- but only when a value is actually given. Left empty,
# nothing here writes anything, so a hand-installed deployment and a
# local-only laptop keep whatever they chose, and a tokenFile placed by
# `grain state adopt -token-file` survives a re-run that changes the
# branch.
configure_state_repo() {
  local file="$GRAIN_DATA_DIR/state-repo.json"
  if [ -n "$GRAIN_STATE_REPO_URL" ]; then
    local current_remote current_branch token_file
    current_remote="$(state_repo_field remote)"
    current_branch="$(state_repo_field branch)"
    token_file="$(state_repo_field tokenFile)"
    if [ "$current_remote" != "$GRAIN_STATE_REPO_URL" ] || [ "$current_branch" != "$GRAIN_STATE_REPO_BRANCH" ]; then
      if [ -n "$current_remote" ] && [ "$current_remote" != "$GRAIN_STATE_REPO_URL" ]; then
        log "State repository changed: $current_remote -> $GRAIN_STATE_REPO_URL"
        archive_state_repo "replaced-$(date +%Y%m%d%H%M%S)"
      fi
      log "Pointing this deployment's state at $GRAIN_STATE_REPO_URL ($GRAIN_STATE_REPO_BRANCH)"
      ( umask 077
        {
          printf '{\n'
          printf '  "remote": "%s",\n' "$(json_escape "$GRAIN_STATE_REPO_URL")"
          if [ -n "$token_file" ]; then
            printf '  "branch": "%s",\n' "$(json_escape "$GRAIN_STATE_REPO_BRANCH")"
            printf '  "tokenFile": "%s"\n' "$(json_escape "$token_file")"
          else
            printf '  "branch": "%s"\n' "$(json_escape "$GRAIN_STATE_REPO_BRANCH")"
          fi
          printf '}\n'
        } > "$file" )
      chown "$GRAIN_USER:$GRAIN_USER" "$file"
    fi
  fi

  # Opened here rather than left to grain-daemon.service's first start,
  # because the steps after this one write *into* the working tree:
  # seed_gcp_minter_key runs `grain secrets set`, and the file that
  # lands in belongs to the repository. Cloning afterwards would either
  # fail (git will not clone into a directory with files in it) or
  # replace what was just written with the remote's own copy -- and the
  # minter key push-secrets.sh rotated on this very deploy would be the
  # thing lost.
  #
  # `status` is what opens it: it clones a repository this host has not
  # seen before, or `git init`s a local-only one, and then prints where
  # state lives -- which is worth having in a deploy log either way.
  #
  # Never fatal. A remote that is unreachable, or a credential that does
  # not cover it, must not cost the deployment its service: the daemon
  # retries on every start and the UI's bootstrap pane is still there.
  if ! /usr/local/bin/grain state -data-dir "$GRAIN_DATA_DIR" status; then
    log "  could not open the state repository yet -- grain-daemon.service will retry on"
    log "  start, and the UI's bootstrap pane can point it somewhere else. Check that"
    log "  the GitHub credential above covers ${GRAIN_STATE_REPO_URL:-the repository these settings name}."
  fi
}

# setup_sandbox_dir lays out GRAIN_SANDBOX_DIR, orchestrator.HostSandboxes'
# own baseDir (-sandbox-dir, cmd/grain/daemon.go) -- deliberately its own
# function, separate from setup_data_dir below, since the whole point
# (bwsalmon/agents#587) is that this directory is not part of
# $GRAIN_DATA_DIR: run unconditionally, since a deployment can flip
# GRAIN_KONTUR_ENABLE later without a fresh install, and grain-daemon.service
# always passes -sandbox-dir either way (write_systemd_units) even though
# only the non-kontur path ever reads it.
setup_sandbox_dir() {
  log "Laying out $GRAIN_SANDBOX_DIR"
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_SANDBOX_DIR"
  # An override that puts the sandbox root inside $GRAIN_DATA_DIR undoes
  # the split this function exists for, and does it quietly: a run's
  # checkout then shares a filesystem with the SQLite store and the
  # secrets database, so the disk a few large checkouts fill is the disk
  # everything else needs to write to. Warned rather than refused --
  # a deployment with one big disk and nothing else to point at is a
  # legitimate choice, and it is only worth knowing what it costs.
  case "$GRAIN_SANDBOX_DIR/" in
    "$GRAIN_DATA_DIR"/*)
      log "WARNING: $GRAIN_SANDBOX_DIR is inside \$GRAIN_DATA_DIR ($GRAIN_DATA_DIR), so a task's checkout"
      log "WARNING: shares a filesystem with the store and secrets; a full sandbox disk takes those down"
      log "WARNING: with it. Point GRAIN_SANDBOX_DIR at a disk of its own (default: /var/lib/grain-sandbox)."
      ;;
  esac
}

setup_data_dir() {
  log "Laying out $GRAIN_DATA_DIR"
  install -d -m0750 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR"
  # bin/ no longer holds a grain binary -- nothing on this host does
  # (install_cli_wrappers) -- but it is kept, owned by $GRAIN_USER, for
  # the same reason $GRAIN_DATA_DIR itself is: a deployment upgraded
  # across bwsalmon/agents#645 already has one, with a previous release's
  # binary in it, and an operator who put something of their own there
  # should find it where they left it. install -d re-applies -o/-g (and
  # its -m mode) even against a directory that already exists, which is
  # exactly what is wanted here and exactly why it is never used on a
  # directory this script does not own.
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/bin"
  # A real, writable HOME for the daemon, exported by grain-daemon.service
  # (write_systemd_units). $GRAIN_USER is created --no-create-home, so the
  # home its passwd entry names (/home/grain) does not exist -- which is
  # fine for the daemon itself, and not fine for the claude CLI it now
  # execs per dispatch: that writes its own state under $HOME, and would
  # fail on every run against a directory that isn't there. Under the data
  # dir, not /home, so it lives on the same disk as everything else this
  # deployment keeps and is backed up with it.
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/home"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github"
  # The state repository (pkg/staterepo): the working tree grain exports
  # its database into, and where the encrypted secrets file lives. Made
  # here, owned by $GRAIN_USER, because the CLI steps below write into it
  # as root -- and a git working tree with root-owned files in it is one
  # the daemon's own git refuses to touch at all ("dubious ownership"),
  # not merely one it cannot write.
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/state-repo"
  # grain-daemon.service's own -data-dir/store, embedded SQLite -- the
  # one process that ever opens it now (this file's own header on
  # bwsalmon/agents#363), so no separate store container or directory
  # layout is needed for it beyond what openStore creates on its own the
  # first time the daemon starts.

  # GitHub credential ladder (pkg/gitproxy/credentials.go): a pattern
  # file plus one <name>.token or <name>.app.json per credential. "*" is
  # the catch-all every repo falls back to absent a narrower entry; this
  # script only ever seeds the one default, as either kind (never both
  # for the same name: CredentialSet.load prefers .app.json when
  # present). A per-repo credential is added from the UI rather than
  # here -- see below.
  #
  # These files are the whole mechanism, and the only one: unlike the
  # agent credentials below, a GitHub credential is never read out of the
  # secrets database (grain/task-137 settled that deliberately -- see
  # pkg/gitproxy/credentials.go's own doc comment). What did change is
  # who else can write one: Settings -> GitHub in the UI adds and removes
  # a <name>.token here, and writes credentials.json itself
  # (grain/task-4), so neither an extra named token nor the ladder entry
  # pointing repos at it needs shell access to this host. Seeding either
  # from here is a convenience for an unattended deploy, not the only
  # way in -- a host that reaches this point with no GitHub credential at
  # all is finishable from the UI, which is what the log line at the end
  # of this function says. A ladder entry is live as soon as it is
  # written; a new *token* still needs the daemon restarted before it can
  # be ticked on a task, the same as everything else in this directory.
  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
     && { [ -n "$GRAIN_GITHUB_TOKEN" ] || [ -n "$GRAIN_GITHUB_APP_ID" ]; }; then
    printf '{"*":"%s"}\n' "$GRAIN_GITHUB_CREDENTIAL_NAME" > "$GRAIN_DATA_DIR/secrets/github/credentials.json"
    chown "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github/credentials.json"
  fi
  seed_secret "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" "$GRAIN_GITHUB_TOKEN"
  seed_github_app_credential

  # Before anything below runs `grain secrets`: that opens the store, and
  # opening a store with no key mints one (pkg/secrets.Open). A host
  # rebuilt with GRAIN_SECRETS_KEY set would then be holding a key it
  # generated a moment before this line, rather than the one its own
  # state repository's secrets file is encrypted to -- and pkg/secrets
  # would rightly refuse to read that file for the rest of the
  # deployment's life.
  seed_secret "$GRAIN_DATA_DIR/secrets/secrets.key" "$GRAIN_SECRETS_KEY"

  # And before it too, for the other half of the same ordering: the
  # encrypted secrets file lives *inside* the state repository's working
  # tree, so the tree has to be the deployment's real one -- cloned from
  # the remote, with the secrets file the remote holds -- before
  # seed_gcp_minter_key writes this deploy's freshly rotated minter key
  # into it. Written into a tree that is later replaced by a clone, that
  # rotation is simply lost, and push-secrets.sh has already begun
  # invalidating the key the daemon is still using.
  configure_state_repo

  # Order matters below: the minter key has to be in the secrets
  # database before mint_gemini_operating_key can authenticate with it.
  seed_secret "$GRAIN_DATA_DIR/secrets/gemini-api-key" "$GRAIN_GEMINI_API_KEY"
  seed_secret "$GRAIN_DATA_DIR/secrets/claude-oauth-token" "$GRAIN_CLAUDE_CODE_OAUTH_TOKEN"
  seed_secret "$GRAIN_DATA_DIR/secrets/openai-api-key" "$GRAIN_OPENAI_API_KEY"

  seed_gcp_minter_key

  mint_gemini_operating_key

  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ]; then
    log "  no GitHub credential configured yet -- open Settings -> GitHub in the UI and paste a"
    log "  token: the first one added becomes this deployment's default, no restart needed."
    log "  Unattended: set GRAIN_GITHUB_TOKEN, or all three of"
    log "  GRAIN_GITHUB_APP_ID/INSTALLATION_ID/PRIVATE_KEY, and re-run"
  fi
}

# seed_gcp_minter_key writes the GCP minter's key into the gcp-key-minter
# secret in the (SQLite-backed, pkg/secrets's own doc comment) secrets
# database -- through the just-installed `grain secrets` CLI rather than a
# raw file write, since that database is no longer a directory tree this
# script can lay files into directly; chown afterward because the CLI, run
# here as root, otherwise leaves the database it creates owned by root,
# unreadable by grain-daemon.service's own $GRAIN_USER.
#
# **It converges rather than seeding once, unlike every plain-file secret
# seed_secret places above, and that difference is the whole point.** This
# used to return early whenever `grain secrets list` already showed a
# gcp-key-minter entry, on the same "never overwrite what is already
# there" rule as the rest -- and for a credential nobody rotates, that
# rule costs nothing. This one is rotated, by the deployment's own
# machinery: terraform/gcp/deploy/push-secrets.sh mints a *fresh* minter
# key on every run, pushes it into instance metadata, and then deletes
# every key on the minter account beyond the newest two. The host, having
# seeded its copy on the first deploy and refused every later one, went on
# authenticating with the key from that first run -- so the third
# push-secrets.sh run deleted the credential the daemon was actually
# using, out from under a deployment that had no idea it had been handed
# a replacement twice.
#
# Nothing failed visibly when that happened. `gcp-key`'s own Settings row
# still read Ready (the secret is set; only GCP knows the key inside it is
# dead), and the failure surfaced a task at a time, as GCP's own
# `invalid_grant` / "Invalid JWT Signature" out of a mint -- see
# pkg/capability/gcpkey's explainRefusedCredential, which now names this
# secret when it happens. The documented remedy was to delete the entry by
# hand over IAP and bump deploy_generation, which is a manual step to
# repair a state the deployment itself created.
#
# So: a key given to this script is the key the daemon ends up holding,
# every run. An operator's own `grain secrets set` still wins for as long
# as no deploy carries a key of its own (the early return below), which is
# the case a hand-installed deployment is actually in -- what it no longer
# does is win against the rotation that is about to invalidate it.
seed_gcp_minter_key() {
  if [ -z "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" ]; then
    return
  fi
  # Staged under $GRAIN_DATA_DIR before being handed to the CLI, and
  # removed straight after: `grain` is a wrapper around `docker run` now
  # (install_cli_wrappers), and -value-file has to name a path that
  # resolves *inside* that container. The file this is given is
  # deliberately somewhere else -- terraform/gcp/files/deploy.sh
  # writes it into a tmpfs under /run -- and mounting an arbitrary
  # operator-supplied path into the daemon's own container for the sake
  # of one read would be a far worse trade than a copy that lives for
  # one command.
  # Staged as $GRAIN_USER, not as root. This script runs as root, so the
  # obvious `umask 077 && cat >` writes a 0600 file owned by root -- and
  # the `grain` that reads it is a `docker run --user` as $GRAIN_USER
  # (install_cli_wrappers), which cannot read that. It failed there,
  # under `set -e`, after the image had been pulled and before
  # write_systemd_units ever ran: a deployment with a minter key got no
  # grain-daemon.service at all, and the only trace was the CLI's own
  # "permission denied" in the deploy log.
  #
  # `install` sets mode and ownership as it copies, so the file is never
  # momentarily readable by anyone else the way a chown after the fact
  # would leave it.
  local staged="$GRAIN_DATA_DIR/secrets/.minter-key.staged.json"
  install -m0600 -o "$GRAIN_USER" -g "$GRAIN_USER" \
    "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" "$staged"
  local key
  key="$(minter_secret_key)"
  /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" set \
    -value-file "$staged" gcp-key-minter "$key"
  rm -f "$staged"
  chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets" "$GRAIN_DATA_DIR/state-repo"
}

# minter_secret_key names the key *inside* the gcp-key-minter secret that
# this deployment's minter credential is written to: whichever single key
# the secret already holds, and key.json for a secret that does not exist
# yet.
#
# Writing in place matters because gcpkey.Config.MinterCredential names
# the bare secret, and pkg/secrets.Store.Resolve only answers a bare name
# when the secret holds exactly one key -- so a secret carrying both a
# `value` written from Settings and a `key.json` written here resolves to
# an error rather than to either of them, and the capability breaks in a
# second way while this function is busy fixing the first. (pkg/ui writes
# to the key already there for exactly the same reason; this is that rule
# from the other side.) A secret that has somehow ended up with several
# keys is already in that state and gets key.json, which is at least the
# name the rest of this deployment's documentation uses.
minter_secret_key() {
  local keys
  keys="$(/usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null |
    sed -n 's/^gcp-key-minter: //p' || true)"
  case "$keys" in
    "" | *,*) echo "key.json" ;;
    *) echo "$keys" ;;
  esac
}

# mint_gemini_operating_key mints the daemon's own Gemini API key, using
# the minter credential seed_gcp_minter_key just placed, when no key is
# in place yet.
#
# A deployment that grants its minter roles/serviceusage.apiKeysAdmin
# (terraform/gcp's enable_gemini_key, on by default there) already has
# every permission this needs, on this host -- so nobody has to paste a
# Gemini key in by hand before this deployment can dispatch anything.
# Where that grant is absent the mint simply fails, and the deployment is
# left exactly where a deployment with no agent credential now sits: the
# daemon runs, the UI serves, and a key can be pasted into Settings
# instead. It must never fail the whole converge, since the GitHub side
# of a deployment is useful without it and a half-applied setup.sh is
# worse than a deployment that cannot yet dispatch.
#
# Seed-once, like everything else here: `grain secrets mint-gemini-key`
# leaves an existing non-empty key file untouched, so config-sync
# re-running this on every convergence pass does not issue a new key each
# time. To rotate, delete the file (and the old key in GCP -- the
# capability's own reaper deliberately never touches an operating key)
# and let the next pass mint a fresh one.
mint_gemini_operating_key() {
  if [ -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ]; then
    return
  fi
  # Guarded on the project alone, not also on
  # GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE: seed_gcp_minter_key needs that
  # file to seed a credential, but a re-run where the credential is
  # already in the secrets database has no key file and can still mint.
  # A project set with no minter credential anywhere fails fast and
  # locally, on the resolve, and is reported rather than fatal.
  if [ -z "$GRAIN_GCP_PROJECT" ]; then
    return
  fi
  log "  no Gemini API key in place -- minting the daemon's own from the GCP minter credential"
  # Run as root and chown afterward, the same shape seed_gcp_minter_key
  # above uses -- this script is root already (its own `id -u` check) and
  # the key file it writes has to end up owned by GRAIN_USER either way.
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" \
     mint-gemini-key -project "$GRAIN_GCP_PROJECT"; then
    chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets" "$GRAIN_DATA_DIR/state-repo"
  else
    log "  could not mint a Gemini API key -- the daemon still runs, but a gemini-framework"
    log "  run cannot dispatch until one exists. Paste one into the UI (Settings -> Agent"
    log "  frameworks), check the minter credential holds roles/serviceusage.apiKeysAdmin,"
    log "  or set GRAIN_GEMINI_API_KEY and re-run."
  fi
}

# --- 6. reformat the store if a breaking schema change shipped ---------
#
# pkg/model.SchemaVersion's own doc comment: bumped exactly when Tables
# or Views change in a way Store.Init's `CREATE TABLE IF NOT EXISTS`
# cannot safely reconcile an existing database into -- Init only adds a
# table that is missing outright, never a column on one that already
# exists, so a build newer than the database it finds would otherwise
# start silently against stale columns instead of refusing or fixing
# anything. `grain schema-version` (cmd/grain/schemaversion.go) is that
# same constant, printed by the binary this run just installed, so this
# function never has to duplicate pkg/model's own definition of
# "breaking" by parsing source.
#
# $GRAIN_DATA_DIR/.schema_version is the marker this function itself
# maintains, recording the schema version the store on disk was last
# known to agree with. No marker yet -- a brand new $GRAIN_DATA_DIR, or
# an upgrade from before this function existed -- never wipes anything:
# it just records the current version and moves on, since there is
# nothing to safely compare against and "assume compatible, preserve the
# data" is the safer of the two guesses. A marker that disagrees with the
# fresh build moves $GRAIN_DATA_DIR/store aside to a timestamped backup
# -- never deletes it outright, so a mistaken schema bump is recoverable
# by hand -- and grain's own sqlite.Open (pkg/model/sqlite) recreates an
# empty one, with the new schema, the next time grain-daemon.service
# starts.
reformat_store_if_schema_changed() {
  local marker="$GRAIN_DATA_DIR/.schema_version"
  local store_dir="$GRAIN_DATA_DIR/store"
  local new_version
  new_version="$(/usr/local/bin/grain schema-version 2>/dev/null || true)"
  # An unanswerable CLI is reported and stepped over, never guessed at:
  # writing an empty marker would make the *next* run read a schema
  # change that never happened and move a live store aside for nothing,
  # and aborting here would cost the deployment its service over a
  # question that only decides whether to reformat.
  if [ -z "$new_version" ]; then
    log "WARNING: could not read a schema version out of $GRAIN_IMAGE_REF -- leaving"
    log "         $store_dir and the marker exactly as they are. If the schema did change"
    log "         in this image, the daemon will say so on its first start."
    return
  fi

  if [ ! -s "$marker" ]; then
    log "No schema-version marker yet -- recording schema $new_version, not touching any existing store"
    printf '%s\n' "$new_version" > "$marker"
    return
  fi

  local old_version
  old_version="$(cat "$marker")"
  if [ "$old_version" = "$new_version" ]; then
    return
  fi

  local stamp
  stamp="$(date +%Y%m%d%H%M%S)"
  if [ -d "$store_dir" ]; then
    local backup="${store_dir}.schema${old_version}-${stamp}"
    log "Schema changed ($old_version -> $new_version) -- moving $store_dir aside to $backup so grain starts a fresh store"
    mv "$store_dir" "$backup"
  else
    log "Schema changed ($old_version -> $new_version) -- no existing store to move aside"
  fi

  # The state repository holds the same rows as the store, exported
  # (pkg/staterepo), so a schema this build cannot read in the store is
  # one it cannot import out of the repository either -- staterepo.Load
  # refuses an older dump outright rather than guessing at a migration.
  # Moving the working tree aside alongside the store is what lets the
  # daemon come up and re-seed it from the fresh database; nothing is
  # lost that a remote does not still have, and a local-only deployment
  # keeps the old tree right here under its timestamped name.
  #
  # The encrypted secrets file used to live in that tree, and had to be
  # carried across by hand here because it is the one thing in there that
  # cannot be regenerated. It lives beside its own private key under
  # $GRAIN_DATA_DIR/secrets now (grain/task-186: everything a sandbox can
  # clone is everything a sandbox can read, and the state repository is
  # somewhere agents are dispatched to work), which archive_state_repo
  # never touches -- so a schema bump no longer goes anywhere near it. A
  # tree written by an older build still has its copy carried across,
  # since grain moves it out on its next start and a tree that was
  # archived first would take it with it.
  if [ -d "$GRAIN_DATA_DIR/state-repo" ]; then
    log "Schema changed -- moving the state repository aside as well; grain re-seeds it on its next start"
    archive_state_repo "schema${old_version}-${stamp}"
    if [ -s "$GRAIN_DATA_DIR/state-repo/secrets.enc" ] && [ ! -s "$GRAIN_DATA_DIR/secrets/secrets.enc" ]; then
      install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
      mv "$GRAIN_DATA_DIR/state-repo/secrets.enc" "$GRAIN_DATA_DIR/secrets/secrets.enc"
      chown "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/secrets.enc"
      log "  carried this deployment's encrypted secrets across, beside its key in $GRAIN_DATA_DIR/secrets"
    fi
  fi
  printf '%s\n' "$new_version" > "$marker"
}

# --- 7. format the target repo, if it is empty --------------------------
#
# Every dispatch grain runs branches off an existing ref -- it never
# creates the first one (tests/e2e's own harness always seeds one commit
# before driving anything against a bare repo). A repo created fresh on
# GitHub has none, so `grain create -repo owner/name ...` would have
# nothing to branch from. Detected with `git ls-remote`, which returns no
# output at all against a repo with zero refs -- no clone needed just to
# find that out.
#
# Both git calls run inside the deployment image (image_run), which
# carries git because the orchestrator clones every task's repo with it
# (Dockerfile). They are the only two steps here that want a real git,
# and running them in there is what lets this host have none at all --
# see this file's header, "What this host has to have". --network host
# so a GRAIN_GITHUB_HOST on this host's own loopback (a mock server,
# under GRAIN_GITHUB_INSECURE_HTTP) is reachable from in there too, and
# the URL travels as an environment variable rather than an argument,
# since it carries a token and a command line is world-readable in `ps`.
#
# The scratch repository is made *inside* the container as well: it
# exists for one empty commit and is thrown away with the container, so
# there is nothing to mount and nothing on this host to clean up.

format_target_repo_if_empty() {
  if [ -z "$GRAIN_TARGET_REPO" ]; then
    return
  fi
  local token="$GRAIN_GITHUB_TOKEN"
  if [ -z "$token" ] && [ -s "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" ]; then
    token="$(cat "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token")"
  fi
  if [ -z "$token" ]; then
    log "GRAIN_TARGET_REPO is set but no GitHub token is available -- skipping the empty-repo check"
    return
  fi

  local proto="https"
  [ "$GRAIN_GITHUB_INSECURE_HTTP" = "1" ] && proto="http"
  local url="${proto}://x-access-token:${token}@${GRAIN_GITHUB_HOST}/${GRAIN_TARGET_REPO}.git"

  log "Checking whether $GRAIN_TARGET_REPO has any commits yet"
  # The single quotes are the point, not an oversight (SC2016): what is
  # between them is the *container's* sh script, and $GRAIN_TARGET_URL is
  # for that sh to expand out of its own environment. Expanding it here
  # would interpolate the token into an argv this host can read in `ps`,
  # which is exactly what the --env above exists to avoid.
  # shellcheck disable=SC2016
  if [ -n "$(image_run --network host --env "GRAIN_TARGET_URL=$url" \
       --entrypoint sh -- -c 'git ls-remote "$GRAIN_TARGET_URL"' 2>/dev/null)" ]; then
    return
  fi

  log "  it's empty -- pushing one empty commit to $GRAIN_TARGET_BRANCH so grain has something to branch from"
  # Same as above: this whole block is the container's script, and its
  # $1, $branch, $tmp and $GRAIN_TARGET_URL all belong to the sh that
  # runs it. Nothing in it may be expanded here (SC2016).
  # shellcheck disable=SC2016
  image_run --network host --env "GRAIN_TARGET_URL=$url" --entrypoint sh -- -c '
    set -e
    branch="$1"
    tmp="$(mktemp -d)"
    git init --quiet -b "$branch" "$tmp"
    git -C "$tmp" -c user.name=grain -c user.email=grain@localhost \
      commit --quiet --allow-empty -m "Initial commit (created by grain setup.sh)"
    git -C "$tmp" push --quiet "$GRAIN_TARGET_URL" "HEAD:refs/heads/$branch"
  ' format_target_repo "$GRAIN_TARGET_BRANCH"
}

# --- 8. the systemd unit ---------------------------------------------------

# Every agent framework runs a CLI as a subprocess -- agent/antigravity
# execs `agy`, agent/claude execs `claude`, agent/codex execs `codex` --
# and the deployment image carries all three (Dockerfile). This checks
# that what is about to be deployed actually does, rather than letting
# grain-daemon.service come up and fail at its first dispatch with
# "executable file not found in $PATH".
#
# Asked of the image, not of this host: that is where they live now. The
# exception is an operator-named copy (GRAIN_CLAUDE_PATH/GRAIN_AGY_PATH/
# GRAIN_CODEX_PATH), which is a host path by definition -- docker_run_args
# mounts it in, and a path that names nothing executable out here is
# worth saying out loud here rather than discovering at dispatch.
#
# Never fatal, and reported rather than enforced: which framework a run
# uses is a live UI choice, a deployment may legitimately only ever use
# one of them, and an image missing one still runs everything else. The
# readiness summary says it again at the end.
# `|| true` is load-bearing: `command -v` exits non-zero for a binary
# that is not there, which is exactly the case every caller here exists
# to report -- and a caller assigns this to a variable, where `set -e`
# would turn that report into an aborted deploy.
agent_cli_in_image() {
  image_run --entrypoint sh -- -c "command -v $1" 2>/dev/null | head -n1 || true
}

# report_agent_cli renders one agent CLI's line for the readiness summary:
# where it resolves, or MISSING and why. $1 is the binary, $2 the
# operator's override for it, $3 that override's variable name.
report_agent_cli() {
  local name="$1" override="$2" var="$3" path
  if [ -n "$override" ]; then
    if [ -x "$override" ]; then
      printf '%s (mounted from this host)\n' "$override"
    else
      printf 'MISSING (%s names nothing executable on this host)\n' "$var"
    fi
    return
  fi
  path="$(agent_cli_in_image "$name")"
  if [ -n "$path" ]; then
    printf '%s (in %s)\n' "$path" "$GRAIN_IMAGE_REF"
  else
    printf 'MISSING\n'
  fi
}

verify_agent_cli() {
  local name path override
  for name in agy claude codex; do
    case "$name" in
      agy) override="$GRAIN_AGY_PATH" ;;
      claude) override="$GRAIN_CLAUDE_PATH" ;;
      codex) override="$GRAIN_CODEX_PATH" ;;
    esac
    if [ -n "$override" ]; then
      if [ -x "$override" ]; then
        continue
      fi
      log "WARNING: the $name path this deployment was given ($override) is not an executable"
      log "         file on this host. docker_run_args mounts it into the container at that"
      log "         same path, so a dispatch using it will fail until it is one."
      continue
    fi
    path="$(agent_cli_in_image "$name")"
    [ -n "$path" ] && continue
    log "WARNING: $GRAIN_IMAGE_REF carries no \"$name\" binary. The agent framework that runs"
    log "         it as a subprocess cannot dispatch until it does -- deploy an image built"
    log "         with it (Dockerfile installs every agent CLI), or set the matching"
    log "         GRAIN_AGY_PATH/GRAIN_CLAUDE_PATH/GRAIN_CODEX_PATH to a copy on this host."
  done
}

write_systemd_units() {
  verify_agent_cli
  log "Writing grain-daemon.service"

  local daemon_args=(
    daemon
    -data-dir "$GRAIN_DATA_DIR"
    -sandbox-dir "$GRAIN_SANDBOX_DIR"
    -max-workers "$GRAIN_MAX_WORKERS"
    -max-mergers "$GRAIN_MAX_MERGERS"
    -poll-interval "$GRAIN_POLL_INTERVAL"
    -gemini-api-key-file "$GRAIN_DATA_DIR/secrets/gemini-api-key"
    -claude-oauth-token-file "$GRAIN_DATA_DIR/secrets/claude-oauth-token"
    -openai-api-key-file "$GRAIN_DATA_DIR/secrets/openai-api-key"
    -github-host "$GRAIN_GITHUB_HOST"
    -ui-addr "$GRAIN_UI_ADDR"
  )
  # bwsalmon/agents#396: the UI's own Upgrade button, which on a
  # container deployment means "pull the image CI published for that
  # branch and restart onto it" rather than "fetch, build, install"
  # (bwsalmon/agents#645, pkg/upgrade/image.go). -upgrade-image names
  # the repository CI publishes a tag per branch to; -upgrade-image-ref-
  # file is the file this unit itself reads as an EnvironmentFile, so an
  # upgrade repoints the service by writing one line and restarting, with
  # no unit to rewrite and no root anywhere in the path.
  #
  # -upgrade-src-dir is not passed at all. It used to ride along, not to
  # build (with -upgrade-image set the daemon never does) but because
  # grantTools read the checkout it named for the self-debug capability.
  # The deployment image now carries the source it was built from
  # (Dockerfile), and cmd/grain/daemon.go's sourceDir prefers that copy
  # over any flag -- which is the point: this checkout tracks a branch and
  # the image is a fixed tag, so the two drifted apart on every upgrade
  # and the agent read source that was not what was running.
  #
  # Left unset entirely when GRAIN_ENABLE_UI_UPGRADE=0
  # (terraform/gcp's own deploy.sh sets exactly that): the daemon
  # flags themselves default to empty/disabled (cmd/grain/daemon.go), so
  # simply not passing them is enough -- see this script's own header on
  # GRAIN_ENABLE_UI_UPGRADE (bwsalmon/agents#405).
  if [ "$GRAIN_ENABLE_UI_UPGRADE" = "1" ]; then
    daemon_args+=(
      -upgrade-image "$GRAIN_IMAGE"
      -upgrade-image-ref-file "$IMAGE_REF_FILE"
      -upgrade-restart-cmd touch -upgrade-restart-cmd "$CONTROL_DIR/restart"
    )
  fi

  # The UI's reboot-host button, through the same control channel: a
  # touch out of the container, a path unit out here (write_control_units)
  # that turns it into `systemctl reboot`. Always passed -- unlike the
  # Upgrade button this has no opt-out, and its default
  # (`sudo systemctl reboot`, cmd/grain/daemon.go's defaultRebootCmd) is
  # the one thing that cannot work from inside a container.
  daemon_args+=(-reboot-cmd touch -reboot-cmd "$CONTROL_DIR/reboot")
  [ -n "$GRAIN_AGY_PATH" ] && daemon_args+=(-agy-path "$GRAIN_AGY_PATH")
  [ -n "$GRAIN_GEMINI_MODEL" ] && daemon_args+=(-gemini-model "$GRAIN_GEMINI_MODEL")
  [ -n "$GRAIN_GEMINI_EFFORT" ] && daemon_args+=(-gemini-effort "$GRAIN_GEMINI_EFFORT")
  [ -n "$GRAIN_CLAUDE_PATH" ] && daemon_args+=(-claude-path "$GRAIN_CLAUDE_PATH")
  [ -n "$GRAIN_CLAUDE_MODEL" ] && daemon_args+=(-claude-model "$GRAIN_CLAUDE_MODEL")
  [ -n "$GRAIN_CODEX_PATH" ] && daemon_args+=(-codex-path "$GRAIN_CODEX_PATH")
  [ -n "$GRAIN_CODEX_MODEL" ] && daemon_args+=(-codex-model "$GRAIN_CODEX_MODEL")
  [ -n "$GRAIN_MAX_AGENT_TURNS" ] && daemon_args+=(-max-agent-turns "$GRAIN_MAX_AGENT_TURNS")
  [ "$GRAIN_GITHUB_INSECURE_HTTP" = "1" ] && daemon_args+=(-github-insecure-http)
  [ -n "$GRAIN_GCP_PROJECT" ] && daemon_args+=(-gcp-project "$GRAIN_GCP_PROJECT")
  [ -n "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL" ] && daemon_args+=(-gcp-agent-service-account "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL")
  [ -n "$GRAIN_TARGET_REPO" ] && daemon_args+=(-default-target-repo "$GRAIN_TARGET_REPO")
  [ -n "$GRAIN_TARGET_REPOS" ] && daemon_args+=(-target-repos "$GRAIN_TARGET_REPOS")

  # -kontur-sandboxes is what actually selects orchestrator.
  # KonturSandboxes over HostSandboxes (cmd/grain/daemon.go's run()); the
  # VM name itself is not configurable, since a VM name has 11 bytes to
  # live in and a run id needs nine of them (orchestrator.VMNamePrefix) --
  # only ever passed once ensure_kontur_images/ensure_kontur_kvm_access
  # have both actually succeeded this run (each resets
  # GRAIN_KONTUR_ENABLE to 0 on its own failure), so a host that cannot
  # yet dispatch onto a real VM keeps dispatching into host directories
  # instead of installing a daemon that would fail every task. -backend
  # docker needs no konturctl setup/containerd/CNI/kubelet on this host
  # (bwsalmon/agents#353). -kontur-image names the sandbox container
  # ensure_kontur_oci_image just pulled, rather than leaving konturctl to
  # its own localhost:5000/kontur:latest default, which is what a locally
  # built image used to be retagged to (bwsalmon/agents#645: there is no
  # local build to retag any more, and naming the pulled reference says
  # out loud which sandbox this deployment runs).
  # No -disk/-kernel/-initramfs, and no -images-hostpath to resolve them
  # against: the guest travels inside -kontur-image, so konturctl boots
  # what that image carries. Those five flags, and the two host
  # directories behind them, were the whole apparatus for handing a VM a
  # disk this script had built or fetched.
  # -guest-port 22 is not optional: konturctl's own default is 80, which
  # silently refuses every connection to this image's actual sshd
  # (scripts/kontur/README.md, "guest-port 22 is not optional").
  # -disk-mode=overlay gives each VM a writable root that costs nothing
  # to create: the guest writes into a thin qcow2 inside its own
  # container, backed by the image's disk, which is only ever read
  # (bwsalmon/kontur#37). konturctl's default is readonly, which a
  # dispatched task cannot use.
  if [ "$GRAIN_KONTUR_ENABLE" = "1" ]; then
    daemon_args+=(
      -kontur-sandboxes
      -kontur-ssh-user "$GRAIN_KONTUR_SSH_USER"
      -kontur-workspace "$GRAIN_KONTUR_WORKSPACE"
      -kontur-net "$GRAIN_KONTUR_NET"
      -kontur-git-proxy-host "$GRAIN_KONTUR_GIT_PROXY_HOST"
      -kontur-create-arg -kontur-image -kontur-create-arg "$GRAIN_KONTUR_OCI_IMAGE"
      -kontur-create-arg -disk-mode=overlay
      # kontur authorizes this boot's generated key for root; the daemon
      # execs as GRAIN_KONTUR_SSH_USER, so that account has to be named
      # too. One flag rather than two settings: konturctl puts it on the
      # VM container as KONTUR_EXEC_USER, where `kontur run` reads it to
      # authorize the account and `kontur exec` reads it to log in as, so
      # the two cannot disagree.
      -kontur-create-arg -guest-user -kontur-create-arg "$GRAIN_KONTUR_SSH_USER"
    )
    # Only when this deployment named one: konturctl's own default is a
    # public resolver, and passing nothing is how a deployment says
    # "that one". See GRAIN_KONTUR_DNS above for why a guest cannot
    # simply use this host's resolver.
    if [ -n "$GRAIN_KONTUR_DNS" ]; then
      daemon_args+=(-kontur-create-arg -dns -kontur-create-arg "$GRAIN_KONTUR_DNS")
    fi
    # Addressing and the forwarded guest port are NAT-mode concerns: flat
    # mode takes its address from docker, and konturctl rejects "-ip"
    # outright under it.
    if [ "$GRAIN_KONTUR_NET" != "flat" ]; then
      daemon_args+=(
        -kontur-base-ip "$GRAIN_KONTUR_BASE_IP"
        -kontur-base-port "$GRAIN_KONTUR_BASE_PORT"
        -kontur-create-arg -guest-port -kontur-create-arg 22
      )
    fi
  fi

  docker_run_args

  # The unit runs `docker run` as root, and the container runs as
  # $GRAIN_USER: `docker run --user` is what makes the daemon
  # unprivileged, and the client needs to reach a root-owned socket to
  # ask for that. So the account this service *starts* as is root and the
  # account grain *is* is unchanged from before any of this ran in a
  # container -- no User= here, --user in docker_run_args instead.
  #
  # --rm plus an ExecStartPre `rm -f` is the pair that makes a restart
  # deterministic: the container is named (so ExecStop and an operator's
  # `docker logs grain-daemon` can find it), and a name outliving a hard
  # kill would otherwise make the next start fail with "name already in
  # use" forever. The `-` prefix makes that pre-step's failure -- the
  # ordinary case, where there is nothing to remove -- not a failure of
  # the unit.
  #
  # ExecStop stops the container rather than leaving systemd to kill the
  # client: `docker run` proxies signals, but a stop that goes through
  # the daemon is what actually gives the container its full
  # TimeoutStopSec to shut down instead of racing the client's death.
  #
  # Requires=docker.service, not merely After=: this unit is a docker
  # client, and a boot where the engine never came up should leave
  # grain-daemon failed and legible rather than restarting forever
  # against a missing socket.
  #
  # ${IMAGE_REF_FILE} carries one GRAIN_IMAGE=<ref> line and is the
  # indirection an upgrade writes (write_image_ref, pkg/upgrade/
  # image.go), which is why the image below is a variable and not a
  # literal: the unit never has to be rewritten to change what runs.
  cat > /etc/systemd/system/grain-daemon.service <<UNIT
[Unit]
Description=grain daemon (task orchestrator, UI and API), from ${GRAIN_IMAGE}
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
EnvironmentFile=${IMAGE_REF_FILE}
ExecStartPre=-$DOCKER_BIN rm -f grain-daemon
ExecStart=$DOCKER_BIN run --name grain-daemon ${DOCKER_ARGS[*]} \${GRAIN_IMAGE} ${daemon_args[*]}
ExecStop=-$DOCKER_BIN stop --time 30 grain-daemon
Restart=on-failure
RestartSec=5
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
UNIT

  write_control_units
  systemctl daemon-reload
}

# docker_run_args fills DOCKER_ARGS with everything grain-daemon.service
# hands `docker run` before the image name. It is one list, built here
# rather than written into the unit by hand, because every entry answers
# the same question: what does a process that used to be a plain systemd
# service on this host still need to see once it is in a container?
#
#   --network host       the UI binds -ui-addr and the git proxy binds an
#                        address every sandbox (a kontur VM in its own
#                        netns included) has to reach. A bridged network
#                        would put both behind a NAT that neither the
#                        load balancer in front of this host nor a
#                        sandbox VM knows how to cross.
#   --user               $GRAIN_USER's own uid:gid, so the store, the
#                        secrets database and every sandbox working tree
#                        come out owned exactly as they were before.
#   --cap-add NET_BIND_SERVICE
#                        only with a privileged -ui-addr port. The image
#                        gives the grain binary the matching file
#                        capability (Dockerfile), which is what turns
#                        a bounding-set entry into an actual grant for a
#                        non-root process -- and grants it to that binary
#                        alone, not to a task's own `bash -c`.
#   the data/sandbox/src mounts
#                        the three directories the daemon reads and
#                        writes, each at the very path it has out here so
#                        that a path in a log line, a flag or an error
#                        means the same thing in both places.
#   the journal mounts   pkg/systemlog.Journalctl shells out to
#                        journalctl for the UI's Logs pane; it needs the
#                        journal files and the machine-id that names
#                        them. Read-only, and whichever of the two
#                        journal directories this host actually uses
#                        (/var/log/journal when persistent storage is on,
#                        /run/log/journal when it is not). Paired with a
#                        --group-add of systemd-journal's GID -- the files
#                        just mounted in are group-readable by that group
#                        alone, and $GRAIN_USER's membership in it on the
#                        host (were it granted there, the way it used to
#                        be) would not reach a process the container only
#                        ever gave two bare uid:gid numbers.
#   the docker socket    kontur (konturctl, and pkg/mcp's docker-exec
#                        transport) and the Upgrade button's own `docker
#                        pull` both talk to this host's engine. Mounted
#                        only when one of those is actually turned on --
#                        this is the one entry here that grants the
#                        container root-equivalent authority over the
#                        host, so it is not given for free to a
#                        deployment that has no use for it.
#   the kontur mount     konturctl records its VMs in /var/lib/kontur,
#                        and the paths it then hands the host's docker
#                        daemon as bind mounts are those same host paths
#                        -- which is why it is mounted at its own path.
#                        The image and disk-overlay directories that used
#                        to be mounted beside it are gone: the guest
#                        travels inside the sandbox image and its overlay
#                        is created in the VM's own container.
#   GRAIN_CLAUDE_PATH/GRAIN_AGY_PATH/GRAIN_CODEX_PATH
#                        an operator's own agent CLI, mounted (with the
#                        directory around it, since a CLI is rarely one
#                        lone file) at the path they named, because a
#                        path passed as -claude-path/-agy-path has to
#                        resolve inside the container.
#   GRAIN_EXTRA_DOCKER_ARGS
#                        the escape hatch for whatever this list does not
#                        anticipate -- another mount, another device,
#                        another environment variable -- so that needing
#                        one does not mean editing this script.
docker_run_args() {
  local uid gid docker_gid journal_gid
  uid="$(id -u "$GRAIN_USER")"
  gid="$(id -g "$GRAIN_USER")"

  DOCKER_ARGS=(
    --rm
    --network host
    --user "${uid}:${gid}"
    --env "HOME=${GRAIN_DATA_DIR}/home"
    --volume "${GRAIN_DATA_DIR}:${GRAIN_DATA_DIR}"
    --volume "${GRAIN_SANDBOX_DIR}:${GRAIN_SANDBOX_DIR}"
  )

  case "${GRAIN_UI_ADDR##*:}" in
    ''|*[!0-9]*) ;;
    *) [ "${GRAIN_UI_ADDR##*:}" -lt 1024 ] && DOCKER_ARGS+=(--cap-add NET_BIND_SERVICE) ;;
  esac

  [ -f /etc/machine-id ] && DOCKER_ARGS+=(--volume /etc/machine-id:/etc/machine-id:ro)
  if [ -d /var/log/journal ] || [ -d /run/log/journal ]; then
    [ -d /var/log/journal ] && DOCKER_ARGS+=(--volume /var/log/journal:/var/log/journal:ro)
    [ -d /run/log/journal ] && DOCKER_ARGS+=(--volume /run/log/journal:/run/log/journal:ro)
    # The journal files this just mounted in are root:systemd-journal,
    # mode 0640 -- unreadable to the container's own $GRAIN_USER without
    # its numeric GID, the same --group-add docker_gid already does for
    # the docker socket a few lines down. Permission is a kernel-level
    # GID check, so it does not matter that nothing inside the image's
    # own /etc/group names this GID systemd-journal (or anything at all).
    journal_gid="$(getent group systemd-journal | cut -d: -f3)"
    [ -n "$journal_gid" ] && DOCKER_ARGS+=(--group-add "$journal_gid")
  fi

  if [ "$GRAIN_KONTUR_ENABLE" = "1" ] || [ "$GRAIN_ENABLE_UI_UPGRADE" = "1" ]; then
    DOCKER_ARGS+=(--volume /var/run/docker.sock:/var/run/docker.sock)
    # The socket is root:docker 0660, and the container is not root:
    # without the group, every docker call from in there is a permission
    # denied. --group-add is the container-side equivalent of the docker
    # group membership this used to grant $GRAIN_USER on the host.
    docker_gid="$(getent group docker | cut -d: -f3)"
    [ -n "$docker_gid" ] && DOCKER_ARGS+=(--group-add "$docker_gid")
  fi

  if [ "$GRAIN_KONTUR_ENABLE" = "1" ]; then
    DOCKER_ARGS+=(--volume /var/lib/kontur:/var/lib/kontur)
  fi

  [ -n "$GRAIN_CLAUDE_PATH" ] && DOCKER_ARGS+=(--volume "$(dirname "$GRAIN_CLAUDE_PATH"):$(dirname "$GRAIN_CLAUDE_PATH"):ro")
  [ -n "$GRAIN_AGY_PATH" ] && DOCKER_ARGS+=(--volume "$(dirname "$GRAIN_AGY_PATH"):$(dirname "$GRAIN_AGY_PATH"):ro")
  [ -n "$GRAIN_CODEX_PATH" ] && DOCKER_ARGS+=(--volume "$(dirname "$GRAIN_CODEX_PATH"):$(dirname "$GRAIN_CODEX_PATH"):ro")

  # Deliberately unquoted: this is a list of arguments an operator wrote,
  # not one argument.
  # shellcheck disable=SC2206
  [ -n "$GRAIN_EXTRA_DOCKER_ARGS" ] && DOCKER_ARGS+=($GRAIN_EXTRA_DOCKER_ARGS)
  return 0
}

enable_services() {
  # enable, then restart -- not "enable --now". An already-enabled unit
  # from a previous run of this script needs restarting to pick up a
  # newly pulled image or a config change; --now would leave an already-
  # running one exactly as it was. v1 hit precisely this bug for its own
  # git-proxy service (found live on v1, same failure): restarting
  # an already-running unit is always safe, and starting a stopped one is
  # exactly what --now would have done anyway.
  systemctl enable grain-daemon.service >/dev/null
  # The two watchers the daemon reaches the host through
  # (write_control_units). --now on these, unlike the service below: a
  # .path unit holds no state worth restarting, and it has to be actively
  # watching before the first request lands in $CONTROL_DIR.
  systemctl enable --now grain-reboot.path grain-restart.path >/dev/null
  # Started unconditionally, even with no agent credential anywhere. It
  # used to be held back until a Gemini key existed, because the daemon
  # built its one agent framework at startup and could not run without
  # one -- but a credential is set from the UI now, and a UI that is not
  # running is a credential that can never be set. The daemon builds a
  # framework per dispatch instead (cmd/grain's agentFrameworks), so a
  # deployment with no key serves the UI, says which keys are missing,
  # and fails any run that needs one with a message naming the pane to
  # fix it in.
  systemctl restart grain-daemon.service
  if [ ! -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ] \
     && [ ! -s "$GRAIN_DATA_DIR/secrets/claude-oauth-token" ] \
     && [ ! -s "$GRAIN_DATA_DIR/secrets/openai-api-key" ]; then
    log "grain-daemon.service is running, but no agent credential is configured -- no task can"
    log "  be dispatched until one is. Set it in the UI (Settings -> Agent frameworks), or set"
    log "  GRAIN_GEMINI_API_KEY / GRAIN_CLAUDE_CODE_OAUTH_TOKEN / GRAIN_OPENAI_API_KEY and"
    log "  re-run this script."
    log "  A deployment whose minter holds roles/serviceusage.apiKeysAdmin can mint the Gemini"
    log "  one instead: set GRAIN_GCP_PROJECT and GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE and re-run."
  fi
}

# report_readiness prints what this host can actually do, as opposed to
# what this script did. The two have come apart repeatedly: seeding a
# credential is seed-once, minting the Gemini key is deliberately
# non-fatal, and a missing GitHub credential only warns -- so a deploy
# can converge, report success, and leave a host that cannot clone a
# repo or will not start its daemon.
#
# Every line names the consequence, not just the state, because the
# consequence is what an operator would otherwise have to work backwards
# from: an agent run that ends without pushing or asking anything looks
# like a model problem long before it looks like an empty sandbox.
#
# Presence only, never values -- the same restriction `grain secrets
# list` holds to.
report_readiness() {
  local github="MISSING" gemini="MISSING" claude="MISSING" openai="MISSING" minter="MISSING" daemon ready=1
  # The binaries, not the tokens: they are independent, and a deployment
  # with a token set and no CLI is exactly the state that fails every run
  # of that framework with "executable file not found in $PATH". All are
  # reported, because which framework a run uses is a live per-task
  # choice -- a deployment is only really ready when every one can run.
  #
  # Asked of the *image*, not of this host: that is where they live now
  # (Dockerfile), so `command -v` out here would report a host that
  # has nothing to do with whether a dispatch can run. The exception is
  # an operator-named copy, which is a host path by definition --
  # docker_run_args mounts it in.
  local claude_cli agy_cli codex_cli
  claude_cli="$(report_agent_cli claude "$GRAIN_CLAUDE_PATH" GRAIN_CLAUDE_PATH)"
  agy_cli="$(report_agent_cli agy "$GRAIN_AGY_PATH" GRAIN_AGY_PATH)"
  codex_cli="$(report_agent_cli codex "$GRAIN_CODEX_PATH" GRAIN_CODEX_PATH)"

  # Either file shape counts, the same way CredentialSet.load reads
  # either: seed_github_app_credential above writes the .app.json one, so
  # asking only about .token reported a perfectly working App-backed
  # deployment as having no GitHub credential at all.
  if [ -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
     && { [ -s "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" ] \
          || [ -s "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.app.json" ]; }; then
    github="present as '${GRAIN_GITHUB_CREDENTIAL_NAME}'"
  elif [ -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
       && { ls "$GRAIN_DATA_DIR"/secrets/github/*.token >/dev/null 2>&1 \
            || ls "$GRAIN_DATA_DIR"/secrets/github/*.app.json >/dev/null 2>&1; }; then
    # And a credential added from the UI is named by whoever added it,
    # which has nothing to do with this script's own default name -- so
    # a deployment finished in the UI reports what it has rather than
    # reporting nothing (the same point the agent-key checks below make).
    github="present"
  fi
  [ -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ] && gemini="present"
  [ -s "$GRAIN_DATA_DIR/secrets/claude-oauth-token" ] && claude="present"
  [ -s "$GRAIN_DATA_DIR/secrets/openai-api-key" ] && openai="present"
  # A key set through the UI lands in the secrets database, not in either
  # file above, so presence has to be asked of both places -- otherwise a
  # deployment configured entirely from the UI reports every credential
  # missing while running perfectly well.
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null | grep -q '^gemini-api-key:'; then
    gemini="present"
  fi
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null | grep -q '^claude-oauth-token:'; then
    claude="present"
  fi
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null | grep -q '^openai-api-key:'; then
    openai="present"
  fi
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null \
     | grep -q '^gcp-key-minter:'; then
    minter="present"
  fi
  daemon="$(systemctl is-active grain-daemon.service 2>/dev/null || echo unknown)"

  # The secrets key, and where this deployment's state lives at all.
  #
  # Presence is asked of the file rather than of `grain state key show`,
  # which would *mint* a key on a host that has none (pkg/secrets.Open) --
  # a readiness report must not create what it is reporting on. The
  # public half is only asked for once the private half is known to be
  # there, which makes that call a read.
  local secrets_key="not yet" state_repo
  if [ -s "$GRAIN_DATA_DIR/secrets/secrets.key" ]; then
    secrets_key="present ($(/usr/local/bin/grain state -data-dir "$GRAIN_DATA_DIR" key show 2>/dev/null || echo 'public key unreadable'))"
  fi
  state_repo="$(state_repo_field remote)"

  echo
  log "Readiness:"
  echo "    daemon:            $daemon"
  echo "    image:             $GRAIN_IMAGE_REF"
  echo "    GitHub credential: $github"
  echo "    Gemini key:        $gemini"
  echo "    Claude token:      $claude"
  echo "    OpenAI key:        $openai"
  echo "    claude CLI:        $claude_cli"
  echo "    agy CLI:           $agy_cli"
  echo "    codex CLI:         $codex_cli"
  echo "    GCP minter key:    $minter"
  echo "    state repository:  ${state_repo:-<local only: a git repository on this host, pushed nowhere>}"
  echo "    secrets key:       $secrets_key"
  echo "    target repos:      ${GRAIN_TARGET_REPOS:-<none: unrestricted -- any repo a task names is allowed>}"
  echo "    default repo:      ${GRAIN_TARGET_REPO:-<none: a task with no repo parks>}"
  echo "    max workers:       ${GRAIN_MAX_WORKERS:-<default>}"
  echo "    max mergers:       ${GRAIN_MAX_MERGERS:-<default>}"
  if [ "$GRAIN_KONTUR_ENABLE" = "1" ]; then
    echo "    sandboxing:        kontur VMs (one per run, over SSH as ${GRAIN_KONTUR_SSH_USER})"
  else
    echo "    sandboxing:        host directories (orchestrator.HostSandboxes, inside the container)"
  fi

  if [ "$github" = "MISSING" ]; then
    ready=0
    echo "    !! With no GitHub credential the git proxy cannot clone. A dispatched run"
    echo "       finds an empty sandbox and ends without pushing or asking anything."
  fi
  if [ "$gemini" = "MISSING" ] && [ "$claude" = "MISSING" ] && [ "$openai" = "MISSING" ]; then
    ready=0
    echo "    !! With no agent credential set at all, the daemon runs and serves ${GRAIN_UI_ADDR},"
    echo "       but every dispatched run fails at setup. Set one in the UI (Settings ->"
    echo "       Agent frameworks) or re-run with GRAIN_GEMINI_API_KEY / "
    echo "       GRAIN_CLAUDE_CODE_OAUTH_TOKEN / GRAIN_OPENAI_API_KEY."
  fi
  # Not a readiness failure on its own: a deployment that only ever uses
  # one framework neither needs nor misses the other. Still worth a line
  # each, because nothing in the UI says a framework it offers cannot run
  # here.
  case "$claude_cli" in
    MISSING*)
      echo "    -- no claude CLI: the \"claude\" agent framework cannot run until there is one"
      echo "       (Settings -> Agent framework, and the per-task override, still offer it)."
      echo "       Deploy an image built with it, or point GRAIN_CLAUDE_PATH at a copy here."
      ;;
  esac
  case "$agy_cli" in
    MISSING*)
      echo "    -- no agy CLI: the \"antigravity\" agent framework cannot run until there is"
      echo "       one, and it is the default -- so a deployment in this state dispatches"
      echo "       nothing unless every task overrides the framework. Deploy an image built"
      echo "       with it, or point GRAIN_AGY_PATH at a copy here."
      ;;
  esac
  case "$codex_cli" in
    MISSING*)
      echo "    -- no codex CLI: the \"codex\" agent framework cannot run until there is one"
      echo "       (Settings -> Agent framework, and the per-task override, still offer it)."
      echo "       Deploy an image built with it, or point GRAIN_CODEX_PATH at a copy here."
      ;;
  esac
  # Never a readiness failure either way: a host with a key is working,
  # and a host without one yet gets one the moment the daemon starts.
  # Printed regardless, because this is where an operator is already
  # looking and the moment to copy that file somewhere is while it still
  # exists -- the deployment that needs it is the *next* one, and by then
  # a lost key is not recoverable from anything.
  if [ "$secrets_key" = "not yet" ]; then
    echo "    -- no secrets key on this host yet: grain-daemon.service mints one on its"
    echo "       first start. Back up $GRAIN_DATA_DIR/secrets/secrets.key once it exists"
    echo "       (\`grain state status\` prints it), and seed it on a rebuilt host with"
    echo "       GRAIN_SECRETS_KEY -- nothing else can decrypt this deployment's secrets\ file."
  else
    echo "    -- back up $GRAIN_DATA_DIR/secrets/secrets.key: the one file here a redeploy"
    echo "       cannot rebuild, and the only thing that can read $GRAIN_DATA_DIR/secrets/secrets.enc."
    echo "       A host restored with a freshly minted key of its own cannot read a line"
    echo "       of it. Seed it back with GRAIN_SECRETS_KEY (see this script's own -h)."
  fi
  if [ "$minter" = "MISSING" ] && [ -n "$GRAIN_GCP_PROJECT" ]; then
    ready=0
    echo "    !! With no minter credential the gcp-key and gemini-key capabilities cannot"
    echo "       mint, so a task granted either will fail to materialize it."
  fi
  if [ "$daemon" != "active" ]; then
    ready=0
    echo "    !! grain-daemon.service is $daemon -- see: journalctl -u grain-daemon -n 50"
  fi
  if [ "$GRAIN_KONTUR_REQUESTED" = "1" ] && [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    ready=0
    echo "    !! GRAIN_KONTUR_ENABLE=1 was requested but a prerequisite wasn't ready this run"
    echo "       (see the earlier log line naming which one) -- dispatching into host"
    echo "       directories instead. Re-run once it is; nothing else needs to change."
  fi
  [ "$ready" -eq 1 ] && echo "    all runtime prerequisites are in place"
  return 0
}

print_summary() {
  echo
  log "Done."
  echo "    UI:      http://${GRAIN_UI_ADDR} -- reach it with: ssh -L 8080:localhost:${GRAIN_UI_ADDR##*:} <this-host>, then open http://localhost:8080"
  echo "    Store:   embedded SQLite under ${GRAIN_DATA_DIR}/store, owned by grain-daemon.service alone"
  echo "    Secrets: ${GRAIN_DATA_DIR}/secrets"
  echo "    State:   ${GRAIN_DATA_DIR}/state-repo -- the same rows as text, in git (grain state status)"
  echo "    CLI:     grain list  (a new shell picks up GRAIN_SERVER from /etc/profile.d/grain.sh;"
  echo "             in this one: export GRAIN_SERVER=http://127.0.0.1:${GRAIN_UI_ADDR##*:})"
  echo "    Image:   ${GRAIN_IMAGE_REF} (recorded in ${IMAGE_REF_FILE}; \`docker images\` for what is local)"
  echo "    Logs:    journalctl -u grain-daemon.service -f  (or: docker logs -f grain-daemon)"
  echo "    Update:  re-run this script (sudo ./setup.sh) -- it pulls the image for GRAIN_REF and restarts"
  echo "             the service. Pin or roll back with GRAIN_IMAGE_TAG=sha-<short sha>."
  report_readiness
}

main() {
  # pull_image comes before everything that runs image_run (the kontur
  # steps, the empty-repo check) and before install_cli_wrappers, which
  # writes wrappers around the same image: with no checkout and no host
  # tooling left, this image is where the rest of this run gets both its
  # `grain` CLI and every tool richer than the shell.
  ensure_user
  grant_docker_group
  pull_image
  # After ensure_user: the wrappers run the image as that account, so it
  # has to exist to have a uid to run as.
  install_cli_wrappers
  setup_sandbox_dir
  # Ahead of every step below that runs the `grain` CLI: that CLI is a
  # `docker run` with $GRAIN_DATA_DIR bind-mounted (install_cli_wrappers),
  # and docker creates a missing bind-mount source itself -- as root,
  # with a mode nothing here asked for. Laying the directory out first
  # means the first CLI invocation mounts a directory this script already
  # owns rather than one docker invented.
  setup_data_dir
  ensure_kontur_images
  ensure_kontur_kvm_access
  ensure_kontur_git_proxy_host
  reformat_store_if_schema_changed
  format_target_repo_if_empty
  # After setup_data_dir: this writes into $GRAIN_DATA_DIR, and it has to
  # be in place before write_systemd_units' unit reads it.
  write_image_ref
  write_systemd_units
  enable_services
  print_summary
}

main "$@"
