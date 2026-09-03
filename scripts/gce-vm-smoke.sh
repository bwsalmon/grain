#!/usr/bin/env bash
# gce-vm-smoke.sh -- prove that a GCP credential can actually run the GCE
# instance lifecycle: create a throwaway VM, SSH into it, delete it again.
#
# This is the "can we deploy at all" check for a credential, not a test of
# anything in grain. terraform/gcp stands a deployment up out of exactly
# these three verbs, and a `gcp-key` capability grant (pkg/capability/gcpkey)
# is supposed to buy a sandboxed agent the same. Both fail in ways that read
# as something else entirely when the credential is short a role -- see the
# three gotchas below, each of which cost a run of task 195 to diagnose -- so
# it is worth being able to answer "is the credential good?" in ninety
# seconds, on its own, before blaming terraform or the agent.
#
# Usage:
#   scripts/gce-vm-smoke.sh [--zone Z] [--network N] [--subnet S] [--iap] [--keep]
#
# It authenticates as whatever gcloud already has active. From inside a
# grain sandbox holding the gcp-key capability, that means first:
#
#   gcloud auth activate-service-account --key-file=~/.gcp-service-account.json
#   gcloud config set project <project>
#
# Activating a key does not select a project, and gcloud's complaint when
# you skip the second line ("The required property [project] is not
# currently set") says nothing about the key -- so it gets misread as the
# key being bad.

set -euo pipefail

ZONE="${ZONE:-us-central1-a}"
NETWORK="${NETWORK:-default}"
SUBNET="${SUBNET:-}"
MACHINE_TYPE="${MACHINE_TYPE:-e2-micro}"
USE_IAP=0
KEEP=0

while [[ $# -gt 0 ]]; do
  # Accept --flag=value as well as --flag value. gcloud's own flags take
  # both, so a caller who has just been typing gcloud commands reasonably
  # expects these to, and the failure otherwise is a bare usage line that
  # does not say which argument it disliked.
  case "$1" in
    --*=*) set -- "${1%%=*}" "${1#*=}" "${@:2}" ;;
  esac
  case "$1" in
    --zone)    ZONE="$2"; shift 2 ;;
    --network) NETWORK="$2"; shift 2 ;;
    # Required for any custom-subnet-mode network -- which is every network
    # terraform/gcp creates (network.tf) and every grain-* network in a real
    # deployment. Only the auto-mode `default` network can infer one, so
    # leaving this off elsewhere fails the create with
    # "Network interface must specify a subnet if the network resource is in
    # custom subnet mode", naming a field the caller never set.
    --subnet)  SUBNET="$2"; shift 2 ;;
    # Reach the VM over IAP TCP forwarding rather than an external IP. This
    # is the shape terraform/gcp actually deploys (network.tf gives the host
    # no external IP at all), so it is the leg worth checking before
    # trusting a credential with a real deployment. It needs more of the
    # project set up in advance than the default path does: the caller wants
    # roles/iap.tunnelResourceAccessor, and the network needs an ingress
    # rule allowing tcp:22 from 35.235.240.0/20, IAP's own range.
    --iap)     USE_IAP=1; shift ;;
    # Leave the VM running -- for poking at a failure by hand. It is then
    # yours to delete; the command is printed at the end.
    --keep)    KEEP=1; shift ;;
    *) echo "usage: $0 [--zone Z] [--network N] [--subnet S] [--iap] [--keep]" >&2; exit 2 ;;
  esac
done

PROJECT="$(gcloud config get-value project 2>/dev/null)"
if [[ -z "$PROJECT" || "$PROJECT" == "(unset)" ]]; then
  echo "no project set: gcloud config set project <project>" >&2
  exit 2
fi

VM="gce-smoke-$(date +%s)-$$"

step() { printf '\n=== %s ===\n' "$*"; }

# A leaked e2-micro is a slow, silent bill, and every path out of this
# script that is not --keep goes through here -- including set -e's, and
# including a Ctrl-C halfway through the SSH retry loop.
cleanup() {
  local rc=$?
  if [[ $KEEP -eq 1 ]]; then
    echo
    echo "--keep: VM $VM left running in $ZONE. Delete it with:"
    echo "  gcloud compute instances delete $VM --zone=$ZONE --quiet"
    return $rc
  fi
  if gcloud compute instances describe "$VM" --zone="$ZONE" >/dev/null 2>&1; then
    step "delete $VM"
    gcloud compute instances delete "$VM" --zone="$ZONE" --quiet
  fi
  return $rc
}
trap cleanup EXIT

CREATE_ARGS=(--network="$NETWORK")
[[ -n "$SUBNET" ]] && CREATE_ARGS+=(--subnet="$SUBNET")

step "create $VM ($MACHINE_TYPE, $NETWORK${SUBNET:+/$SUBNET}, $ZONE)"
# --no-service-account --no-scopes is not an optimisation, it is what makes
# this work as a non-owner. gcloud otherwise attaches the project's default
# compute service account to every VM it creates, and attaching *any*
# service account requires roles/iam.serviceAccountUser **on that account**
# -- a permission on the SA, which a project-level compute role does not
# include. Without it the create fails with
#
#   The user does not have access to service account
#   '<num>-compute@developer.gserviceaccount.com'
#
# which names an account the caller never asked for and reads like the
# credential itself is wrong. A smoke-test VM calls no Google API from
# inside, so it needs no identity of its own; declining one sidesteps the
# grant entirely.
gcloud compute instances create "$VM" \
  --zone="$ZONE" \
  --machine-type="$MACHINE_TYPE" \
  --image-family=debian-12 --image-project=debian-cloud \
  --boot-disk-size=10GB \
  "${CREATE_ARGS[@]}" \
  --no-service-account --no-scopes \
  --labels=purpose=gce-vm-smoke

SSH_ARGS=(--zone="$ZONE" --quiet)
[[ $USE_IAP -eq 1 ]] && SSH_ARGS+=(--tunnel-through-iap)

step "ssh into $VM$([[ $USE_IAP -eq 1 ]] && echo ' (via IAP)')"
# Two things make the first attempts here noisy without anything being wrong,
# and both are worth expecting rather than debugging:
#
#   "Updating project ssh metadata... failed." -- gcloud publishes the
#   generated key to the *project's* common instance metadata first, which
#   needs compute.projects.setCommonInstanceMetadata. A project-level
#   compute admin role does not carry it. gcloud then falls back to the
#   instance's own metadata on its own, which does work, and which is the
#   better outcome anyway: the key is scoped to this one VM and is destroyed
#   with it instead of accumulating on the project.
#
#   "Connection refused" -- the API reports RUNNING as soon as the VM is
#   scheduled, well before the guest has booted far enough to start sshd.
#   There is nothing to poll for that means "sshd is listening", so retry.
#   Under --iap the same "too early" shows up as a wall of Python traceback
#   ending in [4003: 'failed to connect to backend'], which reads like a
#   missing IAP grant or firewall rule but clears on its own once sshd is
#   up. Judge either only by whether the loop below ever succeeds.
ssh_ok=0
for attempt in 1 2 3 4 5 6; do
  echo "--- attempt $attempt ---"
  if gcloud compute ssh "$VM" "${SSH_ARGS[@]}" \
      --command='echo SSH_OK; hostname; uptime' \
      -- -o StrictHostKeyChecking=no -o ConnectTimeout=15; then
    ssh_ok=1
    break
  fi
  sleep 15
done

if [[ $ssh_ok -ne 1 ]]; then
  echo "ssh never succeeded after 6 attempts" >&2
  # The EXIT trap still deletes the VM unless --keep was passed.
  exit 1
fi

# Deletion is the third verb under test, so run it here where a failure is
# reported as one, rather than leaving it to the trap's best-effort cleanup.
if [[ $KEEP -eq 0 ]]; then
  step "delete $VM"
  gcloud compute instances delete "$VM" --zone="$ZONE" --quiet
  # The boot disk goes with the instance (auto-delete is on by default for a
  # disk created inline like this one). Confirm it, because a leaked disk
  # bills like a leaked VM and is easier to miss.
  if gcloud compute disks describe "$VM" --zone="$ZONE" >/dev/null 2>&1; then
    echo "boot disk $VM outlived the instance -- delete it by hand" >&2
    exit 1
  fi
fi

# Say only what actually ran: under --keep the delete is skipped, and
# claiming it passed would make the one flag that leaves a VM behind the
# one that reports the cleanest.
printf '\nOK: create, ssh%s%s as %s in %s.\n' \
  "$([[ $USE_IAP -eq 1 ]] && echo ' (IAP)')" \
  "$([[ $KEEP -eq 1 ]] && echo ' succeeded; delete skipped (--keep)' || echo ' and delete all succeeded')" \
  "$(gcloud config get-value account 2>/dev/null)" "$PROJECT"
