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
#      of this one (reexec_if_updated), so a run always builds with the
#      code it just pulled rather than the code it started with
#   2. builds bin/grain with `make container-build` (v2/Makefile) -- the
#      containerised build, not the host toolchain, so a working
#      `docker` is the one thing this script assumes about the host
#      (`make` itself it installs, see ensure_make). grain is pure Go
#      (bwsalmon/agents#366 removed the one dependency that wasn't), so
#      this buys reproducibility -- one pinned Go toolchain regardless of
#      what is or isn't installed on this machine -- rather than working
#      around any host-specific linkage problem.
#   3. installs it to $GRAIN_DATA_DIR/bin/grain, with a stable
#      /usr/local/bin/grain symlink to that path for an operator's own
#      shell
#   4. creates an unprivileged system user to run it as, and grants it a
#      narrow NOPASSWD sudo rule for the UI's own reboot host button
#      (bwsalmon/agents#395) to reboot the machine. If
#      GRAIN_ENABLE_UI_UPGRADE=1 (the default -- see that variable's own
#      doc below), also gives it ownership of $GRAIN_SRC_DIR, membership
#      in the docker group, and one more narrow NOPASSWD sudo rule, for
#      its Upgrade button (bwsalmon/agents#396, v2/pkg/upgrade) to check
#      out a different branch, rebuild, install, and restart
#      grain-daemon.service later, with no further privilege of its own
#   5. lays out the rest of $GRAIN_DATA_DIR (secrets, the sandbox root,
#      the embedded SQLite store) and seeds secrets from environment
#      variables -- only if they are not already there, so a second run
#      never overwrites a credential placed by the first one or by hand
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
#      run's new binary and new config actually take effect -- see
#      docs/next-session.md item 3's "Update" for why
#      enable-without-restart was already a bug once in v1's own proxy
#      service
#
# Every setting is an environment variable, not a flag, so the common
# case is `sudo GRAIN_GITHUB_TOKEN=... GRAIN_GEMINI_API_KEY=... ./setup.sh`
# and a re-run to pick up a repo update is `sudo ./setup.sh` with no
# arguments at all. Run with -h/--help for the full list.
#
# There used to be a second service (grain-ui.service) and, before
# bwsalmon/agents#366 replaced it with embedded SQLite, a `dolt
# sql-server` container behind it: embedded Dolt was single-writer, and a
# daemon plus a UI on the same store both used to need to write.
# bwsalmon/agents#363 removed the second writer -- the daemon now serves
# the UI/API itself, in-process, over the store it already has open (see
# v2/cmd/grain/daemon.go's own doc comment) -- so this script installs
# one service, needs no separate store container, and no longer requires
# `docker` at runtime (only at build time, for `make container-build`).
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
GRAIN_USER="${GRAIN_USER:-grain}"

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
GRAIN_MAX_AGENT_TURNS="${GRAIN_MAX_AGENT_TURNS:-}"

GRAIN_GCP_PROJECT="${GRAIN_GCP_PROJECT:-}"
GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="${GRAIN_GCP_SERVICE_ACCOUNT_EMAIL:-}"
GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="${GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE:-}"

GRAIN_TARGET_REPO="${GRAIN_TARGET_REPO:-}"
GRAIN_TARGET_BRANCH="${GRAIN_TARGET_BRANCH:-main}"
GRAIN_TARGET_REPOS="${GRAIN_TARGET_REPOS:-}"

# See "Kontur sandboxing" below (ensure_kontur_images/ensure_kontur_kvm_access)
# and terraform/gcp-v2/README.md's own section of the same name for the
# one-time build-and-publish step that makes GRAIN_KONTUR_ENABLE=1 (off
# by default here -- terraform/gcp-v2's own enable_kontur_sandboxes
# variable is what actually turns this on for that deployment shape)
# actually able to create a VM rather than fail loudly at this script's
# own hands. GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE name where
# packer/kontur/build.sh and third_party/kontur's own Dockerfile publish
# to; GRAIN_KONTUR_SSH_KEY_FILE is the private half of the keypair whose
# public half is baked into the guest image (packer/kontur/build.sh's own
# OPERATOR_SSH_PUBLIC_KEY).
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
# -images-hostpath is always mounted read-only (it's a shared, node-local
# image cache several VMs may read from concurrently -- see
# third_party/kontur/README.md, "Operating a node (konturctl CLI)"), so a
# VM's own writable root filesystem instead lives here: a per-VM qcow2 overlay
# konturctl creates under -disk-hostpath, backed by the shared read-only
# disk image (bwsalmon/agents#510). Must be owned by $GRAIN_USER --
# ensure_kontur_kvm_access creates it -- since konturctl runs directly as
# that user, not inside a container the way -disk itself is read.
GRAIN_KONTUR_DISK_HOSTPATH="${GRAIN_KONTUR_DISK_HOSTPATH:-/var/lib/kontur/vm-disks}"
GRAIN_KONTUR_VM_NAME_PREFIX="${GRAIN_KONTUR_VM_NAME_PREFIX:-kontur-}"
GRAIN_KONTUR_SSH_USER="${GRAIN_KONTUR_SSH_USER:-debian}"
GRAIN_KONTUR_SSH_KEY_FILE="${GRAIN_KONTUR_SSH_KEY_FILE:-}"
GRAIN_KONTUR_WORKSPACE="${GRAIN_KONTUR_WORKSPACE:-/home/debian}"
GRAIN_KONTUR_BASE_IP="${GRAIN_KONTUR_BASE_IP:-169.254.100.10}"
GRAIN_KONTUR_BASE_PORT="${GRAIN_KONTUR_BASE_PORT:-12000}"

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

Installs or updates a v2 grain deployment on this machine: clones/builds
the binary, lays out /var/lib/grain (including its embedded SQLite task
store), and installs grain-daemon.service, which runs the dispatch loop
and serves the UI/API itself. Every setting is an environment variable;
all have defaults, so a bare `sudo ./setup.sh` re-run is the update path.
Recognized variables:

  GRAIN_REPO_URL           git remote to deploy from (default: bwsalmon/grain on GitHub)
  GRAIN_REF                branch to build (default: main)
  GRAIN_SRC_DIR             where the checkout lives (default: /opt/grain)
  GRAIN_DATA_DIR            secrets/store/sandbox root (default: /var/lib/grain)
  GRAIN_USER                unprivileged account grain runs as (default: grain)

  GRAIN_UI_ADDR             UI/API bind address (default: 127.0.0.1:80 -- loopback
                             only; reach it with `ssh -L 8080:localhost:80 host`,
                             or put it behind Tailscale/IAP instead)
  GRAIN_MAX_CONCURRENT      maximum number of tasks dispatched at once (default: 1)
  GRAIN_POLL_INTERVAL       daemon reconcile-cycle interval (default: 30s)

  GRAIN_GITHUB_HOST         GitHub API host (default: github.com)
  GRAIN_GITHUB_INSECURE_HTTP  1 to speak plain HTTP to it (mock servers only)
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

  GRAIN_GEMINI_API_KEY      Gemini API key to seed, once. Required for
                             grain-daemon.service to actually start, but
                             optional when GRAIN_GCP_PROJECT is set and a
                             minter credential is available (seeded from
                             GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE below): the
                             minter then mints the daemon's own key here --
                             see mint_gemini_operating_key
  GRAIN_GEMINI_MODEL        override the daemon's default Gemini model
  GRAIN_MAX_AGENT_TURNS     cap on model/tool round trips per run. Empty leaves
                             the framework's own default (20), which a real task
                             can exhaust: reading a few files, writing one, running
                             a test and then add/commit/push are each a turn, and
                             the run fails outright rather than finishing short

  GRAIN_GCP_PROJECT                  enables the gcp-key/gemini-key capabilities
  GRAIN_GCP_SERVICE_ACCOUNT_EMAIL    the narrow agent service account they mint for
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE a minter key to seed under secrets/gcp-key-minter/

  GRAIN_TARGET_REPO         owner/name: the UI's default target for a task with
                             no repo of its own, and the repo this script pushes
                             one empty commit to if it has no commits yet
  GRAIN_TARGET_BRANCH       branch to create there if formatting it (default: main)
  GRAIN_TARGET_REPOS        comma-separated owner/name list a task's repo may
                             name (default: empty, meaning unrestricted) -- the
                             daemon's own -target-repos, the allowlist a task
                             naming anything else is parked with a comment
                             rather than dispatched against

  GRAIN_ENABLE_UI_UPGRADE   1 (default) to wire up the UI's own Upgrade
                             button (bwsalmon/agents#396); set to 0 on a
                             deployment shape that already has its own
                             rollout mechanism (e.g. terraform/gcp-v2's
                             config-sync.sh/deploy.sh), so the two cannot
                             race or drift out of sync with each other

  GRAIN_KONTUR_ENABLE        1 to dispatch onto real bwsalmon/kontur-managed
                             VMs over SSH (orchestrator.KonturSandboxes)
                             instead of host directories (default: 0). Needs
                             a real guest image and OCI image already
                             published -- see packer/kontur/README.md and
                             this repo's terraform/gcp-v2/README.md, "Kontur
                             sandboxing" -- and /dev/kvm on this host
                             (nested virtualization). Left off (with a
                             logged reason) if any prerequisite below is
                             missing, rather than failing the whole run.
  GRAIN_KONTUR_IMAGE_BUCKET  GCS bucket packer/kontur/build.sh's own
                             KONTUR_IMAGE_BUCKET published the guest image
                             to; this script fetches its "latest" alias
  GRAIN_KONTUR_OCI_IMAGE     full reference of the pre-built kontur OCI image
                             (third_party/kontur's own Dockerfile), pulled
                             and retagged to konturctl's own default image
  GRAIN_KONTUR_IMAGES_HOSTPATH  where the fetched guest image lands, bind-
                             mounted read-only into each VM's container
                             (default: /var/lib/vm-images, konturctl's own default)
  GRAIN_KONTUR_DISK_HOSTPATH  host directory each kontur VM's own private,
                             writable qcow2 disk overlay is created under
                             (default: /var/lib/kontur/vm-disks, konturctl's
                             own default) -- without this, a VM's root
                             filesystem is read-only, since
                             GRAIN_KONTUR_IMAGES_HOSTPATH always is
  GRAIN_KONTUR_VM_NAME_PREFIX  prefix for each slot's kontur VM name (default: kontur-)
  GRAIN_KONTUR_SSH_USER      username to SSH into each kontur VM as (default: debian)
  GRAIN_KONTUR_SSH_KEY_FILE  path to the private half of the SSH keypair whose
                             public half is baked into the guest image; seeded
                             once into $GRAIN_DATA_DIR/secrets/kontur-ssh-key
  GRAIN_KONTUR_WORKSPACE     working directory tools operate in on each kontur
                             VM (default: /home/debian, GRAIN_KONTUR_SSH_USER's own home)
  GRAIN_KONTUR_BASE_IP       "-ip" slot 1's kontur VM gets; every later slot's
                             is the next address after it (default: 169.254.100.10)
  GRAIN_KONTUR_BASE_PORT     "-port" slot 1's kontur VM forwards; every later
                             slot's is this plus its own number minus one
                             (default: 12000)
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

for cmd in git docker systemctl install useradd visudo; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "setup.sh: required command not found: $cmd" >&2
    exit 1
  fi
done

# make is the odd one out, so this installs it rather than only reporting
# it missing like the loop above. build_and_install runs `make -C v2
# container-build` and the UI's own Upgrade button runs the same target
# (v2/cmd/grain/daemon.go's BuildCmd), but the compile itself happens
# inside Docker -- so make is the only piece of that toolchain the host
# needs, which is exactly why nobody lists it, and a Debian cloud image
# carries none.
#
# terraform/gcp-v2/files/deploy.sh installs it too, and that was supposed
# to be enough. It is not, because of how each file reaches a host:
# deploy.sh arrives in instance metadata, rewritten only by a
# `terraform apply`, while this script arrives in the git checkout it
# updates itself (sync_repo). A host whose metadata predates that
# deploy.sh fix has no make and no way to get one -- every deploy dies
# here -- until someone runs Terraform, which is not something a machine
# that only ever pulls from git can do for itself. Installing it here
# closes that loop: the fix travels the same path as the script that
# needs it.
ensure_make() {
  command -v make >/dev/null 2>&1 && return 0
  if command -v apt-get >/dev/null 2>&1; then
    log "installing make (the build step runs 'make -C v2 container-build'; no Debian cloud image carries it)"
    apt-get update -qq || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends make || true
  fi
  if ! command -v make >/dev/null 2>&1; then
    echo "setup.sh: required command not found: make, and it could not be installed automatically -- install it (e.g. 'apt-get install make') and re-run" >&2
    exit 1
  fi
}
ensure_make

if ! docker info >/dev/null 2>&1; then
  echo "setup.sh: 'docker info' failed -- is the Docker daemon running? (only needed to build, via make container-build)" >&2
  exit 1
fi

# --- 1. clone or update the checkout -----------------------------------

sync_repo() {
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
# `make: command not found` from build_and_install, with the check that
# exists to name that failure sitting unread on disk a few inches away.
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

# --- 2/3. build and install the binary ----------------------------------
#
# The real binary lives at $GRAIN_DATA_DIR/bin/grain -- inside the
# directory ensure_self_upgrade below gives $GRAIN_USER ownership of --
# rather than /usr/local/bin/grain directly, so a later upgrade this same
# unprivileged account drives (v2/pkg/upgrade.Upgrader.install) can
# overwrite it with no sudo of its own. /usr/local/bin/grain is a
# symlink to that path instead: an operator's shell still finds `grain`
# on PATH, and the symlink itself never has to change again once
# created, so it needs no further privilege past this first run either.
REAL_BIN="$GRAIN_DATA_DIR/bin/grain"

build_and_install() {
  log "Building bin/grain (make container-build -- needs only docker, not a host Go toolchain)"
  make -C "$GRAIN_SRC_DIR/v2" container-build
  mkdir -p "$(dirname "$REAL_BIN")"
  install -m0755 "$GRAIN_SRC_DIR/v2/bin/grain" "$REAL_BIN"
  ln -sf "$REAL_BIN" /usr/local/bin/grain
  log "Installed $REAL_BIN (linked from /usr/local/bin/grain)"
  write_cli_profile
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
# no getent guard is needed the way ensure_self_upgrade's docker one is.
ensure_user() {
  if ! id -u "$GRAIN_USER" >/dev/null 2>&1; then
    log "Creating system user $GRAIN_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$GRAIN_USER"
  fi
  usermod -aG systemd-journal "$GRAIN_USER"
}

# --- 5. sudo to reboot the machine, for the UI's reboot button ----------
#
# grant_reboot_sudo lets the UI's "reboot host" button (v2/pkg/ui/host.go,
# bwsalmon/agents#395) actually reboot the machine: grain-daemon.service
# runs as the unprivileged $GRAIN_USER (ensure_user, above), which cannot
# reboot anything on its own. The drop-in grants exactly one command
# line, matched verbatim by sudoers -- never a blanket NOPASSWD -- the
# same shape ../../provision/controller.sh already uses for v1's own
# self-repair sudoers file, and the same command rebootHost
# (cmd/grain/daemon.go) runs.
grant_reboot_sudo() {
  log "Granting $GRAIN_USER passwordless sudo to reboot this machine"
  cat > /etc/sudoers.d/grain-daemon-reboot <<SUDOERS
${GRAIN_USER} ALL=(root) NOPASSWD: /usr/bin/systemctl reboot
SUDOERS
  chmod 0440 /etc/sudoers.d/grain-daemon-reboot
  visudo -cf /etc/sudoers.d/grain-daemon-reboot
}

# ensure_self_upgrade gives $GRAIN_USER everything the UI's own Upgrade
# button (bwsalmon/agents#396) needs and nothing beyond it: ownership of
# its own checkout (so it can fetch/checkout/build there), membership in
# the docker group (so `make container-build` needs no sudo of its own),
# and one exact, NOPASSWD sudoers line to restart its own service --
# never a wildcard, matching provision/controller.sh's own "software
# gate, not infra gate" self-repair grant. It cannot restart anything
# else; rebooting the host outright is grant_reboot_sudo's separate,
# narrower grant above.
#
# Skipped entirely when GRAIN_ENABLE_UI_UPGRADE=0: none of this grant is
# needed if write_systemd_units below never wires up the flags that let
# the daemon touch its own checkout or restart itself in the first place
# (bwsalmon/agents#405).
#
# Run after ensure_user (the account has to exist) and again on every
# re-run: chown -R is cheap against an already-correctly-owned tree, and
# a previous run's build_and_install (root, via sudo) would otherwise
# leave $GRAIN_SRC_DIR's freshly fetched/built files root-owned again on
# every single upgrade.
# grant_docker_group adds $GRAIN_USER to the docker group if it exists --
# shared by ensure_self_upgrade (make container-build needs it with no
# sudo of its own) and ensure_kontur_kvm_access (the docker kontur backend
# needs it to run each slot's VM container), so either feature alone is
# enough to grant it rather than each needing its own copy of the
# getent-guard idiom.
grant_docker_group() {
  if getent group docker >/dev/null 2>&1; then
    usermod -aG docker "$GRAIN_USER"
  fi
}

ensure_self_upgrade() {
  if [ "$GRAIN_ENABLE_UI_UPGRADE" != "1" ]; then
    log "GRAIN_ENABLE_UI_UPGRADE=$GRAIN_ENABLE_UI_UPGRADE -- skipping the UI Upgrade button's self-upgrade grant"
    return
  fi
  chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_SRC_DIR"
  grant_docker_group

  cat > /etc/sudoers.d/grain-daemon-upgrade <<SUDOERS
$GRAIN_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart grain-daemon.service
SUDOERS
  chmod 0440 /etc/sudoers.d/grain-daemon-upgrade
  visudo -cf /etc/sudoers.d/grain-daemon-upgrade
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
# authenticate docker to Artifact Registry (ensure_kontur_images) without
# installing the whole gcloud SDK just for those two things. iam.tf's own
# host_reads_kontur_images/host_reads_kontur_registry are what make the
# token itself actually able to do either.
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

# ensure_kontur_images fetches the guest image build.sh published
# (packer/kontur/build.sh's own "latest" alias -- see that script's own
# comment on why one exists) and pulls+retags the OCI image
# (third_party/kontur's own Dockerfile) -- both needed before
# write_systemd_units can wire up -kontur-create-arg with anything real
# for `konturctl vm create` to actually find. Never installs or builds
# either itself: that is packer/kontur/build.sh and a plain `docker
# build`/`docker push` against third_party/kontur, run once, separately,
# ahead of any deploy (terraform/gcp-v2/README.md, "Kontur sandboxing") --
# this only ever fetches what that step already published.
ensure_kontur_images() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ -z "$GRAIN_KONTUR_IMAGE_BUCKET" ] || [ -z "$GRAIN_KONTUR_OCI_IMAGE" ]; then
    log "GRAIN_KONTUR_ENABLE=1 but GRAIN_KONTUR_IMAGE_BUCKET/GRAIN_KONTUR_OCI_IMAGE are not both set -- leaving kontur sandboxing off this run"
    GRAIN_KONTUR_ENABLE=0
    return
  fi

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

# ensure_kontur_kvm_access grants $GRAIN_USER /dev/kvm (for the nested
# cloud-hypervisor guest itself), the docker group (for the docker
# kontur backend's own `docker run`) -- the same grant
# ensure_self_upgrade gives for an unrelated reason, and safe to grant
# twice: usermod -aG only ever adds -- and ownership of
# GRAIN_KONTUR_DISK_HOSTPATH, where konturctl (run directly as
# $GRAIN_USER, not inside a container) creates each VM's own writable
# disk overlay (bwsalmon/agents#510). Unlike GRAIN_KONTUR_IMAGES_HOSTPATH
# (populated by this script, running as root, before grain-daemon ever
# runs), that directory is created lazily by konturctl itself on a VM's
# first "vm create" -- $GRAIN_USER needs to own its parent so that
# mkdir succeeds unprivileged.
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
}

# seed_kontur_ssh_key writes GRAIN_KONTUR_SSH_KEY_FILE's contents to
# $GRAIN_DATA_DIR/secrets/kontur-ssh-key, the same never-overwrite
# contract seed_secret gives every other plain-file secret -- reused
# directly here since an SSH private key is just another multi-line
# value, nothing seed_secret's own printf '%s' needs to treat specially.
seed_kontur_ssh_key() {
  if [ "$GRAIN_KONTUR_ENABLE" != "1" ]; then
    return
  fi
  if [ -z "$GRAIN_KONTUR_SSH_KEY_FILE" ] || [ ! -s "$GRAIN_KONTUR_SSH_KEY_FILE" ]; then
    log "GRAIN_KONTUR_ENABLE=1 but GRAIN_KONTUR_SSH_KEY_FILE is not set (or empty) -- leaving kontur sandboxing off this run; see push-secrets.sh's own GRAIN_KONTUR_SSH_KEY"
    GRAIN_KONTUR_ENABLE=0
    return
  fi
  seed_secret "$GRAIN_DATA_DIR/secrets/kontur-ssh-key" "$(cat "$GRAIN_KONTUR_SSH_KEY_FILE")"
}

# --- 6. data directory and secrets --------------------------------------

seed_secret() {
  # Writes $2 to file $1 only if it is missing or empty, and only if a
  # value was actually given -- never overwrites a credential a previous
  # run (or an operator by hand) already placed, and never writes an
  # empty file for a value nobody supplied this time.
  local path="$1" value="$2"
  if [ -s "$path" ]; then
    return
  fi
  if [ -z "$value" ]; then
    return
  fi
  ( umask 077; printf '%s' "$value" > "$path" )
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

setup_data_dir() {
  log "Laying out $GRAIN_DATA_DIR"
  install -d -m0750 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR"
  # build_and_install (root, above) may have just (re-)created this on a
  # fresh -data-dir before this function ever ran; re-asserting
  # ownership here is what lets $GRAIN_USER overwrite its own binary on
  # every later upgrade with no sudo of its own -- install -d re-applies
  # -o/-g even against a directory that already exists (this file's own
  # comment on sync_repo's mkdir-vs-install-d distinction).
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/bin"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github"
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/sandbox"
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
  /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" set \
    -value-file "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" gcp-key-minter key.json
  chown -R "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
}

# mint_gemini_operating_key mints the daemon's own Gemini API key, using
# the minter credential seed_gcp_minter_key just placed, when no key is
# in place yet.
#
# A deployment that grants its minter roles/serviceusage.apiKeysAdmin
# (terraform/gcp-v2's enable_gemini_key, on by default there) already has
# every permission this needs, on this host -- so an operator does not
# also have to paste a Gemini key in by hand before grain-daemon.service
# will start. Where that grant is absent the mint simply fails, and this
# stays exactly the "install but stay stopped" state the deploy path
# already handles: it must never fail the whole converge, since the
# GitHub side of a deployment is useful without it and a half-applied
# setup.sh is worse than a stopped daemon.
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
    log "  could not mint a Gemini API key -- the daemon will install but stay stopped."
    log "  Check the minter credential holds roles/serviceusage.apiKeysAdmin, or set"
    log "  GRAIN_GEMINI_API_KEY and re-run."
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

write_systemd_units() {
  log "Writing grain-daemon.service"

  local daemon_args=(
    daemon
    -data-dir "$GRAIN_DATA_DIR"
    -max-concurrent "$GRAIN_MAX_CONCURRENT"
    -poll-interval "$GRAIN_POLL_INTERVAL"
    -gemini-api-key-file "$GRAIN_DATA_DIR/secrets/gemini-api-key"
    -github-host "$GRAIN_GITHUB_HOST"
    -ui-addr "$GRAIN_UI_ADDR"
  )
  # bwsalmon/agents#396: the UI's own Upgrade button. Wired up whenever
  # GRAIN_ENABLE_UI_UPGRADE=1 (the default) -- ensure_self_upgrade
  # (above) is what actually gives $GRAIN_USER the ownership and the one
  # sudoers line this needs, run every time this script is, so the
  # feature works the same whether this is a first install or the
  # hundredth re-run. Left unset when GRAIN_ENABLE_UI_UPGRADE=0
  # (terraform/gcp-v2's own deploy.sh sets exactly that): the daemon
  # flags themselves default to empty/disabled (cmd/grain/daemon.go), so
  # simply not passing them is enough -- see this script's own header on
  # GRAIN_ENABLE_UI_UPGRADE (bwsalmon/agents#405).
  if [ "$GRAIN_ENABLE_UI_UPGRADE" = "1" ]; then
    daemon_args+=(
      -upgrade-src-dir "$GRAIN_SRC_DIR"
      -upgrade-install-path "$REAL_BIN"
      -upgrade-restart-cmd sudo -upgrade-restart-cmd systemctl -upgrade-restart-cmd restart -upgrade-restart-cmd grain-daemon.service
    )
  fi
  [ -n "$GRAIN_GEMINI_MODEL" ] && daemon_args+=(-gemini-model "$GRAIN_GEMINI_MODEL")
  [ -n "$GRAIN_MAX_AGENT_TURNS" ] && daemon_args+=(-max-agent-turns "$GRAIN_MAX_AGENT_TURNS")
  [ "$GRAIN_GITHUB_INSECURE_HTTP" = "1" ] && daemon_args+=(-github-insecure-http)
  [ -n "$GRAIN_GCP_PROJECT" ] && daemon_args+=(-gcp-project "$GRAIN_GCP_PROJECT")
  [ -n "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL" ] && daemon_args+=(-gcp-agent-service-account "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL")
  [ -n "$GRAIN_TARGET_REPO" ] && daemon_args+=(-default-target-repo "$GRAIN_TARGET_REPO")
  [ -n "$GRAIN_TARGET_REPOS" ] && daemon_args+=(-target-repos "$GRAIN_TARGET_REPOS")

  # -kontur-vm-name-prefix is what actually selects orchestrator.
  # KonturSandboxes over HostSandboxes (cmd/grain/daemon.go's run()) --
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
  # destination directory, not a version string this script has to track.
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
      -kontur-vm-name-prefix "$GRAIN_KONTUR_VM_NAME_PREFIX"
      -kontur-backend docker
      -kontur-ssh-user "$GRAIN_KONTUR_SSH_USER"
      -kontur-ssh-key "$GRAIN_DATA_DIR/secrets/kontur-ssh-key"
      -kontur-workspace "$GRAIN_KONTUR_WORKSPACE"
      -kontur-base-ip "$GRAIN_KONTUR_BASE_IP"
      -kontur-base-port "$GRAIN_KONTUR_BASE_PORT"
      -kontur-create-arg -images-hostpath -kontur-create-arg "$GRAIN_KONTUR_IMAGES_HOSTPATH"
      -kontur-create-arg -disk -kontur-create-arg /images/current/disk.img
      -kontur-create-arg -kernel -kontur-create-arg /images/current/vmlinuz
      -kontur-create-arg -initramfs -kontur-create-arg /images/current/initrd.img
      -kontur-create-arg -guest-port -kontur-create-arg 22
      -kontur-create-arg -disk-readonly=false
      -kontur-create-arg -disk-hostpath -kontur-create-arg "$GRAIN_KONTUR_DISK_HOSTPATH"
    )
  fi

  cat > /etc/systemd/system/grain-daemon.service <<UNIT
[Unit]
Description=grain daemon (task orchestrator, UI and API)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${GRAIN_USER}
Group=${GRAIN_USER}
# CAP_NET_BIND_SERVICE, not root, is what lets an unprivileged process
# bind -ui-addr's port 80 -- see this script's own header on why that's
# the port and why it's bound to loopback only.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ExecStart=/usr/local/bin/grain ${daemon_args[*]}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
}

enable_services() {
  # enable, then restart -- not "enable --now". An already-enabled unit
  # from a previous run of this script needs restarting to pick up a
  # rebuilt binary or a config change; --now would leave an already-
  # running one exactly as it was. v1 hit precisely this bug for its own
  # git-proxy service (docs/next-session.md item 3's "Update"): restarting
  # an already-running unit is always safe, and starting a stopped one is
  # exactly what --now would have done anyway.
  systemctl enable grain-daemon.service >/dev/null
  if [ -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ]; then
    systemctl restart grain-daemon.service
  else
    log "grain-daemon.service is enabled but not started -- it needs a Gemini API key first."
    log "  Set GRAIN_GEMINI_API_KEY and re-run this script, or place one at"
    log "  $GRAIN_DATA_DIR/secrets/gemini-api-key and run: systemctl restart grain-daemon.service"
    log "  A deployment whose minter holds roles/serviceusage.apiKeysAdmin can mint one"
    log "  instead: set GRAIN_GCP_PROJECT and GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE and re-run."
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
  local github="MISSING" gemini="MISSING" minter="MISSING" daemon ready=1

  if [ -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] \
     && [ -s "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" ]; then
    github="present as '${GRAIN_GITHUB_CREDENTIAL_NAME}'"
  fi
  [ -s "$GRAIN_DATA_DIR/secrets/gemini-api-key" ] && gemini="present"
  if /usr/local/bin/grain secrets -data-dir "$GRAIN_DATA_DIR" list 2>/dev/null \
     | grep -q '^gcp-key-minter:'; then
    minter="present"
  fi
  daemon="$(systemctl is-active grain-daemon.service 2>/dev/null || echo unknown)"

  echo
  log "Readiness:"
  echo "    daemon:            $daemon"
  echo "    GitHub credential: $github"
  echo "    Gemini key:        $gemini"
  echo "    GCP minter key:    $minter"
  echo "    target repos:      ${GRAIN_TARGET_REPOS:-<none: every task parks>}"
  echo "    default repo:      ${GRAIN_TARGET_REPO:-<none: a task with no repo parks>}"
  echo "    max concurrent:    ${GRAIN_MAX_CONCURRENT:-<default>}"
  if [ "$GRAIN_KONTUR_ENABLE" = "1" ]; then
    echo "    sandboxing:        kontur VMs (prefix '${GRAIN_KONTUR_VM_NAME_PREFIX}', over SSH as ${GRAIN_KONTUR_SSH_USER})"
  else
    echo "    sandboxing:        host directories (orchestrator.HostSandboxes)"
  fi

  if [ "$github" = "MISSING" ]; then
    ready=0
    echo "    !! With no GitHub credential the git proxy cannot clone. A dispatched run"
    echo "       finds an empty sandbox and ends without pushing or asking anything."
  fi
  if [ "$gemini" = "MISSING" ]; then
    ready=0
    echo "    !! With no Gemini key grain-daemon.service will not start, so nothing is"
    echo "       served on ${GRAIN_UI_ADDR} and no task is ever dispatched."
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
  echo "    CLI:     grain list  (a new shell picks up GRAIN_SERVER from /etc/profile.d/grain.sh;"
  echo "             in this one: export GRAIN_SERVER=http://127.0.0.1:${GRAIN_UI_ADDR##*:})"
  echo "    Logs:    journalctl -u grain-daemon.service -f"
  echo "    Update:  re-run this script (sudo ./setup.sh) -- it pulls, rebuilds, and restarts the service"
  report_readiness
}

main() {
  sync_repo
  reexec_if_updated "$@"
  build_and_install
  ensure_user
  grant_reboot_sudo
  ensure_self_upgrade
  ensure_kontur_images
  ensure_kontur_kvm_access
  setup_data_dir
  reformat_store_if_schema_changed
  format_target_repo_if_empty
  write_systemd_units
  enable_services
  print_summary
}

main "$@"
