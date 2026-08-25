#!/usr/bin/env bash
# GCE startup script. Runs as root on every boot, and must be idempotent.
#
# It does the two things that only make sense at boot -- get the data disk
# mounted, get the config-sync service running -- and nothing else. All the
# real work is in deploy.sh, which config-sync fetches from instance
# metadata so it can change without recreating the instance.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly DATA_DEV="/dev/disk/by-id/google-grain-data"
readonly DATA_MNT="/var/lib/grain"
readonly SYNC="/opt/grain-deploy/config-sync.sh"

log() { echo "grain-startup: $*"; }

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

mount_data_disk() {
  local uuid
  for _ in $(seq 1 60); do
    if [ -b "$DATA_DEV" ]; then break; fi
    sleep 1
  done
  if [ ! -b "$DATA_DEV" ]; then
    log "FATAL: $DATA_DEV never appeared; is the data disk attached?"
    exit 1
  fi

  if ! blkid -o value -s TYPE "$DATA_DEV" >/dev/null 2>&1; then
    log "formatting $DATA_DEV (first boot)"
    mkfs.ext4 -F -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$DATA_DEV"
  fi

  uuid="$(blkid -o value -s UUID "$DATA_DEV")"
  mkdir -p "$DATA_MNT"
  if ! grep -q "^UUID=$uuid " /etc/fstab; then
    log "adding $DATA_MNT to /etc/fstab"
    echo "UUID=$uuid $DATA_MNT ext4 defaults,discard,nofail 0 2" >> /etc/fstab
  fi
  mountpoint -q "$DATA_MNT" || mount "$DATA_MNT"
  log "$DATA_MNT mounted ($(df -h --output=size,avail "$DATA_MNT" | tail -1 | tr -s ' '))"
}

install_config_sync() {
  install -d -m 0700 /opt/grain-deploy
  md instance/attributes/grain-config-sync-script > "$SYNC.new"
  install -m 0700 "$SYNC.new" "$SYNC"
  rm -f "$SYNC.new"

  cat > /etc/systemd/system/grain-config-sync.service <<'UNIT'
[Unit]
Description=Watch instance metadata and redeploy grain when the config repo changes
After=network-online.target google-guest-agent.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/grain-deploy/config-sync.sh
Restart=always
RestartSec=10
# The deploy drives libvirt, writes /var/lib/grain, and reads secrets
# from its own instance metadata. It needs to be root.
User=root

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable --now grain-config-sync.service
  log "grain-config-sync.service enabled"
}

# Found live: a real deploy failure was undiagnosable from CI's own guest-
# attribute summary (a bare "exit=N"), and journalctl -u grain-config-sync
# needs an SSH/IAP path neither the deploy identity nor an operator may
# actually have. Cloud Logging sidesteps both -- readable from the Cloud
# Console in a browser, using nothing but whatever IAM the viewer's own
# account already has there. vm_service_account_roles already grants
# logging.logWriter by default; this is what actually uses it.
install_ops_agent() {
  if ! dpkg -s google-cloud-ops-agent >/dev/null 2>&1; then
    log "installing google-cloud-ops-agent"
    curl -sSO https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh
    bash add-google-cloud-ops-agent-repo.sh --also-install
    rm -f add-google-cloud-ops-agent-repo.sh
  fi

  # No unit-name filter at the receiver level (upstream doesn't offer one)
  # -- ships the whole systemd journal, filtered at query time instead:
  # jsonPayload._SYSTEMD_UNIT="grain-config-sync.service" in Cloud Logging.
  cat > /etc/google-cloud-ops-agent/config.yaml <<'YAML'
logging:
  receivers:
    journald:
      type: systemd_journald
  service:
    pipelines:
      default_pipeline:
        receivers: [journald]
YAML

  systemctl restart google-cloud-ops-agent
  log "google-cloud-ops-agent installed and forwarding the journal to Cloud Logging"
}

mount_data_disk
install_ops_agent
install_config_sync
log "done; watch the rollout with: journalctl -u grain-config-sync -f"
