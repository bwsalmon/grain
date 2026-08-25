#!/usr/bin/env bash
# Runs on the host, as root, once per config generation. Idempotent: it
# converges the host onto whatever `grain-config` in instance metadata now
# says, and a re-run after a failure resumes rather than starting over --
# the same property `grain host bootstrap` has.
#
# Nothing here is secret. The credentials it needs arrive as instance
# metadata -- pushed straight there by the deploy workflow, read back with
# no GCP credential at all -- and they exist on disk only inside /run
# (tmpfs), 0600, for the seconds it takes grain to place them on the
# controller's /data.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly DATA_MNT="/var/lib/grain"
readonly SRC="/opt/grain-src"
readonly IMAGE_DIR="$DATA_MNT/images"
readonly IMAGE_PATH="$IMAGE_DIR/debian-12.qcow2"
readonly RUNDIR="/run/grain-deploy"
readonly CLUSTER_FILE="$DATA_MNT/cluster.toml"
readonly GITHUB_TOKEN_ATTR="grain-github-token"
readonly CLAUDE_TOKEN_ATTR="grain-claude-token"
# Minted fresh by the deploy workflow on every run that has an agent
# service account to mint one for -- never a long-lived Actions secret, so
# there is no wait budget worth calling "required": if
# agent_service_account_email is unset, one will just never arrive.
readonly GCP_KEY_ATTR="grain-agent-service-account-key"
readonly SECRET_WAIT_REQUIRED=600
readonly SECRET_WAIT_OPTIONAL=180

log() { echo "deploy: $*"; }
die() { echo "deploy: FATAL: $*" >&2; exit 1; }

# curl's own --retry only covers curl; git has nothing equivalent, so
# network-touching git commands get the same 3-attempts/5s-apart shape by
# hand. A transient egress gap right at boot -- Cloud NAT not yet
# programmed, a DNS blip -- would otherwise fail sync_source permanently
# on its very first command, with no self-heal until config-sync's own
# ~5-minute retry cycle came back around.
retry() {
  local attempt=1 max=3 delay=5
  until "$@"; do
    if [ "$attempt" -ge "$max" ]; then
      return 1
    fi
    log "retrying ($attempt/$max): $*"
    attempt=$((attempt + 1))
    sleep "$delay"
  done
}

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

cleanup() { rm -rf "$RUNDIR"; }
trap cleanup EXIT

# ------------------------------------------------------------------ config --

load_config() {
  # 077 guards the config blob and env.sh this writes into $RUNDIR -- both
  # carry deployment configuration, and env.sh is sourced. It is restored
  # before returning, which is the whole point: found live, leaving it set
  # leaked into sync_source's `git clone`, so /opt/grain-src came out
  # 0700/0600, `deploy_tree` faithfully copied those modes onto the
  # controller's /opt/grain, and every dispatched task unit then died on
  # `cd /opt/grain: Permission denied` before `claude -p` ever started --
  # surfacing as task issues flapping between the trigger and in-progress
  # labels once a minute as the sweeper requeued each instant failure.
  # `die` exits the script, so the failure paths below need no restore.
  local prior_umask
  prior_umask="$(umask)"
  umask 077
  mkdir -p "$RUNDIR"
  md instance/attributes/grain-config > "$RUNDIR/config.json" \
    || die "no grain-config in instance metadata"

  # One python pass renders both the shell variables this script needs and
  # grain's cluster.toml, so the mapping from repo config to on-host files
  # lives in exactly one place.
  python3 - <<'PY' || die "grain-config is not valid"
import json, shlex

cfg = json.load(open("/run/grain-deploy/config.json"))

def sh(name, value):
    return f"{name}={shlex.quote(str(value))}\n"

out = ""
for key in ("grain_repo_url", "grain_ref", "debian_image_url",
            "task_repo", "default_target_repo", "credential_name",
            "bootstrap_ssh_timeout_seconds", "agent_service_account_email",
            "gemini_project_id"):
    out += sh(key.upper(), cfg.get(key, "") or "")
targets = cfg.get("target_repos") or []
out += "TARGET_REPOS=(" + " ".join(shlex.quote(t) for t in targets) + ")\n"
open("/run/grain-deploy/env.sh", "w").write(out)

# cluster.toml: grain reads every key it recognises here and falls back to
# its own defaults for the rest.
def toml_value(v):
    s = str(v)
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return s
    try:
        float(s)
        return s
    except ValueError:
        return json.dumps(s)

lines = ["# Written by the grain config repo's deploy.sh. Edits are overwritten.",
         f"sandbox_count = {int(cfg.get('sandbox_count', 2))}",
         f"image = {json.dumps('/var/lib/grain/images/debian-12.qcow2')}"]
for key, value in sorted((cfg.get("cluster_overrides") or {}).items()):
    if key in ("sandbox_count", "image"):
        continue
    lines.append(f"{key} = {toml_value(value)}")
open("/run/grain-deploy/cluster.toml", "w").write("\n".join(lines) + "\n")
PY

  # shellcheck source=/dev/null
  . "$RUNDIR/env.sh"
  [ -n "$TASK_REPO" ] || die "task_repo is empty; set it in config/grain.tfvars"
  umask "$prior_umask"
}

# ------------------------------------------------------------------- host ---

require_data_disk() {
  mountpoint -q "$DATA_MNT" \
    || die "$DATA_MNT is not a mountpoint; the startup script should have mounted the data disk"
  mkdir -p "$IMAGE_DIR"
}

require_kvm() {
  [ -e /dev/kvm ] \
    || die "/dev/kvm is missing -- nested virtualization is not enabled on this instance"
}

ensure_packages() {
  local pkgs=(qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients
              cloud-image-utils nftables python3 git openssh-client curl ca-certificates)
  local missing=()
  local p
  for p in "${pkgs[@]}"; do
    dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "^install ok installed$" || missing+=("$p")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    log "installing: ${missing[*]}"
    # Boot-time unattended-upgrades holds the dpkg lock; wait rather than fail.
    apt-get -o DPkg::Lock::Timeout=600 -qq update
    DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 -qq -y install "${missing[@]}"
  fi
  systemctl enable --now libvirtd >/dev/null 2>&1 || true
}

sync_source() {
  log "syncing $GRAIN_REPO_URL @ $GRAIN_REF"
  if [ ! -d "$SRC/.git" ]; then
    rm -rf "$SRC"
    retry git clone --quiet "$GRAIN_REPO_URL" "$SRC" \
      || die "could not clone $GRAIN_REPO_URL after 3 attempts"
  fi
  git -C "$SRC" remote set-url origin "$GRAIN_REPO_URL"
  retry git -C "$SRC" fetch --quiet --prune --tags origin \
    || die "could not fetch $GRAIN_REPO_URL after 3 attempts"
  # grain_ref may be a branch, a tag, or a commit.
  if git -C "$SRC" rev-parse --verify --quiet "origin/$GRAIN_REF^{commit}" >/dev/null; then
    git -C "$SRC" checkout --quiet --detach "origin/$GRAIN_REF"
  elif git -C "$SRC" rev-parse --verify --quiet "$GRAIN_REF^{commit}" >/dev/null; then
    git -C "$SRC" checkout --quiet --detach "$GRAIN_REF"
  else
    die "grain_ref '$GRAIN_REF' is not a branch, tag, or commit of $GRAIN_REPO_URL"
  fi
  # Converge the modes too, not just the contents: a host whose checkout
  # was made by an earlier build of this script -- under the umask that
  # used to leak out of load_config -- keeps 0700/0600 forever otherwise,
  # since the clone above only runs when .git is missing. Nothing here is
  # secret: $GRAIN_REPO_URL is fetched anonymously and carries no
  # credential, and no secret is ever written under $SRC.
  chmod -R u=rwX,go=rX "$SRC"
  log "grain at $(git -C "$SRC" rev-parse --short HEAD)"
}

fetch_base_image() {
  if [ -s "$IMAGE_PATH" ]; then
    return
  fi
  log "fetching base image (once): $DEBIAN_IMAGE_URL"
  curl -fL --retry 3 --retry-delay 5 -o "$IMAGE_PATH.partial" "$DEBIAN_IMAGE_URL" \
    || die "could not fetch the Debian base image"
  mv "$IMAGE_PATH.partial" "$IMAGE_PATH"
}

write_cluster_file() {
  install -m 0644 "$RUNDIR/cluster.toml" "$CLUSTER_FILE"
  log "cluster.toml: $(tr '\n' ' ' < "$CLUSTER_FILE")"
}

# ---------------------------------------------------------------- secrets ---

# The deploy workflow adds these to instance metadata with `gcloud compute
# instances add-metadata` right after the instance exists, so on a first
# deploy the key can be a few seconds behind config-sync waking up. Wait,
# rather than fail a race.
fetch_secret_to_file() {
  local attr="$1" path="$2" budget="$3" waited=0
  while true; do
    if md "instance/attributes/$attr" > "$path" 2>/dev/null && [ -s "$path" ]; then
      chmod 0600 "$path"
      return 0
    fi
    rm -f "$path"
    if [ "$waited" -ge "$budget" ]; then
      return 1
    fi
    log "metadata key '$attr' not set yet; waiting ($waited/${budget}s)"
    sleep 15
    waited=$((waited + 15))
  done
}

# ---------------------------------------------------------------- bootstrap -

# Found live: a real deploy kept failing with "Cannot access storage file
# ... Permission denied" for a VM disk under $DATA_MNT/instances, and every
# operator who hit it lacked osLoginExternalUser -- so SSH/IAP was a dead
# end for checking the actual file owner vs. what libvirt's qemu.conf
# expects. This surfaces the same facts through the one channel that was
# actually reachable: Cloud Logging, via deploy.sh's own stdout.
dump_storage_diagnostics() {
  log "--- storage diagnostics: $DATA_MNT/instances ---"
  ls -la "$DATA_MNT/instances" 2>&1 | while IFS= read -r line; do log "  $line"; done

  local qemu_user qemu_group
  qemu_user=$(awk -F'"' '/^[[:space:]]*user[[:space:]]*=/{print $2; exit}' /etc/libvirt/qemu.conf)
  qemu_group=$(awk -F'"' '/^[[:space:]]*group[[:space:]]*=/{print $2; exit}' /etc/libvirt/qemu.conf)
  log "qemu.conf: user='${qemu_user:-<unset, defaults to root>}' group='${qemu_group:-<unset, defaults to root>}'"
  grep -E '^[[:space:]]*dynamic_ownership' /etc/libvirt/qemu.conf 2>&1 \
    | while IFS= read -r line; do log "  qemu.conf: $line"; done

  if [ -n "$qemu_user" ]; then
    getent passwd "$qemu_user" 2>&1 | while IFS= read -r line; do log "  passwd: $line"; done
  fi
  if [ -n "$qemu_group" ]; then
    getent group "$qemu_group" 2>&1 | while IFS= read -r line; do log "  group: $line"; done
  fi
}

run_bootstrap() {
  local gh_file="$RUNDIR/github.token"
  local claude_file="$RUNDIR/claude.token"
  local gcp_key_file="$RUNDIR/gcp-service-account.json"
  local args=(--cluster-file "$CLUSTER_FILE" host bootstrap --task-repo "$TASK_REPO")
  local repo

  if [ -n "$BOOTSTRAP_SSH_TIMEOUT_SECONDS" ]; then
    args+=(--ssh-timeout "$BOOTSTRAP_SSH_TIMEOUT_SECONDS")
  fi

  fetch_secret_to_file "$GITHUB_TOKEN_ATTR" "$gh_file" "$SECRET_WAIT_REQUIRED" \
    || die "no '$GITHUB_TOKEN_ATTR' in instance metadata; set GRAIN_GITHUB_TOKEN in the repo's Actions secrets"

  for repo in "${TARGET_REPOS[@]}"; do
    args+=(--target-repo "$repo")
  done
  if [ -n "$DEFAULT_TARGET_REPO" ]; then
    args+=(--default-target-repo "$DEFAULT_TARGET_REPO")
  fi
  if [ -n "$CREDENTIAL_NAME" ]; then
    args+=(--credential-name "$CREDENTIAL_NAME")
  fi
  args+=(--github-token-file "$gh_file")

  if fetch_secret_to_file "$CLAUDE_TOKEN_ATTR" "$claude_file" "$SECRET_WAIT_OPTIONAL"; then
    args+=(--claude-token-file "$claude_file")
  else
    log "WARNING: no Claude Code OAuth token in instance metadata; deploying without one."
    log "         Set GRAIN_CLAUDE_CODE_OAUTH_TOKEN in Actions secrets and push again;"
    log "         until then the automation service cannot dispatch a task."
  fi

  # agent_service_account_email is non-secret config (published in
  # grain-config, like task_repo); the key itself is the one part of this
  # that is secret, minted fresh by the deploy workflow and pushed
  # separately. Empty email means agent_service_account_roles was never
  # set -- no key will ever arrive, so there is nothing to wait for.
  if [ -n "$AGENT_SERVICE_ACCOUNT_EMAIL" ]; then
    if fetch_secret_to_file "$GCP_KEY_ATTR" "$gcp_key_file" "$SECRET_WAIT_OPTIONAL"; then
      args+=(--gcp-service-account-key-file "$gcp_key_file"
              --gcp-agent-service-account-email "$AGENT_SERVICE_ACCOUNT_EMAIL"
              --gcp-project-id "$(md project/project-id)")
      # gemini_project_id (terraform/gcp's enable_gemini_key) reuses the
      # same key just placed above -- grain/automation/gemini_keys.py's own
      # docstring on why one primary key covers both. Only meaningful once
      # that key actually arrived, hence nested here rather than gated on
      # GEMINI_PROJECT_ID alone.
      if [ -n "$GEMINI_PROJECT_ID" ]; then
        args+=(--gemini-project-id "$GEMINI_PROJECT_ID")
      fi
    else
      log "WARNING: agent_service_account_roles is set but no GCP service account key"
      log "         was found in instance metadata; sandboxed agents will have no GCP access."
    fi
  fi

  # The token *paths* are fine to log; the files themselves are 0600 on tmpfs.
  log "running: python3 -m grain.cli ${args[*]}"
  if ! ( cd "$SRC" && python3 -m grain.cli "${args[@]}" ); then
    dump_storage_diagnostics
    return 1
  fi
}

# -------------------------------------------------------------------- main --

log "starting"
load_config
require_data_disk
require_kvm
ensure_packages
sync_source
fetch_base_image
write_cluster_file
run_bootstrap
log "converged"
