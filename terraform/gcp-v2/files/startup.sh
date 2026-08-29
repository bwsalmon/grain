#!/usr/bin/env bash
# GCE startup script. Runs as root on every boot, and must be idempotent.
#
# Mirrors terraform/gcp/files/startup.sh's v1 shape exactly: mount the
# data disk, install and enable config-sync, and nothing else. All the
# real work is in deploy.sh, which config-sync fetches from instance
# metadata so it can change without recreating the instance.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly DATA_DEV="/dev/disk/by-id/google-grain-v2-data"
readonly DATA_MNT="/var/lib/grain"
readonly SYNC="/opt/grain-deploy/config-sync.sh"

log() { echo "grain-v2-startup: $*"; }

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

  cat > /etc/systemd/system/grain-v2-config-sync.service <<'UNIT'
[Unit]
Description=Watch instance metadata and redeploy grain v2 when it changes
After=network-online.target google-guest-agent.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/grain-deploy/config-sync.sh
Restart=always
RestartSec=10
# The deploy writes /var/lib/grain, creates a system user, and reads
# secrets from its own instance metadata. It needs to be root.
User=root

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable --now grain-v2-config-sync.service
  log "grain-v2-config-sync.service enabled"
}

mount_data_disk
install_config_sync
log "done; watch the rollout with: journalctl -u grain-v2-config-sync -f"
