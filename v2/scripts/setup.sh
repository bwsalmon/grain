#!/usr/bin/env bash
# Installer and updater for a v2 grain deployment, run directly on the
# target machine -- bwsalmon/agents#355.
#
# v1's shape (../../provision/controller.sh, ../../docs/design.md) is a
# controller VM plus a pool of sandbox VMs, all built by a Python host
# adapter (../../grain/adapter/) driving libvirt. v2 has no host adapter
# yet (v2/README.md, "What this does not have yet") and does not need
# one to be useful: its daemon already defaults to running dispatched
# work as plain host directories (orchestrator.HostSandboxes), no VM
# involved. So this script does the simpler thing the issue actually
# asks for -- run the one `grain` binary directly on this machine, as a
# single systemd service, with no controller VM anywhere in the picture.
# Real sandbox isolation (a VM or container per task) is still open and
# out of scope here; see v2/README.md's own "neither sandbox stand-in
# carries any real isolation."
#
# What this script does, every time it runs (safe to re-run -- this is
# the installer AND the updater):
#   1. clones or updates this repo under $GRAIN_SRC_DIR -- and, if that
#      update replaced this script itself, re-runs the new copy in place
#      of this one (reexec_if_updated), so a run always deploys with the
#      code it just pulled rather than the code it started with. The
#      checkout is no longer what grain is built from (see 2), but it is
#      still what this script, packer/kontur's own image builds and the
#      self-debug capability's read of grain's own source all come from
#   2. pulls the deployment image -- $GRAIN_IMAGE:$GRAIN_IMAGE_TAG,
#      published to GHCR by ../../.github/workflows/build-artifacts.yml
#      on every commit -- instead of building a binary here
#      (bwsalmon/agents#645). That image carries grain *and* every binary
#      it shells out to: git, the docker CLI, konturctl and the claude
#      CLI (v2/Dockerfile). What this host has to have shrinks to `git`
#      and `docker`, which this script still installs itself if a vanilla
#      Debian VM doesn't have them (ensure_git, ensure_docker;
#      bwsalmon/agents#617) -- no `make`, no Go or Node toolchain, no
#      per-host claude install, and no minutes-long build on every deploy
#   3. installs /usr/local/bin/grain and /usr/local/bin/konturctl as thin
#      wrappers that run that same image (install_cli_wrappers), so an
#      operator's own shell -- and the rest of this script, which uses
#      `grain schema-version` and `grain secrets` -- reaches the exact
#      build the service runs, with nothing installed on the host to
#      drift out of step with it
#   4. creates an unprivileged system user to run the container as, and
#      installs the two systemd path units that let it act on the host it
#      cannot reach from inside a container: the UI's reboot-host button
#      (bwsalmon/agents#395) and the restart its Upgrade button needs
#      (bwsalmon/agents#396) each become a touch of a file under
#      $GRAIN_DATA_DIR/control, watched by a unit out here
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
#      docs/next-session.md item 3's "Update" for why
#      enable-without-restart was already a bug once in v1's own proxy
#      service
#
# Every setting is an environment variable, not a flag, so the common
# case is `sudo GRAIN_GITHUB_TOKEN=... GRAIN_GEMINI_API_KEY=... ./setup.sh`
# and a re-run to pick up a repo update is `sudo ./setup.sh` with no
# arguments at all. Run with -h/--help for the full list.
#
# Most daemon settings below (everything except GRAIN_UI_ADDR,
# GRAIN_TARGET_REPO/GRAIN_TARGET_BRANCH and GRAIN_ENABLE_UI_UPGRADE) are
# only *seeded* from these variables, the first time a deployment's store
# has none (cmd/grain/daemon.go's loadConfig, bwsalmon/agents#320) --
# passing this script a new GRAIN_GITHUB_HOST or GRAIN_MAX_CONCURRENT on
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
# v2/cmd/grain/daemon.go's own doc comment) -- so this script still
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
# "single-operator tool" framing (v2/README.md, "The UI") for why that is
# the whole access-control story today -- the API and the UI it serves
# carry no auth of their own, so whatever reaches -ui-addr can act as the
# deployment's one configured actor.

set -euo pipefail

# --- configuration (every value overridable via environment) ----------

GRAIN_REPO_URL="${GRAIN_REPO_URL:-https://github.com/bwsalmon/grain.git}"
GRAIN_REF="${GRAIN_REF:-main}"
GRAIN_SRC_DIR="${GRAIN_SRC_DIR:-/opt/grain}"
GRAIN_DATA_DIR="${GRAIN_DATA_DIR:-/var/lib/grain}"
# Root for HostSandboxes' per-slot working directories -- only used
# without kontur sandboxing (GRAIN_KONTUR_ENABLE=0). Deliberately not
# under $GRAIN_DATA_DIR (bwsalmon/agents#587, see this file's own header
# comment, item 5): unlike GRAIN_DATA_DIR, nothing under here needs to
# survive a redeploy, so its default lives outside whatever separate,
# persistent disk an operator mounts at $GRAIN_DATA_DIR (terraform/gcp-v2's
# own data_disk_gb) -- the same reasoning GRAIN_KONTUR_IMAGES_HOSTPATH and
# GRAIN_KONTUR_DISK_HOSTPATH below already follow for kontur's own state.
GRAIN_SANDBOX_DIR="${GRAIN_SANDBOX_DIR:-/var/lib/grain-sandbox}"
GRAIN_USER="${GRAIN_USER:-grain}"

# --- the deployment image (bwsalmon/agents#645) ------------------------
#
# GRAIN_IMAGE is the repository CI publishes to on every commit, with no
# tag: ../../.github/workflows/build-artifacts.yml's own grain-container
# job. GRAIN_IMAGE_TAG picks which one to run, and defaults to the tag
# published for GRAIN_REF -- the branch this checkout tracks -- so a
# deployment pinned to a branch stays pinned to that branch's image with
# nothing extra to set. '/' is not legal in a docker tag and grain's
# branches routinely contain one, so it becomes '-', the same
# substitution CI makes when it pushes and v2/pkg/upgrade's TagForBranch
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
GRAIN_MAX_CONCURRENT="${GRAIN_MAX_CONCURRENT:-1}"
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
# Where the Antigravity CLI (agy) lives *on this host*. Unlike the claude
# CLI, agy is not in the deployment image at all -- this repo has no
# verified installer URL for it (v2/README.md, "The agent runtime is a
# CLI now"), so there is nothing to bake in -- which makes this path the
# only way the default agent framework ever gets a binary to run: what it
# names is bind-mounted into the container at the same path and passed as
# -agy-path. Empty falls back to resolving "agy" inside the container,
# which finds nothing unless an image was built with one.
#
# ~/.gemini/bin/agy is where its own installer puts it. verify_agent_cli
# below says all this out loud, non-fatally, rather than letting the
# daemon fail at its first dispatch.
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
# wants -- resolves "claude" inside the container, where v2/Dockerfile
# already installed it: nothing to download at deploy time and nothing
# per-host to keep in step with the build.
#
# Set, this host's copy is bind-mounted into the container at that same
# path and the daemon is pointed at it (-claude-path). That is the escape
# hatch for a deployment that must pin a particular CLI version, and the
# reason it is a *mount* rather than a $PATH entry: a path that resolves
# out here has to resolve identically in there.
GRAIN_CLAUDE_PATH="${GRAIN_CLAUDE_PATH:-}"

GRAIN_GCP_PROJECT="${GRAIN_GCP_PROJECT:-}"
GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="${GRAIN_GCP_SERVICE_ACCOUNT_EMAIL:-}"
GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="${GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE:-}"

GRAIN_TARGET_REPO="${GRAIN_TARGET_REPO:-}"
GRAIN_TARGET_BRANCH="${GRAIN_TARGET_BRANCH:-main}"
GRAIN_TARGET_REPOS="${GRAIN_TARGET_REPOS:-}"

# See "Kontur sandboxing" below (ensure_kontur_ssh_key/ensure_kontur_images/
# ensure_kontur_kvm_access) and terraform/gcp-v2/README.md's
# own section of the same name. GRAIN_KONTUR_ENABLE=1 (off by default here
# -- terraform/gcp-v2's own enable_kontur_sandboxes variable is what
# actually turns this on for that deployment shape) needs no manual
# build-and-publish step first (bwsalmon/agents#531): with
# GRAIN_KONTUR_IMAGE_BUCKET/
# GRAIN_KONTUR_OCI_IMAGE left empty (the default), this script builds its
# own guest image and OCI image, and generates its own SSH keypair for
# the former if none is already provided or seeded. Set both of those two
# together instead to fetch a guest/OCI image pair built and published
# centrally, elsewhere, the way every deployment before bwsalmon/agents#531
# required; GRAIN_KONTUR_SSH_KEY_FILE similarly overrides the generated
# keypair with a specific one of an operator's own choosing.
GRAIN_KONTUR_ENABLE="${GRAIN_KONTUR_ENABLE:-0}"
# Remembers what was actually asked for, since ensure_kontur_images/
# ensure_kontur_kvm_access/seed_kontur_ssh_key overwrite GRAIN_KONTUR_ENABLE
# itself back to 0 on a failure partway through -- report_readiness uses
# this to tell "kontur was never requested" apart from "kontur was
# requested but a prerequisite wasn't ready this run".
GRAIN_KONTUR_REQUESTED="$GRAIN_KONTUR_ENABLE"
GRAIN_KONTUR_IMAGE_BUCKET="${GRAIN_KONTUR_IMAGE_BUCKET:-}"
GRAIN_KONTUR_OCI_IMAGE="${GRAIN_KONTUR_OCI_IMAGE:-}"
GRAIN_KONTUR_IMAGES_HOSTPATH="${GRAIN_KONTUR_IMAGES_HOSTPATH:-/var/lib/vm-images}"
# Filename the kontur SSH private key is staged under inside
# GRAIN_KONTUR_IMAGES_HOSTPATH (see ensure_kontur_exec_key), and so the
# basename of the /images path -kontur-exec-key points the daemon at.
GRAIN_KONTUR_EXEC_KEY_NAME="${GRAIN_KONTUR_EXEC_KEY_NAME:-kontur-exec-key}"
# -images-hostpath is always mounted read-only (it's a shared, node-local
# image cache several VMs may read from concurrently -- see
# third_party/kontur/README.md, "Operating a node (konturctl CLI)"), so a
# VM's own writable root filesystem instead lives here: a per-VM qcow2 overlay
# konturctl creates under -disk-hostpath, backed by the shared read-only
# disk image (bwsalmon/agents#510). Must be owned by $GRAIN_USER --
# ensure_kontur_kvm_access creates it -- since konturctl runs directly as
# that user, not inside a container the way -disk itself is read.
GRAIN_KONTUR_DISK_HOSTPATH="${GRAIN_KONTUR_DISK_HOSTPATH:-/var/lib/kontur/vm-disks}"
GRAIN_KONTUR_SSH_USER="${GRAIN_KONTUR_SSH_USER:-debian}"
GRAIN_KONTUR_SSH_KEY_FILE="${GRAIN_KONTUR_SSH_KEY_FILE:-}"
GRAIN_KONTUR_WORKSPACE="${GRAIN_KONTUR_WORKSPACE:-/home/debian}"
# flat: the guest is spliced onto its sandbox container's own segment and
# takes over the address docker assigned it, so nothing here has to assign
# one. nat is kontur's original mode, where each VM needs its own address
# on a shared private bridge and its own forwarded port -- which is all
# GRAIN_KONTUR_BASE_IP/GRAIN_KONTUR_BASE_PORT below exist to derive, and
# which flat mode ignores. Flat mode needs a guest image carrying kontur's
# own guest overlays (packer/kontur/build-guest.sh builds one); a
# deployment pulling a prebuilt guest from GRAIN_KONTUR_IMAGE_BUCKET must
# republish it from that build before switching.
GRAIN_KONTUR_NET="${GRAIN_KONTUR_NET:-flat}"
GRAIN_KONTUR_BASE_IP="${GRAIN_KONTUR_BASE_IP:-169.254.100.10}"
GRAIN_KONTUR_BASE_PORT="${GRAIN_KONTUR_BASE_PORT:-12000}"
GRAIN_KONTUR_GIT_PROXY_HOST="${GRAIN_KONTUR_GIT_PROXY_HOST:-}"

# On by default -- the common case is a single, directly-managed host
# with no rollout mechanism of its own, where the UI's Upgrade button
# (bwsalmon/agents#396) is the only way to ship a new build. Set to 0 by
# terraform/gcp-v2/files/deploy.sh's own invocation of this script: that
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

  GRAIN_REPO_URL           git remote to deploy from (default: bwsalmon/grain on GitHub)
  GRAIN_REF                branch to deploy (default: main) -- names both the
                             checkout this script keeps and, via GRAIN_IMAGE_TAG,
                             which published image it runs
  GRAIN_SRC_DIR             where the checkout lives (default: /opt/grain)
  GRAIN_DATA_DIR            secrets/store root -- state that must survive a redeploy
                             (default: /var/lib/grain)
  GRAIN_SANDBOX_DIR         HostSandboxes' per-slot working directory root, only used
                             without kontur sandboxing -- state that a redeploy is free
                             to discard (default: /var/lib/grain-sandbox)
  GRAIN_USER                unprivileged account grain runs as (default: grain)

  GRAIN_UI_ADDR             UI/API bind address (default: 127.0.0.1:80 -- loopback
                             only; reach it with `ssh -L 8080:localhost:80 host`,
                             or put it behind Tailscale/IAP instead)
  GRAIN_MAX_CONCURRENT      maximum number of tasks dispatched at once (default: 1).
                             Seeded once, like every setting below marked the same
                             way -- see this file's own header comment
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
                             API -- see terraform/gcp-v2/README.md, "There is
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
  GRAIN_AGY_PATH            path on THIS HOST to the Antigravity CLI (agy) the
                             default agent framework runs as a subprocess.
                             Bind-mounted in the same way -- and, unlike claude,
                             the only way to have one at all: agy is not in the
                             image (no installer to bake in)
  GRAIN_GEMINI_MODEL        override the daemon's default Gemini model. Seeded once
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
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE a minter key to seed under secrets/gcp-key-minter/

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
                             rollout mechanism (e.g. terraform/gcp-v2's
                             config-sync.sh/deploy.sh), so the two cannot
                             race or drift out of sync with each other

  GRAIN_KONTUR_ENABLE        1 to dispatch onto real bwsalmon/kontur-managed
                             VMs over SSH (orchestrator.KonturSandboxes)
                             instead of host directories (default: 0). Builds
                             its own guest image and OCI image right here on
                             first use (ensure_kontur_images, below -- see
                             packer/kontur/README.md for what that actually
                             runs) unless GRAIN_KONTUR_IMAGE_BUCKET/
                             GRAIN_KONTUR_OCI_IMAGE are both set, and needs
                             /dev/kvm on this host (nested virtualization)
                             either way. Left off (with a logged reason) if
                             any prerequisite below is missing, rather than
                             failing the whole run.
  GRAIN_KONTUR_IMAGE_BUCKET  set together with GRAIN_KONTUR_OCI_IMAGE below to
                             fetch a guest/OCI image pair someone already built
                             and published centrally (packer/kontur/build-guest.sh's
                             own KONTUR_IMAGE_BUCKET; this script fetches its
                             "latest" alias) instead of building one locally.
                             Leave both empty (the default) to build locally.
  GRAIN_KONTUR_OCI_IMAGE     the other half of GRAIN_KONTUR_IMAGE_BUCKET above:
                             full reference of a pre-built kontur OCI image
                             (third_party/kontur's own Dockerfile), pulled and
                             retagged to konturctl's own default image. Leave
                             empty (the default) to build one locally instead.
  GRAIN_KONTUR_IMAGES_HOSTPATH  where the guest image (fetched, or built
                             locally and cached by a hash of what defines its
                             contents -- see kontur_image_tag) lands, bind-
                             mounted read-only into each VM's container
                             (default: /var/lib/vm-images, konturctl's own default)
  GRAIN_KONTUR_DISK_HOSTPATH  host directory each kontur VM's own private,
                             writable qcow2 disk overlay is created under
                             (default: /var/lib/kontur/vm-disks, konturctl's
                             own default) -- without this, a VM's root
                             filesystem is read-only, since
                             GRAIN_KONTUR_IMAGES_HOSTPATH always is
  GRAIN_KONTUR_SSH_USER      username to SSH into each kontur VM as (default: debian)
  GRAIN_KONTUR_SSH_KEY_FILE  path to the private half of a specific SSH keypair
                             to bake the public half of into the guest image,
                             for an operator who wants one keypair reused
                             across a fleet (push-secrets.sh's own
                             GRAIN_KONTUR_SSH_KEY does exactly that). Optional:
                             with none given, and none already seeded,
                             ensure_kontur_ssh_key generates one itself; either
                             way it ends up seeded once into
                             $GRAIN_DATA_DIR/secrets/kontur-ssh-key
  GRAIN_KONTUR_WORKSPACE     working directory tools operate in on each kontur
                             VM (default: /home/debian, GRAIN_KONTUR_SSH_USER's own home)
  GRAIN_KONTUR_NET           kontur networking mode: "flat" (default) or "nat".
                             Flat needs a guest built by build-guest.sh; see
                             packer/kontur/README.md.
  GRAIN_KONTUR_BASE_IP       "-ip" slot 1's kontur VM gets; every later slot's
                             (nat mode only -- ignored under flat)
                             is the next address after it (default: 169.254.100.10)
  GRAIN_KONTUR_BASE_PORT     "-port" slot 1's kontur VM forwards; every later
                             slot's is this plus its own number minus one
                             (default: 12000)
  GRAIN_KONTUR_GIT_PROXY_HOST  host (no port) a kontur VM reaches this
                             daemon's own git proxy through, in place of the
                             loopback address it otherwise binds to -- a
                             kontur VM's guest has its own unrelated
                             127.0.0.1, with no route to this host's
                             (bwsalmon/agents#567). Defaults to docker's own
                             "bridge" network gateway address, detected via
                             `docker network inspect bridge` or, failing
                             that, the bridge device's own address; set
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

# Taken here, before sync_repo below can replace the file underneath this
# running process -- see reexec_if_updated for what it is compared
# against and why.
SELF_PATH="$(readlink -f "$0" 2>/dev/null || echo "$0")"
SELF_SUM_BEFORE="$(sha256sum "$SELF_PATH" 2>/dev/null | awk '{print $1}' || true)"

for cmd in systemctl install useradd; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "setup.sh: required command not found: $cmd" >&2
    exit 1
  fi
done

# git and docker are installed rather than only reported missing --
# bwsalmon/agents#617. Until then both were only ever guaranteed by terraform/gcp-v2/files/deploy.sh's own
# install_prerequisites, which runs *before* this script but is no help
# to anyone who reaches this script the way its own header comment says
# it should be reachable: cloning this repo onto a bare Debian VM and
# running it directly, no Terraform or GCP metadata involved. A vanilla
# Debian cloud image carries neither.
ensure_git() {
  command -v git >/dev/null 2>&1 && return 0
  if command -v apt-get >/dev/null 2>&1; then
    log "installing git (needed to clone/update $GRAIN_SRC_DIR)"
    apt-get update -qq || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git ca-certificates || true
  fi
  if ! command -v git >/dev/null 2>&1; then
    echo "setup.sh: required command not found: git, and it could not be installed automatically -- install it (e.g. 'apt-get install git') and re-run" >&2
    exit 1
  fi
}
ensure_git

# Installs the docker.io package if the CLI is missing, then makes sure
# the daemon is actually up -- a fresh install's postinst usually starts
# it already, but this does not rely on that. The `docker info` check a
# few lines down is what actually gates the rest of the script; this is
# just the one attempt to make that check pass on its own rather than
# hand the operator a cryptic failure for a one-line fix.
#
# docker is now what grain *runs in*, not merely what it was once built
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

# --- 1. clone or update the checkout -----------------------------------

sync_repo() {
  # git 2.35.2+ refuses to operate on a repository it does not own
  # ("detected dubious ownership in repository at ..."), which this
  # checkout can easily be: it is root-owned here, and a previous
  # deployment's chown may have left it owned by $GRAIN_USER instead. A
  # git command that fails closed is worse than it sounds --
  # kontur_image_tag, later in main(), redirects git's stderr to
  # /dev/null and falls back to the literal string "unknown", which
  # silently breaks the content-hash caching that tag exists for: a
  # packer/kontur edit or third_party/kontur vendor bump would stop
  # changing the tag at all, so ensure_kontur_images_build
  # would keep reusing whatever it built the first time, forever. The
  # same failure hits this function's own git calls below whenever the
  # checkout is owned by anyone but root. Exempting it here,
  # before anything else touches the checkout, covers every git
  # invocation for the rest of this run and every run after it -- a
  # global config entry, so guarded against piling up duplicates across
  # re-runs.
  git config --global --get-all safe.directory 2>/dev/null | grep -qxF "$GRAIN_SRC_DIR" \
    || git config --global --add safe.directory "$GRAIN_SRC_DIR"

  if [ -d "$GRAIN_SRC_DIR/.git" ]; then
    log "Updating checkout at $GRAIN_SRC_DIR ($GRAIN_REF)"
    if ! git -C "$GRAIN_SRC_DIR" diff --quiet || ! git -C "$GRAIN_SRC_DIR" diff --cached --quiet; then
      echo "setup.sh: $GRAIN_SRC_DIR has uncommitted changes -- refusing to overwrite them. Commit, stash, or remove them and re-run." >&2
      exit 1
    fi
    git -C "$GRAIN_SRC_DIR" fetch --quiet origin "$GRAIN_REF"
    git -C "$GRAIN_SRC_DIR" checkout --quiet "$GRAIN_REF"
    git -C "$GRAIN_SRC_DIR" reset --quiet --hard "origin/$GRAIN_REF"
  else
    log "Cloning $GRAIN_REPO_URL ($GRAIN_REF) into $GRAIN_SRC_DIR"
    # mkdir -p, not `install -d`: the parent (e.g. /opt) usually already
    # exists, and `install -d` unconditionally applies its -m mode even
    # to a directory that was already there -- found live, against /tmp,
    # while testing this script: it silently stripped /tmp's sticky bit.
    # mkdir -p never touches a directory that already exists.
    mkdir -p "$(dirname "$GRAIN_SRC_DIR")"
    git clone --quiet --branch "$GRAIN_REF" "$GRAIN_REPO_URL" "$GRAIN_SRC_DIR"
  fi
}

# sync_repo updates the checkout this script itself lives in, and git
# swaps the file for a new one rather than rewriting it in place -- so
# this process goes on reading the copy it started with. Every step below
# is therefore the *old* script's version of that step, and a fix to this
# file only takes effect on the run after the one that pulled it.
#
# That is not hypothetical: it is how the deploy that pulled ff6e818
# ("Install make on the host, and check for it") still died on a bare
# `make: command not found`, with the check that exists to name that
# failure sitting unread on disk a few inches away.
#
# So: if the file at $0 is not the one this process is running, hand over
# to it. Only ever once -- $GRAIN_SETUP_REEXECED is exported across the
# exec, so the new copy runs its own sync_repo (a no-op by then, the
# checkout is already at $GRAIN_REF) and carries on rather than looking
# for a third. A setup.sh run from a copy outside the checkout is
# untouched by sync_repo, so its checksum does not change and this does
# nothing at all.
reexec_if_updated() {
  [ -z "${GRAIN_SETUP_REEXECED:-}" ] || return 0
  [ -n "$SELF_SUM_BEFORE" ] || return 0
  local now=""
  now="$(sha256sum "$SELF_PATH" 2>/dev/null | awk '{print $1}' || true)"
  [ -n "$now" ] || return 0
  [ "$now" != "$SELF_SUM_BEFORE" ] || return 0
  log "$SELF_PATH changed in the update just pulled; re-running it"
  export GRAIN_SETUP_REEXECED=1
  exec "$SELF_PATH" "$@"
}

# --- 2/3. pull the deployment image, and install the CLI wrappers -------
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
# v2/pkg/upgrade/image.go).
GRAIN_IMAGE_REF="${GRAIN_IMAGE}:${GRAIN_IMAGE_TAG}"
IMAGE_REF_FILE="$GRAIN_DATA_DIR/image.env"

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
# v2/scripts/kontur-diag.sh's `konturctl vm list` both reach exactly the
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
# Written by v2/scripts/setup.sh (install_cli_wrappers). Runs one command
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
  --volume ${GRAIN_DATA_DIR}:${GRAIN_DATA_DIR}
)
[ -d "${GRAIN_SANDBOX_DIR}" ] && args+=(--volume ${GRAIN_SANDBOX_DIR}:${GRAIN_SANDBOX_DIR})
[ -d "${GRAIN_SRC_DIR}" ] && args+=(--volume ${GRAIN_SRC_DIR}:${GRAIN_SRC_DIR}:ro)
# konturctl talks to this host's docker daemon and keeps its VM records
# out here; the image paths it hands docker are host paths, so each has
# to be mounted at the very path it already has.
if [ -S /var/run/docker.sock ]; then
  args+=(--volume /var/run/docker.sock:/var/run/docker.sock)
  docker_gid="\$(getent group docker | cut -d: -f3)"
  [ -n "\$docker_gid" ] && args+=(--group-add "\$docker_gid")
fi
[ -d /var/lib/kontur ] && args+=(--volume /var/lib/kontur:/var/lib/kontur)
[ -d "${GRAIN_KONTUR_IMAGES_HOSTPATH}" ] && args+=(--volume ${GRAIN_KONTUR_IMAGES_HOSTPATH}:${GRAIN_KONTUR_IMAGES_HOSTPATH}:ro)
[ -d "${GRAIN_KONTUR_DISK_HOSTPATH}" ] && args+=(--volume ${GRAIN_KONTUR_DISK_HOSTPATH}:${GRAIN_KONTUR_DISK_HOSTPATH})

exec docker run "\${args[@]}" "\${extra[@]}" "\$image" "\$@"
WRAPPER
  chmod 0755 /usr/local/lib/grain/run-image.sh

  cat > /usr/local/bin/grain <<'CLI'
#!/usr/bin/env bash
# Written by v2/scripts/setup.sh: the grain CLI, out of the deployment
# image. `grain <args>` reaches the image's own entrypoint.
exec /usr/local/lib/grain/run-image.sh -- "$@"
CLI
  chmod 0755 /usr/local/bin/grain

  cat > /usr/local/bin/konturctl <<'CLI'
#!/usr/bin/env bash
# Written by v2/scripts/setup.sh: konturctl, out of the deployment image
# -- the same copy pkg/kontur runs inside grain-daemon.service's own
# container, so a diagnostic run out here (v2/scripts/kontur-diag.sh)
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
# deliberately the same file v2/pkg/upgrade's image path writes, so the
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
# daemon.
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
write_cli_profile() {
  local port="${GRAIN_UI_ADDR##*:}"
  if [ -z "$port" ] || [ "$port" = "$GRAIN_UI_ADDR" ]; then
    log "  GRAIN_UI_ADDR ($GRAIN_UI_ADDR) has no port; not writing /etc/profile.d/grain.sh"
    return 0
  fi
  cat > /etc/profile.d/grain.sh <<PROFILE
# Written by v2/scripts/setup.sh. Points the grain CLI at the daemon this
# host runs, whose port comes from the -ui-addr it was started with.
# An explicit -server flag still overrides this.
export GRAIN_SERVER="http://127.0.0.1:${port}"
PROFILE
  chmod 0644 /etc/profile.d/grain.sh
  log "  grain CLI on this host defaults to http://127.0.0.1:${port} (/etc/profile.d/grain.sh)"
}

# --- 4. the unprivileged account grain runs as --------------------------

# ensure_user creates $GRAIN_USER and, on every run, makes sure it is in
# the systemd-journal group -- without it, journalctl refuses outright
# ("No journal files were opened due to insufficient permission") when
# the UI's own Logs page (pkg/ui/logs.go, pkg/systemlog.Journalctl) shells
# out to read grain-daemon.service's own journal, which normally takes
# membership in adm or systemd-journal. systemd-journal is the narrower
# of the two -- read access to the journal alone, nothing else adm also
# carries -- and every distribution this script otherwise assumes (a
# working systemctl/journalctl) already creates that group by default, so
# no getent guard is needed the way grant_docker_group's own one is.
ensure_user() {
  if ! id -u "$GRAIN_USER" >/dev/null 2>&1; then
    log "Creating system user $GRAIN_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$GRAIN_USER"
  fi
  usermod -aG systemd-journal "$GRAIN_USER"
}

# --- 5. the control channel: acting on the host from inside the container -
#
# Two UI buttons ask grain to do something to the machine it runs on:
# "reboot host" (v2/pkg/ui/host.go, bwsalmon/agents#395) and the restart
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

# ensure_src_dir_readable keeps $GRAIN_SRC_DIR readable by $GRAIN_USER.
#
# The checkout is no longer something the daemon writes to -- it is
# mounted read-only into the container, and nothing in there builds
# (bwsalmon/agents#645) -- but it is still what the self-debug capability
# reads grain's own source out of, and sync_repo runs as root, so a
# world-unreadable umask on this host would otherwise leave the daemon
# looking at a tree it cannot open.
ensure_src_dir_readable() {
  chmod -R a+rX "$GRAIN_SRC_DIR" 2>/dev/null || true
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
# enough to read the guest image out of GCS (gcs_fetch) and to
# authenticate docker to Artifact Registry (ensure_kontur_images_fetch)
# without installing the whole gcloud SDK just for those two things.
# iam.tf's own host_reads_kontur_images/host_reads_kontur_registry are
# what make the token itself actually able to do either. Only used by
# ensure_kontur_images_fetch -- the local build path needs no GCP
# credential of its own at all.
kontur_gcp_access_token() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
    | python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])'
}

# gcs_fetch downloads gs://$1/$2 to file $3 using kontur_gcp_access_token,
# the GCS JSON API's own object-download endpoint (the "alt=media" query
# parameter) rather than gsutil -- see kontur_gcp_access_token's own
# comment on why.
gcs_fetch() {
  local bucket="$1" object="$2" dest="$3" encoded_object
  encoded_object="$(python3 -c 'import urllib.parse, sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$object")"
  curl -fsS -H "Authorization: Bearer $(kontur_gcp_access_token)" \
    "https://storage.googleapis.com/storage/v1/b/${bucket}/o/${encoded_object}?alt=media" \
    -o "$dest"
}

# ensure_kontur_ssh_key finds or generates the SSH keypair a guest image
# bakes in as the operator account's only authorized_keys entry
# (packer/kontur/guest-setup.sh's own OPERATOR_SSH_PUBLIC_KEY), before
# ensure_kontur_images needs the public half to build one. Generating one
# automatically -- rather than requiring an operator to run `ssh-keygen`
# and push-secrets.sh by hand before a first deploy, the way
# terraform/gcp-v2/README.md's "Kontur sandboxing" used to -- is the other
# half of what removes that manual step (bwsalmon/agents#531): a fresh
# host with none of this configured still ends up with a working keypair,
# with no extra input required.
#
# Checked in order: GRAIN_KONTUR_SSH_KEY_FILE (an operator's own explicit
# choice), then $GRAIN_DATA_DIR/secrets/kontur-ssh-key (a previous run's
# generated-or-seeded key -- reused rather than replaced, since the guest
# image already baked in the matching public half and a mismatched keypair
# cannot SSH in at all), then generated fresh. Sets KONTUR_SSH_PUBLIC_KEY
# (fed into the guest-image build below) and KONTUR_SSH_PRIVATE_KEY
# (seeded into $GRAIN_DATA_DIR/secrets by seed_kontur_ssh_key, once that
# directory exists -- setup_data_dir runs well after this) -- both left
# empty, and GRAIN_KONTUR_ENABLE reset to 0, on any failure below.
KONTUR_SSH_PUBLIC_KEY=""
KONTUR_SSH_PRIVATE_KEY=""

ensure_kontur_ssh_key() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if ! command -v ssh-keygen >/dev/null 2>&1; then
    log "GRAIN_KONTUR_ENABLE=1 but ssh-keygen is not installed -- leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi

  local existing="" existing_is_managed=0
  if [ -n "$GRAIN_KONTUR_SSH_KEY_FILE" ] && [ -s "$GRAIN_KONTUR_SSH_KEY_FILE" ]; then
    existing="$GRAIN_KONTUR_SSH_KEY_FILE"
  elif [ -s "$GRAIN_DATA_DIR/secrets/kontur-ssh-key" ]; then
    existing="$GRAIN_DATA_DIR/secrets/kontur-ssh-key"
    existing_is_managed=1
  fi

  if [ -n "$existing" ]; then
    KONTUR_SSH_PRIVATE_KEY="$(cat "$existing")"
    KONTUR_SSH_PUBLIC_KEY="$(ssh-keygen -y -f "$existing" 2>/dev/null || true)"
    if [ -n "$KONTUR_SSH_PUBLIC_KEY" ]; then
      return
    fi
    KONTUR_SSH_PRIVATE_KEY=""
    # $GRAIN_DATA_DIR/secrets/kontur-ssh-key is the slot this script
    # alone ever writes (GRAIN_KONTUR_SSH_KEY_FILE, above, is always an
    # operator's own explicit choice instead) -- so a corrupt file there
    # can only be this script's own past mistake (bwsalmon/agents#543:
    # seed_secret used to drop the trailing newline an OpenSSH private
    # key needs). seed_secret's own never-overwrite contract means
    # nothing would ever replace it on its own, wedging kontur off on
    # every future run even after that bug is fixed -- so, but only when
    # this run is about to build its own guest image right alongside it
    # (no GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE pinning this
    # host to a specific already-published keypair -- the same condition
    # the generate-fresh path below already requires), move it aside and
    # fall through to generate a fresh one instead of just giving up.
    if [ "$existing_is_managed" = "1" ] && [ -z "$GRAIN_KONTUR_IMAGE_BUCKET" ] && [ -z "$GRAIN_KONTUR_OCI_IMAGE" ]; then
      log "GRAIN_KONTUR_ENABLE=1 but $existing is not a valid SSH private key -- moving it aside and generating a fresh one"
      mv -f "$existing" "$existing.invalid.$(date +%s)"
    else
      log "GRAIN_KONTUR_ENABLE=1 but $existing is not a valid SSH private key -- leaving kontur sandboxing off this run"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
  fi

  # A guest image built and published elsewhere (GRAIN_KONTUR_IMAGE_BUCKET/
  # GRAIN_KONTUR_OCI_IMAGE both set -- ensure_kontur_images_fetch, not
  # _build, is what this run will actually take) already has some specific
  # public key baked into its authorized_keys. Generating a fresh one here
  # would not match it -- every kontur VM would then fail to SSH in with
  # no clue why, since nothing checks the two still agree (this file's own
  # header on that, and push-secrets.sh's). Only safe to generate one when
  # this run is about to build its own guest image right alongside it
  # (ensure_kontur_images_build, below), which bakes in whatever key this
  # function hands it.
  if [ -n "$GRAIN_KONTUR_IMAGE_BUCKET" ] || [ -n "$GRAIN_KONTUR_OCI_IMAGE" ]; then
    log "GRAIN_KONTUR_ENABLE=1 but no kontur SSH key is available -- set GRAIN_KONTUR_SSH_KEY_FILE, or push-secrets.sh's own GRAIN_KONTUR_SSH_KEY, to the private half of whatever keypair the guest image at GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE already has baked in; leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi

  log "No kontur SSH keypair yet -- generating one (its public half is baked into the guest image ensure_kontur_images builds next; no manual step needed)"
  local tmp
  tmp="$(mktemp -d)"
  ssh-keygen -q -t ed25519 -N '' -f "$tmp/key" -C grain-kontur
  KONTUR_SSH_PRIVATE_KEY="$(cat "$tmp/key")"
  KONTUR_SSH_PUBLIC_KEY="$(cat "$tmp/key.pub")"
  rm -rf "$tmp"
}

# kontur_image_tag names the guest/OCI image pair ensure_kontur_images_build
# builds and caches by hashing exactly what defines their contents:
# packer/kontur's own git tree (guest-setup.sh and build-guest.sh -- the "startup
# script" a guest image is provisioned from) and third_party/kontur's own
# vendored git tree (the kontur binary and cloud-hypervisor version the
# OCI image actually bakes in -- the "kontur version"), plus the operator
# SSH public key (a different keypair means a different authorized_keys
# baked into the guest disk). Any one of those changing -- a provision.sh
# edit, a third_party/kontur vendor bump, a rotated keypair -- changes
# this tag, which is exactly what tells ensure_kontur_images_build it has
# to rebuild rather than reuse what is already on disk (bwsalmon/agents#531:
# "name the image based on the hash of the startup script and kontur
# version so it knows when it needs to re-generate it"). Nothing here
# hashes file *contents* by hand the way, say, Terraform's own filesha256
# would: packer/kontur and third_party/kontur already live inside
# GRAIN_SRC_DIR's own git checkout, so their tree object IDs already are
# exactly that, for free.
kontur_image_tag() {
  local packer_tree tp_tree
  packer_tree="$(git -C "$GRAIN_SRC_DIR" rev-parse "HEAD:packer/kontur" 2>/dev/null || echo unknown)"
  tp_tree="$(git -C "$GRAIN_SRC_DIR" rev-parse "HEAD:third_party/kontur" 2>/dev/null || echo unknown)"
  printf '%s\n' "${packer_tree}:${tp_tree}:${KONTUR_SSH_PUBLIC_KEY}" | sha256sum | awk '{print $1}' | cut -c1-16
}

# ensure_kontur_images makes sure a guest image and an OCI image exist
# before write_systemd_units can wire up -kontur-create-arg with anything
# real for `konturctl vm create` to find.
#
# Default path (GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE both
# empty, now that neither is required by terraform/gcp-v2's own
# instance.tf precondition -- bwsalmon/agents#531): builds both itself,
# right here, caching each by kontur_image_tag so a re-run with nothing
# changed -- the overwhelmingly common case, since this runs on every
# deploy generation -- pays neither cost again. A first deploy pays it
# once, itself, instead of failing outright for want of a guest image
# nobody built yet; a later provision.sh edit or third_party/kontur vendor
# bump pays it again, automatically, the next time this script runs --
# neither needs an operator to notice and re-run a separate build step by
# hand.
#
# GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE, when both set instead,
# keep the pre-bwsalmon/agents#531 path: fetch a guest/OCI image pair
# someone already built and published centrally (terraform/gcp-v2/
# README.md, "Kontur sandboxing"), for an operator who would rather build
# once and share the result across many hosts than pay debootstrap's cost
# -- several minutes, against a real Debian mirror -- on every one of
# them.
# ensure_kontur_exec_key copies the deployment's kontur SSH private key
# into GRAIN_KONTUR_IMAGES_HOSTPATH, which konturctl mounts read-only at
# /images inside every VM container it starts. That is where `kontur exec`
# reads it from (-kontur-exec-key, below): the daemon reaches a sandbox
# guest by exec'ing into that VM's own container, so the key has to be
# readable *there*, not on the host where the daemon runs.
#
# It is the same key ensure_kontur_ssh_key already generated and
# packer/kontur/guest-setup.sh already baked into the guest's own
# authorized_keys -- copied, not moved, since $GRAIN_DATA_DIR/secrets
# remains where the deployment's own copy lives.
ensure_kontur_exec_key() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ ! -s "$GRAIN_DATA_DIR/secrets/kontur-ssh-key" ]; then
    log "kontur exec key: $GRAIN_DATA_DIR/secrets/kontur-ssh-key is missing or empty -- leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  install -d -m 0755 "$GRAIN_KONTUR_IMAGES_HOSTPATH"
  install -m 0600 "$GRAIN_DATA_DIR/secrets/kontur-ssh-key" \
    "$GRAIN_KONTUR_IMAGES_HOSTPATH/$GRAIN_KONTUR_EXEC_KEY_NAME"
  log "kontur exec key staged at $GRAIN_KONTUR_IMAGES_HOSTPATH/$GRAIN_KONTUR_EXEC_KEY_NAME (/images/$GRAIN_KONTUR_EXEC_KEY_NAME in each VM container)"
}

ensure_kontur_images() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi

  if [ -n "$GRAIN_KONTUR_IMAGE_BUCKET" ] || [ -n "$GRAIN_KONTUR_OCI_IMAGE" ]; then
    if [ -z "$GRAIN_KONTUR_IMAGE_BUCKET" ] || [ -z "$GRAIN_KONTUR_OCI_IMAGE" ]; then
      log "GRAIN_KONTUR_ENABLE=1 and one of GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE is set but not the other -- both or neither; leaving kontur sandboxing off this run"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
    ensure_kontur_images_fetch
    return
  fi

  ensure_kontur_images_build
}

# ensure_kontur_images_fetch is the pre-bwsalmon/agents#531 behavior,
# unchanged: always (re-)fetches the bucket's "latest" alias and pulls the
# OCI image, on every run, rather than caching by kontur_image_tag -- an
# operator choosing this path already owns when "latest" changes (their
# own build-guest.sh/build-oci-image.sh invocation, run separately), so there is
# no local staleness for this script to detect on its own.
ensure_kontur_images_fetch() {
  log "Fetching kontur guest image from gs://${GRAIN_KONTUR_IMAGE_BUCKET}/kontur-guest/latest"
  local img_dir="${GRAIN_KONTUR_IMAGES_HOSTPATH}/current" tmp_dir f
  tmp_dir="$(mktemp -d)"
  for f in vmlinuz initrd.img disk.img; do
    if ! gcs_fetch "$GRAIN_KONTUR_IMAGE_BUCKET" "kontur-guest/latest/$f" "$tmp_dir/$f"; then
      log "  could not fetch kontur-guest/latest/$f from gs://${GRAIN_KONTUR_IMAGE_BUCKET} -- leaving kontur sandboxing off this run"
      rm -rf "$tmp_dir"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
  done
  # rm -rf, not rmdir/install -d alone: a previous run of this same script
  # against the same host may have taken ensure_kontur_images_build's own
  # path instead (or vice versa, on a later config change), which leaves
  # "current" a symlink rather than a real directory -- this needs to work
  # either way.
  rm -rf "$img_dir"
  install -d -m0755 "$img_dir"
  mv -f "$tmp_dir"/vmlinuz "$tmp_dir"/initrd.img "$tmp_dir"/disk.img "$img_dir/"
  rmdir "$tmp_dir"

  log "Pulling kontur OCI image $GRAIN_KONTUR_OCI_IMAGE"
  local registry_host="${GRAIN_KONTUR_OCI_IMAGE%%/*}"
  if ! docker login -u oauth2accesstoken -p "$(kontur_gcp_access_token)" "https://${registry_host}" >/dev/null 2>&1; then
    log "  could not authenticate docker to ${registry_host} -- leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  if ! docker pull "$GRAIN_KONTUR_OCI_IMAGE"; then
    log "  could not pull $GRAIN_KONTUR_OCI_IMAGE -- leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  # konturctl's own default -kontur-image is localhost:5000/kontur:latest
  # (bwsalmon/kontur's own internal/staticpod/spec.go) -- retagging here
  # means write_systemd_units needs no -kontur-create-arg=-kontur-image of
  # its own, and no registry actually has to run at :5000 for it: docker
  # only resolves a tag's registry host when it has to push or pull that
  # exact tag, never for a local retag of an image already present.
  docker tag "$GRAIN_KONTUR_OCI_IMAGE" localhost:5000/kontur:latest
}

# ensure_kontur_images_build is the default path (bwsalmon/agents#531):
# builds the guest image (packer/kontur/build-guest.sh -- kontur's own
# guest build, a plain `docker build` needing neither root nor a VM boot)
# and the OCI image (build-oci-image.sh -- likewise a plain `docker
# build`, KONTUR_OCI_SKIP_PUSH=1 so no registry is ever touched) itself,
# right here, skipping either step entirely once kontur_image_tag shows
# a matching one already exists on disk.
ensure_kontur_images_build() {
  local tag current img_dir
  tag="$(kontur_image_tag)"
  current="${GRAIN_KONTUR_IMAGES_HOSTPATH}/current"
  img_dir="${GRAIN_KONTUR_IMAGES_HOSTPATH}/${tag}"

  if [ -s "$img_dir/vmlinuz" ] && [ -s "$img_dir/initrd.img" ] && [ -s "$img_dir/disk.img" ]; then
    log "kontur guest image ${tag} already built -- reusing it"
  else
    log "Building kontur guest image ${tag} (packer/kontur/build-guest.sh -- one docker build, no VM boot; this can take several minutes)"
    local tmp_out
    tmp_out="$(mktemp -d)"
    if ! env \
        OPERATOR_SSH_PUBLIC_KEY="$KONTUR_SSH_PUBLIC_KEY" \
        SANDBOX_SETUP_SCRIPT="" \
        OUTPUT_DIR="$tmp_out" \
        "$GRAIN_SRC_DIR/packer/kontur/build-guest.sh"; then
      log "  packer/kontur/build-guest.sh failed -- leaving kontur sandboxing off this run"
      rm -rf "$tmp_out"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
    rm -rf "$img_dir"
    install -d -m0755 "$img_dir"
    mv -f "$tmp_out"/vmlinuz "$tmp_out"/initrd.img "$tmp_out"/disk.img "$img_dir/"
    rm -rf "$tmp_out"
  fi

  if docker image inspect "localhost:5000/kontur:${tag}" >/dev/null 2>&1; then
    log "kontur OCI image ${tag} already built -- reusing it"
  else
    log "Building kontur OCI image ${tag} (build-oci-image.sh -- docker build only, no push)"
    if ! env \
        KONTUR_OCI_IMAGE="localhost:5000/kontur:${tag}" \
        KONTUR_OCI_SKIP_PUSH=1 \
        "$GRAIN_SRC_DIR/packer/kontur/build-oci-image.sh"; then
      log "  packer/kontur/build-oci-image.sh failed -- leaving kontur sandboxing off this run"
      GRAIN_KONTUR_ENABLE=0
      return
    fi
  fi
  # konturctl's own default -kontur-image is localhost:5000/kontur:latest
  # (bwsalmon/kontur's own internal/staticpod/spec.go) -- retagging here
  # means write_systemd_units needs no -kontur-create-arg=-kontur-image of
  # its own.
  docker tag "localhost:5000/kontur:${tag}" localhost:5000/kontur:latest

  # "current" is a symlink here, not a real directory the way
  # ensure_kontur_images_fetch's own is -- img_dir itself is already named
  # for kontur_image_tag and left in place (never overwritten) so a rollback
  # to a previous tag, or a rebuild racing a still-running VM against the
  # old one, never has to reconstruct it from scratch.
  rm -rf "$current"
  ln -s "$tag" "$current"
}

# konturctl itself is no longer built or installed here: it ships inside
# the deployment image (v2/Dockerfile builds it from the same vendored
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
# cloud-hypervisor guest itself), the docker group (for the docker
# kontur backend's own `docker run`) -- the same grant
# grant_docker_group gives an operator's own shell, and safe to grant
# twice: usermod -aG only ever adds -- and ownership of
# GRAIN_KONTUR_DISK_HOSTPATH, where konturctl creates each VM's own
# writable disk overlay (bwsalmon/agents#510). Unlike
# GRAIN_KONTUR_IMAGES_HOSTPATH (populated by this script, running as
# root, before grain-daemon ever runs), that directory is created lazily
# by konturctl itself on a VM's first "vm create" -- $GRAIN_USER needs to
# own its parent so that mkdir succeeds unprivileged.
#
# konturctl runs inside grain-daemon.service's own container now, as this
# same uid and against this same path: docker_run_args mounts both this
# directory and /var/lib/kontur/vms below at the very paths they have out
# here, precisely so that a path konturctl writes, and the identical path
# it then hands the host's docker daemon as a bind mount for the VM
# container, mean the same directory.
ensure_kontur_kvm_access() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ ! -e /dev/kvm ]; then
    log "GRAIN_KONTUR_ENABLE=1 but /dev/kvm does not exist on this host -- terraform/gcp-v2's enable_nested_virtualization must be on and machine_type must support it (see that variable's own doc); leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  if getent group kvm >/dev/null 2>&1; then
    usermod -aG kvm "$GRAIN_USER"
  fi
  grant_docker_group
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_KONTUR_DISK_HOSTPATH"
  # konturctl's own defaultStateDir (third_party/kontur/internal/cli/
  # vm.go) -- where it records each VM it creates, distinct from
  # GRAIN_KONTUR_DISK_HOSTPATH above (each VM's own disk overlay). Never
  # overridden by a -kontur-create-arg -state-dir below, so this is the
  # exact path konturctl -- run unprivileged, as $GRAIN_USER -- actually
  # tries to create on its first "vm create": without this, that mkdir
  # fails closed with "permission denied" and grain-daemon.service dies
  # on its very first dispatched task, the same way GRAIN_KONTUR_DISK_
  # HOSTPATH would without the install -d right above it.
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
# docker.io 20.10.24 on the grain-v2-staging-host image) never fills it
# in for the default bridge network's own auto-allocated pool, even
# after a container has actually attached to it, so `gw` here came back
# empty on every real install and permanently disabled kontur sandboxing.
# The address containers on that network actually get routed through is
# simpler ground truth: whatever IPv4 address the bridge device itself
# (by default docker0, but overridable via `docker network create -o
# com.docker.network.bridge.name`, hence reading it from the network's
# own Options rather than hardcoding it) carries on this host.
ensure_kontur_git_proxy_host() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ -n "$GRAIN_KONTUR_GIT_PROXY_HOST" ]; then
    return
  fi
  local gw iface
  gw="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null)"
  if [ -z "$gw" ]; then
    iface="$(docker network inspect bridge -f '{{index .Options "com.docker.network.bridge.name"}}' 2>/dev/null)"
    iface="${iface:-docker0}"
    gw="$(ip -4 -o addr show dev "$iface" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1)"
  fi
  if [ -z "$gw" ]; then
    log "GRAIN_KONTUR_ENABLE=1 but GRAIN_KONTUR_GIT_PROXY_HOST is unset and docker's own \"bridge\" network has no gateway address to default it to -- set GRAIN_KONTUR_GIT_PROXY_HOST explicitly (see this script's own -h); leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  GRAIN_KONTUR_GIT_PROXY_HOST="$gw"
  log "kontur git proxy host defaulted to docker bridge gateway $GRAIN_KONTUR_GIT_PROXY_HOST"
}

# seed_kontur_ssh_key writes KONTUR_SSH_PRIVATE_KEY -- found or generated
# by ensure_kontur_ssh_key, above, well before $GRAIN_DATA_DIR/secrets
# necessarily existed to write it into -- to
# $GRAIN_DATA_DIR/secrets/kontur-ssh-key, the same never-overwrite
# contract seed_secret gives every other plain-file secret -- reused
# directly here since an SSH private key is just another multi-line
# value, nothing seed_secret's own printf '%s' needs to treat specially.
seed_kontur_ssh_key() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  seed_secret "$GRAIN_DATA_DIR/secrets/kontur-ssh-key" "$KONTUR_SSH_PRIVATE_KEY"
}

# --- 6. data directory and secrets --------------------------------------

seed_secret() {
  # Writes $2 to file $1 only if it is missing or empty, and only if a
  # value was actually given -- never overwrites a credential a previous
  # run (or an operator by hand) already placed, and never writes an
  # empty file for a value nobody supplied this time.
  #
  # Trailing newline appended deliberately: $2 reaches every caller
  # through at least one layer of "$(...)" command substitution (here,
  # in ensure_kontur_ssh_key, in files/deploy.sh reading metadata --
  # bash strips every trailing newline doing that), so without adding
  # one back a multi-line PEM/OpenSSH-format value -- kontur-ssh-key
  # chief among them -- is written one newline short of what it started
  # as. ssh-keygen/ssh both refuse to parse an OpenSSH private key
  # missing its final newline ("error in libcrypto", no clearer
  # message), which is exactly what made ensure_kontur_ssh_key call its
  # own freshly-generated, self-seeded key invalid on every later run
  # (bwsalmon/agents#543). Harmless for every other seed_secret caller
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
}

setup_data_dir() {
  log "Laying out $GRAIN_DATA_DIR"
  install -d -m0750 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR"
  # bin/ no longer holds a grain binary -- nothing on this host does
  # (install_cli_wrappers) -- but it is kept, owned by $GRAIN_USER, for
  # the same reason $GRAIN_DATA_DIR itself is: a deployment upgraded
  # across bwsalmon/agents#645 already has one, with a previous release's
  # binary in it, and an operator who put something of their own there
  # should find it where they left it. install -d re-applies -o/-g even
  # against a directory that already exists (this file's own comment on
  # sync_repo's mkdir-vs-install-d distinction).
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
  # grain-daemon.service's own -data-dir/store, embedded SQLite -- the
  # one process that ever opens it now (this file's own header on
  # bwsalmon/agents#363), so no separate store container or directory
  # layout is needed for it beyond what openStore creates on its own the
  # first time the daemon starts.

  # GitHub credential ladder (v2/pkg/gitproxy/credentials.go): a pattern
  # file plus one <name>.token or <name>.app.json per credential. "*" is
  # the catch-all every repo falls back to absent a narrower entry -- an
  # operator wanting a per-repo credential edits credentials.json and
  # adds another <name>.token/.app.json by hand; this script only ever
  # seeds the one default, as either kind (never both for the same name:
  # CredentialSet.load prefers .app.json when present).
  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
     && { [ -n "$GRAIN_GITHUB_TOKEN" ] || [ -n "$GRAIN_GITHUB_APP_ID" ]; }; then
    printf '{"*":"%s"}\n' "$GRAIN_GITHUB_CREDENTIAL_NAME" > "$GRAIN_DATA_DIR/secrets/github/credentials.json"
    chown "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github/credentials.json"
  fi
  seed_secret "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" "$GRAIN_GITHUB_TOKEN"
  seed_github_app_credential

  # Order matters below: the minter key has to be in the secrets
  # database before mint_gemini_operating_key can authenticate with it.
  seed_secret "$GRAIN_DATA_DIR/secrets/gemini-api-key" "$GRAIN_GEMINI_API_KEY"
  seed_secret "$GRAIN_DATA_DIR/secrets/claude-oauth-token" "$GRAIN_CLAUDE_CODE_OAUTH_TOKEN"

  seed_gcp_minter_key

  mint_gemini_operating_key

  seed_kontur_ssh_key

  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ]; then
    log "  no GitHub credential configured yet -- set GRAIN_GITHUB_TOKEN, or all three of"
    log "  GRAIN_GITHUB_APP_ID/INSTALLATION_ID/PRIVATE_KEY, and re-run, or place"
    log "  $GRAIN_DATA_DIR/secrets/github/credentials.json and a matching .token/.app.json by hand"
  fi
}

# seed_gcp_minter_key writes the GCP minter's key into gcp-key-minter/key.json
# in the (SQLite-backed, v2/pkg/secrets's own doc comment) secrets database
# -- through the just-installed `grain secrets` CLI rather than a raw file
# write, since that database is no longer a directory tree this script can
# lay files into directly. `grain secrets list` is checked first so a
# re-run never overwrites a key an earlier run (or an operator's own
# `grain secrets set`) already placed, the same never-overwrite rule
# seed_secret gives every plain-file secret above; chown afterward because
# the CLI, run here as root, otherwise leaves the database it creates
# owned by root, unreadable by grain-daemon.service's own $GRAIN_USER.
seed_gcp_minter_key() {
  if [ -z "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" ]; then
    return
  fi
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null | grep -q '^gcp-key-minter:'; then
    return
  fi
  # Staged under $GRAIN_DATA_DIR before being handed to the CLI, and
  # removed straight after: `grain` is a wrapper around `docker run` now
  # (install_cli_wrappers), and -value-file has to name a path that
  # resolves *inside* that container. The file this is given is
  # deliberately somewhere else -- terraform/gcp-v2/files/deploy.sh
  # writes it into a tmpfs under /run -- and mounting an arbitrary
  # operator-supplied path into the daemon's own container for the sake
  # of one read would be a far worse trade than a copy that lives for
  # one command.
  local staged="$GRAIN_DATA_DIR/secrets/.minter-key.staged.json"
  ( umask 077 && cat "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" > "$staged" )
  /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" set \
    -value-file "$staged" gcp-key-minter key.json
  rm -f "$staged"
  chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
}

# mint_gemini_operating_key mints the daemon's own Gemini API key, using
# the minter credential seed_gcp_minter_key just placed, when no key is
# in place yet.
#
# A deployment that grants its minter roles/serviceusage.apiKeysAdmin
# (terraform/gcp-v2's enable_gemini_key, on by default there) already has
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
    chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
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
  new_version="$(/usr/local/bin/grain schema-version)"

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

  if [ -d "$store_dir" ]; then
    local backup="${store_dir}.schema${old_version}-$(date +%Y%m%d%H%M%S)"
    log "Schema changed ($old_version -> $new_version) -- moving $store_dir aside to $backup so grain starts a fresh store"
    mv "$store_dir" "$backup"
  else
    log "Schema changed ($old_version -> $new_version) -- no existing store to move aside"
  fi
  printf '%s\n' "$new_version" > "$marker"
}

# --- 7. format the target repo, if it is empty --------------------------
#
# Every dispatch grain runs branches off an existing ref -- it never
# creates the first one (v2/e2e's own harness always seeds one commit
# before driving anything against a bare repo). A repo created fresh on
# GitHub has none, so `grain create -repo owner/name ...` would have
# nothing to branch from. Detected with `git ls-remote`, which returns no
# output at all against a repo with zero refs -- no clone needed just to
# find that out.

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
  if [ -n "$(git ls-remote "$url" 2>/dev/null)" ]; then
    return
  fi

  log "  it's empty -- pushing one empty commit to $GRAIN_TARGET_BRANCH so grain has something to branch from"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  git init --quiet -b "$GRAIN_TARGET_BRANCH" "$tmp"
  git -C "$tmp" -c user.name=grain -c user.email=grain@localhost \
    commit --quiet --allow-empty -m "Initial commit (created by grain setup.sh)"
  git -C "$tmp" push --quiet "$url" "HEAD:refs/heads/$GRAIN_TARGET_BRANCH"
}

# --- 8. the systemd unit ---------------------------------------------------

# The default agent framework (agent/antigravity) runs Google's
# Antigravity CLI as a subprocess, so unlike the in-process Gemini
# runtime it replaced it needs a binary on this host. Report a missing
# one here, where the log is already being read, rather than letting
# grain-daemon.service come up and fail at its first dispatch with
# "resolving the agy binary".
#
# Never fatal: this script is re-run on every deploy generation, a host
# may legitimately be running -agent-framework claude instead, and an
# install that lands after this point still works with no further
# action. Warning and carrying on is the same trade ensure_ops_agent
# makes.
verify_agent_cli() {
  if [ -n "$GRAIN_AGY_PATH" ]; then
    [ -x "$GRAIN_AGY_PATH" ] && return
    log "WARNING: GRAIN_AGY_PATH=$GRAIN_AGY_PATH is not an executable file on this host."
  fi
  log "WARNING: no Antigravity CLI (agy) for this deployment to run. The default agent"
  log "         framework runs it as a subprocess, so dispatches will fail until there is"
  log "         one. Unlike the claude CLI, agy is not in the deployment image -- this repo"
  log "         has no verified installer URL to bake one in with -- so install it on this"
  log "         host (its own installer targets ~/.gemini/bin/agy) and set GRAIN_AGY_PATH,"
  log "         which is what mounts it into the container. A deployment running"
  log "         -agent-framework claude instead can ignore this."
}

write_systemd_units() {
  verify_agent_cli
  log "Writing grain-daemon.service"

  local daemon_args=(
    daemon
    -data-dir "$GRAIN_DATA_DIR"
    -sandbox-dir "$GRAIN_SANDBOX_DIR"
    -max-concurrent "$GRAIN_MAX_CONCURRENT"
    -poll-interval "$GRAIN_POLL_INTERVAL"
    -gemini-api-key-file "$GRAIN_DATA_DIR/secrets/gemini-api-key"
    -claude-oauth-token-file "$GRAIN_DATA_DIR/secrets/claude-oauth-token"
    -github-host "$GRAIN_GITHUB_HOST"
    -ui-addr "$GRAIN_UI_ADDR"
  )
  # bwsalmon/agents#396: the UI's own Upgrade button, which on a
  # container deployment means "pull the image CI published for that
  # branch and restart onto it" rather than "fetch, build, install"
  # (bwsalmon/agents#645, v2/pkg/upgrade/image.go). -upgrade-image names
  # the repository CI publishes a tag per branch to; -upgrade-image-ref-
  # file is the file this unit itself reads as an EnvironmentFile, so an
  # upgrade repoints the service by writing one line and restarting, with
  # no unit to rewrite and no root anywhere in the path.
  #
  # -upgrade-src-dir rides along, and no longer means "build here": with
  # -upgrade-image set the daemon never builds, and this is only what
  # grantTools (cmd/grain/daemon.go) reads grain's own source out of for
  # the self-debug capability. It is passed only alongside the Upgrade
  # button for the same reason it always was -- it is the same read-only
  # mount either way, and a deployment that turned this feature off has
  # said it wants nothing here wired up.
  #
  # Left unset entirely when GRAIN_ENABLE_UI_UPGRADE=0
  # (terraform/gcp-v2's own deploy.sh sets exactly that): the daemon
  # flags themselves default to empty/disabled (cmd/grain/daemon.go), so
  # simply not passing them is enough -- see this script's own header on
  # GRAIN_ENABLE_UI_UPGRADE (bwsalmon/agents#405).
  if [ "$GRAIN_ENABLE_UI_UPGRADE" = "1" ]; then
    daemon_args+=(
      -upgrade-image "$GRAIN_IMAGE"
      -upgrade-image-ref-file "$IMAGE_REF_FILE"
      -upgrade-src-dir "$GRAIN_SRC_DIR"
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
  [ -n "$GRAIN_CLAUDE_PATH" ] && daemon_args+=(-claude-path "$GRAIN_CLAUDE_PATH")
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
  # only ever passed once ensure_kontur_images/ensure_kontur_kvm_access/
  # seed_kontur_ssh_key have all actually succeeded this run (each resets
  # GRAIN_KONTUR_ENABLE to 0 on its own failure), so a host that cannot
  # yet dispatch onto a real VM keeps dispatching into host directories
  # instead of installing a daemon that would fail every task. -backend
  # docker matches ensure_kontur_images' own retag (localhost:5000/
  # kontur:latest) and needs no konturctl setup/containerd/CNI/kubelet on
  # this host (bwsalmon/agents#353). -disk/-kernel/-initramfs are
  # container-internal paths, resolved against -images-hostpath mounted
  # read-only at /images -- "current" is ensure_kontur_images' own fixed
  # destination path (a real directory when it fetched a pre-built image,
  # a symlink to whatever kontur_image_tag it built when it built one
  # itself), not a version string this script has to track.
  # -guest-port 22 is not optional: konturctl's own default is 80, which
  # silently refuses every connection to this image's actual sshd
  # (packer/kontur/README.md, "guest-port 22 is not optional").
  # -disk-readonly=false/-disk-hostpath give each VM a genuinely
  # persistent, writable root filesystem instead of the read-only one
  # -images-hostpath alone provides (bwsalmon/agents#510):
  # third_party/kontur/README.md's "Operating a node (konturctl CLI)"
  # section explains why -images-hostpath itself can never be made
  # writable.
  if [ "$GRAIN_KONTUR_ENABLE" = "1" ]; then
    daemon_args+=(
      -kontur-sandboxes
      -kontur-ssh-user "$GRAIN_KONTUR_SSH_USER"
      -kontur-exec-key "/images/$GRAIN_KONTUR_EXEC_KEY_NAME"
      -kontur-workspace "$GRAIN_KONTUR_WORKSPACE"
      -kontur-net "$GRAIN_KONTUR_NET"
      -kontur-git-proxy-host "$GRAIN_KONTUR_GIT_PROXY_HOST"
      -kontur-create-arg -images-hostpath -kontur-create-arg "$GRAIN_KONTUR_IMAGES_HOSTPATH"
      -kontur-create-arg -disk -kontur-create-arg /images/current/disk.img
      -kontur-create-arg -kernel -kontur-create-arg /images/current/vmlinuz
      -kontur-create-arg -initramfs -kontur-create-arg /images/current/initrd.img
      -kontur-create-arg -disk-readonly=false
      -kontur-create-arg -disk-hostpath -kontur-create-arg "$GRAIN_KONTUR_DISK_HOSTPATH"
    )
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
#                        capability (v2/Dockerfile), which is what turns
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
#                        /run/log/journal when it is not).
#   the docker socket    kontur (konturctl, and pkg/mcp's docker-exec
#                        transport) and the Upgrade button's own `docker
#                        pull` both talk to this host's engine. Mounted
#                        only when one of those is actually turned on --
#                        this is the one entry here that grants the
#                        container root-equivalent authority over the
#                        host, so it is not given for free to a
#                        deployment that has no use for it.
#   the kontur mounts    konturctl records its VMs in /var/lib/kontur and
#                        creates each VM's disk overlay under
#                        GRAIN_KONTUR_DISK_HOSTPATH; the paths it then
#                        hands the host's docker daemon as bind mounts
#                        are those same host paths, which is why every
#                        one of them is mounted at its own path.
#   GRAIN_CLAUDE_PATH/GRAIN_AGY_PATH
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
  local uid gid docker_gid
  uid="$(id -u "$GRAIN_USER")"
  gid="$(id -g "$GRAIN_USER")"

  DOCKER_ARGS=(
    --rm
    --network host
    --user "${uid}:${gid}"
    --env "HOME=${GRAIN_DATA_DIR}/home"
    --volume "${GRAIN_DATA_DIR}:${GRAIN_DATA_DIR}"
    --volume "${GRAIN_SANDBOX_DIR}:${GRAIN_SANDBOX_DIR}"
    --volume "${GRAIN_SRC_DIR}:${GRAIN_SRC_DIR}:ro"
  )

  case "${GRAIN_UI_ADDR##*:}" in
    ''|*[!0-9]*) ;;
    *) [ "${GRAIN_UI_ADDR##*:}" -lt 1024 ] && DOCKER_ARGS+=(--cap-add NET_BIND_SERVICE) ;;
  esac

  [ -f /etc/machine-id ] && DOCKER_ARGS+=(--volume /etc/machine-id:/etc/machine-id:ro)
  [ -d /var/log/journal ] && DOCKER_ARGS+=(--volume /var/log/journal:/var/log/journal:ro)
  [ -d /run/log/journal ] && DOCKER_ARGS+=(--volume /run/log/journal:/run/log/journal:ro)

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
    DOCKER_ARGS+=(
      --volume /var/lib/kontur:/var/lib/kontur
      --volume "${GRAIN_KONTUR_IMAGES_HOSTPATH}:${GRAIN_KONTUR_IMAGES_HOSTPATH}:ro"
      --volume "${GRAIN_KONTUR_DISK_HOSTPATH}:${GRAIN_KONTUR_DISK_HOSTPATH}"
    )
  fi

  [ -n "$GRAIN_CLAUDE_PATH" ] && DOCKER_ARGS+=(--volume "$(dirname "$GRAIN_CLAUDE_PATH"):$(dirname "$GRAIN_CLAUDE_PATH"):ro")
  [ -n "$GRAIN_AGY_PATH" ] && DOCKER_ARGS+=(--volume "$(dirname "$GRAIN_AGY_PATH"):$(dirname "$GRAIN_AGY_PATH"):ro")

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
  # git-proxy service (docs/next-session.md item 3's "Update"): restarting
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
     && [ ! -s "$GRAIN_DATA_DIR/secrets/claude-oauth-token" ]; then
    log "grain-daemon.service is running, but no agent credential is configured -- no task can"
    log "  be dispatched until one is. Set it in the UI (Settings -> Agent frameworks), or set"
    log "  GRAIN_GEMINI_API_KEY / GRAIN_CLAUDE_CODE_OAUTH_TOKEN and re-run this script."
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
  local github="MISSING" gemini="MISSING" claude="MISSING" minter="MISSING" daemon ready=1
  # The binary, not the token: they are independent, and a deployment
  # with the token set and no CLI is exactly the state that fails every
  # claude run with "executable file not found in $PATH".
  #
  # Asked of the *image*, not of this host: that is where the CLI lives
  # now (v2/Dockerfile), so `command -v claude` out here would report a
  # host that has nothing to do with whether a dispatch can run. The
  # exception is an operator-named copy, which is a host path by
  # definition -- docker_run_args mounts it in.
  local claude_cli="MISSING"
  if [ -n "$GRAIN_CLAUDE_PATH" ]; then
    [ -x "$GRAIN_CLAUDE_PATH" ] && claude_cli="$GRAIN_CLAUDE_PATH (mounted from this host)" || claude_cli="MISSING (GRAIN_CLAUDE_PATH names nothing executable on this host)"
  elif claude_cli="$(docker run --rm --entrypoint sh "$GRAIN_IMAGE_REF" -c 'command -v claude' 2>/dev/null)" && [ -n "$claude_cli" ]; then
    claude_cli="$claude_cli (in $GRAIN_IMAGE_REF)"
  else
    claude_cli="MISSING"
  fi

  if [ -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
     && [ -s "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" ]; then
    github="present as '${GRAIN_GITHUB_CREDENTIAL_NAME}'"
  fi
  [ -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ] && gemini="present"
  [ -s "$GRAIN_DATA_DIR/secrets/claude-oauth-token" ] && claude="present"
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
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null \
     | grep -q '^gcp-key-minter:'; then
    minter="present"
  fi
  daemon="$(systemctl is-active grain-daemon.service 2>/dev/null || echo unknown)"

  echo
  log "Readiness:"
  echo "    daemon:            $daemon"
  echo "    image:             $GRAIN_IMAGE_REF"
  echo "    GitHub credential: $github"
  echo "    Gemini key:        $gemini"
  echo "    Claude token:      $claude"
  echo "    claude CLI:        $claude_cli"
  echo "    GCP minter key:    $minter"
  echo "    target repos:      ${GRAIN_TARGET_REPOS:-<none: unrestricted -- any repo a task names is allowed>}"
  echo "    default repo:      ${GRAIN_TARGET_REPO:-<none: a task with no repo parks>}"
  echo "    max concurrent:    ${GRAIN_MAX_CONCURRENT:-<default>}"
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
  if [ "$gemini" = "MISSING" ] && [ "$claude" = "MISSING" ]; then
    ready=0
    echo "    !! With neither agent credential set, the daemon runs and serves ${GRAIN_UI_ADDR},"
    echo "       but every dispatched run fails at setup. Set one in the UI (Settings ->"
    echo "       Agent frameworks) or re-run with GRAIN_GEMINI_API_KEY/GRAIN_CLAUDE_CODE_OAUTH_TOKEN."
  fi
  case "$claude_cli" in
    MISSING*)
      # Not a readiness failure: a gemini deployment neither needs nor
      # misses this. It is still worth a line, because nothing in the UI
      # says the framework it offers cannot run here.
      echo "    -- no claude CLI in $GRAIN_IMAGE_REF: the \"claude\" agent framework cannot run"
      echo "       until there is one (Settings -> Agent framework, and the per-task override,"
      echo "       still offer it). That image was built with INSTALL_CLAUDE_CLI=0 or its"
      echo "       install failed in CI -- deploy a tag that has one, or point"
      echo "       GRAIN_CLAUDE_PATH at a copy on this host."
      ;;
  esac
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
  echo "    CLI:     grain list  (a new shell picks up GRAIN_SERVER from /etc/profile.d/grain.sh;"
  echo "             in this one: export GRAIN_SERVER=http://127.0.0.1:${GRAIN_UI_ADDR##*:})"
  echo "    Image:   ${GRAIN_IMAGE_REF} (recorded in ${IMAGE_REF_FILE}; \`docker images\` for what is local)"
  echo "    Logs:    journalctl -u grain-daemon.service -f  (or: docker logs -f grain-daemon)"
  echo "    Update:  re-run this script (sudo ./setup.sh) -- it pulls the image for GRAIN_REF and restarts"
  echo "             the service. Pin or roll back with GRAIN_IMAGE_TAG=sha-<short sha>."
  report_readiness
}

main() {
  sync_repo
  reexec_if_updated "$@"
  ensure_user
  ensure_src_dir_readable
  grant_docker_group
  pull_image
  # After ensure_user (the wrappers run the image as that account) and
  # before setup_data_dir, which is the first thing here to actually use
  # the `grain` CLI they install.
  install_cli_wrappers
  ensure_kontur_ssh_key
  ensure_kontur_images
  ensure_kontur_kvm_access
  ensure_kontur_git_proxy_host
  setup_sandbox_dir
  setup_data_dir
  # After setup_data_dir: seed_kontur_ssh_key, which it calls, is what
  # actually writes $GRAIN_DATA_DIR/secrets/kontur-ssh-key -- the key this
  # then stages into the images directory for `kontur exec` to read from
  # inside each VM container.
  ensure_kontur_exec_key
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
