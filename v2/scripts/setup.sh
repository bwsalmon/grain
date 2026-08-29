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
#   1. clones or updates this repo under $GRAIN_SRC_DIR
#   2. builds bin/grain with `make container-build` (v2/Makefile) --
#      the containerised build, not the host toolchain, which is the
#      one thing this script assumes: a working `docker`. That sidesteps
#      the whole ICU/glibc version-coupling problem the Makefile's own
#      comments describe, without asking this script to know anything
#      about the host's package manager.
#   3. installs it to /usr/local/bin/grain
#   4. creates an unprivileged system user to run it as
#   5. lays out $GRAIN_DATA_DIR (secrets, the sandbox root, the embedded
#      store) and seeds secrets from environment variables -- only if
#      they are not already there, so a second run never overwrites a
#      credential placed by the first one or by hand
#   6. if $GRAIN_TARGET_REPO is set and has no commits yet, pushes one
#      empty commit so grain has a branch to work from ("format" it --
#      grain always branches off an existing ref, never creates one)
#   7. writes and enables grain-daemon.service, so it comes back on
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
# There used to be a second service (grain-ui.service) and a `dolt
# sql-server` container behind it: embedded Dolt is single-writer, and a
# daemon plus a UI on the same store both used to need to write.
# bwsalmon/agents#363 removed the second writer -- the daemon now serves
# the UI/API itself, in-process, over the store it already has open (see
# v2/cmd/grain/daemon.go's own doc comment) -- so this script installs
# one service, needs no Dolt server container, and no longer requires
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
GRAIN_SLOTS="${GRAIN_SLOTS:-local}"
GRAIN_POLL_INTERVAL="${GRAIN_POLL_INTERVAL:-30s}"

GRAIN_GITHUB_HOST="${GRAIN_GITHUB_HOST:-github.com}"
GRAIN_GITHUB_INSECURE_HTTP="${GRAIN_GITHUB_INSECURE_HTTP:-0}"
GRAIN_GITHUB_TOKEN="${GRAIN_GITHUB_TOKEN:-}"
GRAIN_GITHUB_CREDENTIAL_NAME="${GRAIN_GITHUB_CREDENTIAL_NAME:-bot}"

GRAIN_GEMINI_API_KEY="${GRAIN_GEMINI_API_KEY:-}"
GRAIN_GEMINI_MODEL="${GRAIN_GEMINI_MODEL:-}"

GRAIN_GCP_PROJECT="${GRAIN_GCP_PROJECT:-}"
GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="${GRAIN_GCP_SERVICE_ACCOUNT_EMAIL:-}"
GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="${GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE:-}"

GRAIN_TARGET_REPO="${GRAIN_TARGET_REPO:-}"
GRAIN_TARGET_BRANCH="${GRAIN_TARGET_BRANCH:-main}"

usage() {
  cat <<'EOF'
Usage: sudo ./setup.sh

Installs or updates a v2 grain deployment on this machine: clones/builds
the binary, lays out /var/lib/grain (including its embedded task store),
and installs grain-daemon.service, which runs the dispatch loop and
serves the UI/API itself. Every setting is an environment variable; all
have defaults, so a bare `sudo ./setup.sh` re-run is the update path.
Recognized variables:

  GRAIN_REPO_URL           git remote to deploy from (default: bwsalmon/grain on GitHub)
  GRAIN_REF                branch to build (default: main)
  GRAIN_SRC_DIR             where the checkout lives (default: /opt/grain)
  GRAIN_DATA_DIR            secrets/store/sandbox root (default: /var/lib/grain)
  GRAIN_USER                unprivileged account grain runs as (default: grain)

  GRAIN_UI_ADDR             UI/API bind address (default: 127.0.0.1:80 -- loopback
                             only; reach it with `ssh -L 8080:localhost:80 host`,
                             or put it behind Tailscale/IAP instead)
  GRAIN_SLOTS               comma-separated concurrency slots (default: local)
  GRAIN_POLL_INTERVAL       daemon reconcile-cycle interval (default: 30s)

  GRAIN_GITHUB_HOST         GitHub API host (default: github.com)
  GRAIN_GITHUB_INSECURE_HTTP  1 to speak plain HTTP to it (mock servers only)
  GRAIN_GITHUB_TOKEN        a token to seed the credential ladder with, once
                             (only written if no credential is configured yet)
  GRAIN_GITHUB_CREDENTIAL_NAME  name to store that token under (default: bot)

  GRAIN_GEMINI_API_KEY      Gemini API key to seed, once (required for
                             grain-daemon.service to actually start)
  GRAIN_GEMINI_MODEL        override the daemon's default Gemini model

  GRAIN_GCP_PROJECT                  enables the gcp-key/gemini-key capabilities
  GRAIN_GCP_SERVICE_ACCOUNT_EMAIL    the narrow agent service account they mint for
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE a minter key to seed under secrets/gcp-key-minter/

  GRAIN_TARGET_REPO         owner/name: the UI's default target for a task with
                             no repo of its own, and the repo this script pushes
                             one empty commit to if it has no commits yet
  GRAIN_TARGET_BRANCH       branch to create there if formatting it (default: main)
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

for cmd in git docker systemctl install useradd; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "setup.sh: required command not found: $cmd" >&2
    exit 1
  fi
done
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

# --- 2/3. build and install the binary ----------------------------------

build_and_install() {
  log "Building bin/grain (make container-build -- needs only docker, not a host Go toolchain)"
  make -C "$GRAIN_SRC_DIR/v2" container-build
  install -m0755 "$GRAIN_SRC_DIR/v2/bin/grain" /usr/local/bin/grain
  log "Installed /usr/local/bin/grain"
}

# --- 4. the unprivileged account grain runs as --------------------------

ensure_user() {
  if ! id -u "$GRAIN_USER" >/dev/null 2>&1; then
    log "Creating system user $GRAIN_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$GRAIN_USER"
  fi
}

# --- 5. data directory and secrets --------------------------------------

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

setup_data_dir() {
  log "Laying out $GRAIN_DATA_DIR"
  install -d -m0750 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets"
  install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github"
  install -d -m0755 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/sandbox"
  # grain-daemon.service's own -data-dir/store, embedded Dolt -- the one
  # process that ever opens it now (this file's own header on
  # bwsalmon/agents#363), so no separate sql-server container or
  # directory layout is needed for it beyond what openStore creates on
  # its own the first time the daemon starts.

  # GitHub credential ladder (v2/pkg/gitproxy/credentials.go): a pattern
  # file plus one <name>.token per credential. "*" is the catch-all every
  # repo falls back to absent a narrower entry -- an operator wanting a
  # per-repo credential edits credentials.json and adds another
  # <name>.token by hand; this script only ever seeds the one default.
  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ] && [ -n "$GRAIN_GITHUB_TOKEN" ]; then
    printf '{"*":"%s"}\n' "$GRAIN_GITHUB_CREDENTIAL_NAME" > "$GRAIN_DATA_DIR/secrets/github/credentials.json"
    chown "$GRAIN_USER:$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/github/credentials.json"
  fi
  seed_secret "$GRAIN_DATA_DIR/secrets/github/${GRAIN_GITHUB_CREDENTIAL_NAME}.token" "$GRAIN_GITHUB_TOKEN"

  seed_secret "$GRAIN_DATA_DIR/secrets/gemini-api-key" "$GRAIN_GEMINI_API_KEY"

  if [ -n "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" ] && [ ! -s "$GRAIN_DATA_DIR/secrets/gcp-key-minter/key.json" ]; then
    install -d -m0700 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_DATA_DIR/secrets/gcp-key-minter"
    install -m0600 -o "$GRAIN_USER" -g "$GRAIN_USER" "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" "$GRAIN_DATA_DIR/secrets/gcp-key-minter/key.json"
  fi

  if [ ! -s "$GRAIN_DATA_DIR/secrets/github/credentials.json" ]; then
    log "  no GitHub credential configured yet -- set GRAIN_GITHUB_TOKEN and re-run, or place"
    log "  $GRAIN_DATA_DIR/secrets/github/credentials.json and a matching .token file by hand"
  fi
}

# --- 6. format the target repo, if it is empty --------------------------
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

# --- 7. the systemd unit ---------------------------------------------------

write_systemd_units() {
  log "Writing grain-daemon.service"

  local daemon_args=(
    daemon
    -data-dir "$GRAIN_DATA_DIR"
    -slots "$GRAIN_SLOTS"
    -poll-interval "$GRAIN_POLL_INTERVAL"
    -gemini-api-key-file "$GRAIN_DATA_DIR/secrets/gemini-api-key"
    -github-host "$GRAIN_GITHUB_HOST"
    -ui-addr "$GRAIN_UI_ADDR"
  )
  [ -n "$GRAIN_GEMINI_MODEL" ] && daemon_args+=(-gemini-model "$GRAIN_GEMINI_MODEL")
  [ "$GRAIN_GITHUB_INSECURE_HTTP" = "1" ] && daemon_args+=(-github-insecure-http)
  [ -n "$GRAIN_GCP_PROJECT" ] && daemon_args+=(-gcp-project "$GRAIN_GCP_PROJECT")
  [ -n "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL" ] && daemon_args+=(-gcp-agent-service-account "$GRAIN_GCP_SERVICE_ACCOUNT_EMAIL")
  [ -n "$GRAIN_TARGET_REPO" ] && daemon_args+=(-default-target-repo "$GRAIN_TARGET_REPO")

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
  fi
}

print_summary() {
  echo
  log "Done."
  echo "    UI:      http://${GRAIN_UI_ADDR} -- reach it with: ssh -L 8080:localhost:${GRAIN_UI_ADDR##*:} <this-host>, then open http://localhost:8080"
  echo "    Store:   embedded Dolt under ${GRAIN_DATA_DIR}/store, owned by grain-daemon.service alone"
  echo "    Secrets: ${GRAIN_DATA_DIR}/secrets"
  echo "    Logs:    journalctl -u grain-daemon.service -f"
  echo "    Update:  re-run this script (sudo ./setup.sh) -- it pulls, rebuilds, and restarts the service"
}

main() {
  sync_repo
  build_and_install
  ensure_user
  setup_data_dir
  format_target_repo_if_empty
  write_systemd_units
  enable_services
  print_summary
}

main
