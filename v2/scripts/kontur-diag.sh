#!/usr/bin/env bash
# kontur-diag.sh -- bisect "grain's kontur sandbox VM is up but SSH does not
# answer" down to the single hop that is actually broken.
#
# Reaching a kontur sandbox VM under the docker backend (the default, see
# v2/cmd/grain/daemon.go's -kontur-backend) is a chain of five hops, and
# every one of them fails with the same outward symptom -- a TCP connect to
# <pod IP>:<port> that hangs or is refused:
#
#   host -> netns container's docker IP ("the pod IP")
#        -> netshim's nftables DNAT (<pod IP>:<port> -> <guest IP>:<guestPort>)
#        -> the tap device / bridge cloud-hypervisor attached the guest to
#        -> the guest's own eth0, addressed from the "ip=" kernel cmdline
#        -> sshd listening inside the guest
#
# This script walks those hops in order and reports the first one that
# fails, with the raw evidence underneath, so the next step is a fact
# rather than a guess.
#
# Everything here is read-only: it inspects state, containers, the netns
# and the guest's console log, and opens TCP connections. It changes
# nothing. Run it as root on the host running the sandbox VMs.
#
# Usage:
#   sudo v2/scripts/kontur-diag.sh [vm-name]
#
# With no vm-name it diagnoses every VM in the state directory. Override
# the state directory with KONTUR_STATE_DIR (default /var/lib/kontur/vms,
# matching pkg/kontur.DefaultStateDir), and the SSH identity/user the
# final end-to-end check uses with KONTUR_SSH_KEY/KONTUR_SSH_USER (the
# same values grain's own -kontur-ssh-key/-kontur-ssh-user carry; the SSH
# check is skipped when no key is given).

set -uo pipefail

STATE_DIR="${KONTUR_STATE_DIR:-/var/lib/kontur/vms}"
SSH_KEY="${KONTUR_SSH_KEY:-}"
SSH_USER="${KONTUR_SSH_USER:-debian}"
CONNECT_TIMEOUT="${KONTUR_CONNECT_TIMEOUT:-5}"

fail_count=0

say()  { printf '%s\n' "$*"; }
hdr()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  [ OK ] %s\n' "$*"; }
bad()  { printf '  [FAIL] %s\n' "$*"; fail_count=$((fail_count + 1)); }
warn() { printf '  [WARN] %s\n' "$*"; }
info() { printf '         %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || { say "kontur-diag: required tool '$1' is not installed"; exit 2; }
}
have() { command -v "$1" >/dev/null 2>&1; }

need docker
have jq || { say "kontur-diag: 'jq' is required to read kontur's state JSON"; exit 2; }

# tcp_connect HOST PORT -- succeeds if a TCP connection is established
# within CONNECT_TIMEOUT. Uses bash's own /dev/tcp rather than nc, which
# is not installed on every host and, more importantly, is not needed:
# this has to work under `nsenter -n` too, where only the network
# namespace changes and the binary still comes from the host.
tcp_connect() {
  timeout "$CONNECT_TIMEOUT" bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null
}

# in_netns PID CMD... -- run a host binary inside the netns container's
# network namespace.
in_netns() { nsenter -t "$1" -n "${@:2}"; }

diagnose() {
  local name="$1"
  local spec="$STATE_DIR/$name.json"

  hdr "VM $name"

  # ---- Hop 0: kontur's own saved spec -----------------------------------
  # grain never re-creates a VM whose state file it can already read
  # (orchestrator.KonturSandboxes.ensure treats that as "already exists"),
  # so a VM created before -guest-port 22 was passed keeps konturctl's own
  # default of 80 forever, and every DNAT'd connection lands on a port
  # nothing in the guest listens on.
  if [ ! -f "$spec" ]; then
    bad "no saved spec at $spec"
    return
  fi
  local ip port guest_port backend cmdline vm_c netns_c
  ip="$(jq -r '.ip // ""' "$spec")"
  port="$(jq -r '.port // 0' "$spec")"
  guest_port="$(jq -r '.guestPort // 0' "$spec")"
  backend="$(jq -r '.backend // "static-pod"' "$spec")"
  cmdline="$(jq -r '.cmdline // ""' "$spec")"
  say "  spec: ip=$ip port=$port guestPort=$guest_port backend=$backend"
  say "  cmdline: ${cmdline:-<none>}"

  if [ "$backend" != "docker" ]; then
    warn "backend is $backend, not docker -- this script only diagnoses the docker backend"
    return
  fi
  if [ "$guest_port" = "22" ]; then
    ok "guestPort is 22 (the port the guest image's sshd actually listens on)"
  else
    bad "guestPort is $guest_port, not 22 -- netshim DNATs $port to $ip:$guest_port, where nothing is listening"
    info "konturctl's own default guestPort is 80. grain must pass"
    info "  -kontur-create-arg=-guest-port -kontur-create-arg=22"
    info "and this VM predates that (grain reuses any VM whose state file exists"
    info "rather than re-creating it -- see KonturSandboxes.ensure). Fix by deleting"
    info "and letting grain recreate it:  konturctl vm delete $name -state-dir $STATE_DIR"
  fi
  case "$cmdline" in
    *"ip=$ip::"*) ok "cmdline carries the guest's own static address (ip=$ip::...)" ;;
    "")           bad "cmdline is empty -- the guest gets no ip= and will never address eth0" ;;
    *)            warn "cmdline's ip= does not start with $ip -- guest and DNAT target may disagree" ;;
  esac
  case "$cmdline" in
    *:eth0:*) ok "cmdline names eth0 (the name the guest image pins via 00-eth0.link)" ;;
    *)        warn "cmdline does not name eth0 -- ipconfig(8) in the guest configures nothing" ;;
  esac

  # ---- Hop 1: the two containers ----------------------------------------
  vm_c="kontur-vm-$name"
  netns_c="kontur-vm-$name-netns"
  local vm_status netns_status
  vm_status="$(docker inspect -f '{{.State.Status}}' "$vm_c" 2>/dev/null)"
  netns_status="$(docker inspect -f '{{.State.Status}}' "$netns_c" 2>/dev/null)"
  if [ "$netns_status" = "running" ]; then
    ok "netns holder $netns_c is running"
  else
    bad "netns holder $netns_c is ${netns_status:-absent} -- the shared network namespace is gone"
    return
  fi
  if [ "$vm_status" = "running" ]; then
    ok "VM container $vm_c is running"
  else
    bad "VM container $vm_c is ${vm_status:-absent} (exit $(docker inspect -f '{{.State.ExitCode}}' "$vm_c" 2>/dev/null))"
    info "last console output:"
    docker logs --tail 25 "$vm_c" 2>&1 | sed 's/^/           /'
    return
  fi

  local pid
  pid="$(docker inspect -f '{{.State.Pid}}' "$netns_c" 2>/dev/null)"
  if [ -z "$pid" ] || [ "$pid" = "0" ]; then
    bad "cannot resolve $netns_c's PID to enter its network namespace"
    return
  fi

  # ---- Hop 2: the shared network namespace ------------------------------
  local pod_ip tap
  pod_ip="$(docker inspect -f '{{$i := index .NetworkSettings "IPAddress"}}{{if $i}}{{$i}}{{else}}{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}{{end}}' "$netns_c" 2>/dev/null)"
  tap="tap-$name"
  say "  pod IP (what grain dials): ${pod_ip:-<none>}"
  info "links in the shared netns:"
  in_netns "$pid" ip -br addr 2>&1 | sed 's/^/           /'

  if in_netns "$pid" ip link show kontur0 >/dev/null 2>&1; then
    ok "netshim's bridge kontur0 exists"
  else
    bad "no kontur0 bridge in the netns -- netshim never ran, or ran against a different namespace"
  fi

  # A tap device reports NO-CARRIER until something opens its file
  # descriptor. cloud-hypervisor holding it open is precisely what makes
  # the guest reachable, so this is the cheapest proof that the VMM
  # actually attached to the tap netshim built for it.
  if in_netns "$pid" ip link show "$tap" >/dev/null 2>&1; then
    local tapline
    tapline="$(in_netns "$pid" ip -br link show "$tap" 2>/dev/null)"
    if printf '%s' "$tapline" | grep -q 'NO-CARRIER'; then
      bad "$tap has NO-CARRIER -- cloud-hypervisor is not attached to it"
      info "$tapline"
      info "the guest has no link at all; check the VM container's console log below"
    else
      ok "$tap is up with carrier (cloud-hypervisor is attached)"
    fi
    local master
    master="$(in_netns "$pid" ip -o link show "$tap" 2>/dev/null | grep -o 'master [^ ]*' | awk '{print $2}')"
    if [ "$master" = "kontur0" ]; then
      ok "$tap is enslaved to kontur0"
    else
      bad "$tap's master is ${master:-<none>}, not kontur0 -- guest traffic never reaches the bridge"
    fi
  else
    bad "no $tap device in the netns -- netshim did not create this VM's tap"
  fi

  # ---- Hop 3: netshim's DNAT rules --------------------------------------
  if have nft; then
    local rules
    rules="$(in_netns "$pid" nft list table ip kontur 2>&1)"
    if printf '%s' "$rules" | grep -q 'chain prerouting'; then
      ok "netshim's nftables table 'kontur' is installed"
      if printf '%s' "$rules" | grep -q "dnat to $ip:$guest_port"; then
        ok "DNAT rule targets $ip:$guest_port"
      else
        bad "no 'dnat to $ip:$guest_port' rule -- the forward does not point where the spec says"
      fi
      if printf '%s' "$rules" | grep -q "dport $port"; then
        ok "a DNAT rule matches dport $port"
      else
        bad "no rule matching dport $port -- nothing forwards the port grain dials"
      fi
      # The prerouting rule additionally matches the pod IP netshim saw on
      # eth0 when it ran. Docker hands out a new IP on every container
      # recreate, so a stale rule silently matches nothing.
      if [ -n "$pod_ip" ] && printf '%s' "$rules" | grep -q "daddr $pod_ip"; then
        ok "DNAT matches the netns container's current IP ($pod_ip)"
      elif [ -n "$pod_ip" ]; then
        bad "DNAT matches a different address than $pod_ip -- netshim ran against a since-replaced container IP"
        info "$(printf '%s' "$rules" | grep -o 'daddr [0-9.]*' | sort -u | tr '\n' ' ')"
      fi
    else
      bad "no nftables table 'kontur' in the netns -- netshim's rules are missing entirely"
      info "$rules"
    fi
  else
    warn "'nft' is not installed on the host -- skipping the DNAT rule check"
  fi

  # ---- Hop 4: the guest itself, straight off the bridge -----------------
  # This is the decisive split. Reaching the guest directly at its bridge
  # address bypasses every DNAT rule above: if this works and the pod-IP
  # check below does not, the bug is in netshim's rules; if this fails
  # too, the guest's own networking or sshd never came up and the rules
  # are irrelevant.
  if in_netns "$pid" ping -c 2 -W 2 "$ip" >/dev/null 2>&1; then
    ok "guest answers ICMP at $ip (its eth0 is configured)"
  else
    bad "guest does not answer ping at $ip -- its eth0 never got an address"
    info "the guest image configures eth0 from the ip= cmdline via the"
    info "kontur-net-cmdline.service oneshot (packer/kontur/guest-setup.sh)."
    info "grep the console log for it:  docker logs $vm_c 2>&1 | grep -i -e ipconfig -e eth0 -e kontur-net"
  fi

  if in_netns "$pid" timeout "$CONNECT_TIMEOUT" bash -c "exec 3<>/dev/tcp/$ip/$guest_port" 2>/dev/null; then
    ok "sshd answers directly at $ip:$guest_port (guest networking and sshd are both fine)"
  else
    bad "no TCP answer at $ip:$guest_port straight off the bridge"
    info "if ping above succeeded, the guest is addressed but sshd is not listening;"
    info "if ping failed too, the guest has no working network at all."
  fi

  # ---- Hop 5: through the DNAT, from inside then outside the netns ------
  # netshim installs the same DNAT in both prerouting and output, because
  # prerouting never sees traffic originated inside the pod's own netns.
  # Testing both separates a broken output rule from a broken prerouting
  # rule -- they fail independently.
  if [ -n "$pod_ip" ]; then
    if in_netns "$pid" timeout "$CONNECT_TIMEOUT" bash -c "exec 3<>/dev/tcp/$pod_ip/$port" 2>/dev/null; then
      ok "$pod_ip:$port answers from inside the netns (output-chain DNAT works)"
    else
      bad "$pod_ip:$port does not answer from inside the netns (output-chain DNAT)"
    fi
    if tcp_connect "$pod_ip" "$port"; then
      ok "$pod_ip:$port answers from the host (prerouting DNAT works -- this is what grain dials)"
    else
      bad "$pod_ip:$port does not answer from the host (prerouting DNAT)"
    fi
  fi

  # Dialing port 22 on the pod IP is expected to fail and is not a bug:
  # netshim only ever forwards the VM's own external port, and the netns
  # holder itself runs "kontur sleep", which listens on nothing.
  if [ -n "$pod_ip" ] && [ "$port" != "22" ]; then
    if tcp_connect "$pod_ip" 22; then
      warn "$pod_ip:22 answers -- unexpected, nothing in kontur forwards port 22 on the pod IP"
    else
      info "note: $pod_ip:22 not answering is expected -- netshim forwards only port $port,"
      info "and the netns holder container itself listens on nothing. Dial $pod_ip:$port."
    fi
  fi

  # ---- Hop 6: end to end, the way grain does it -------------------------
  if [ -n "$SSH_KEY" ] && [ -n "$pod_ip" ]; then
    if ssh -i "$SSH_KEY" -p "$port" \
        -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout="$CONNECT_TIMEOUT" \
        "$SSH_USER@$pod_ip" true 2>/tmp/kontur-diag-ssh.$$; then
      ok "ssh $SSH_USER@$pod_ip:$port succeeds -- this VM is fully reachable"
    else
      bad "ssh $SSH_USER@$pod_ip:$port failed"
      sed 's/^/           /' /tmp/kontur-diag-ssh.$$
    fi
    rm -f /tmp/kontur-diag-ssh.$$
  else
    info "set KONTUR_SSH_KEY (and KONTUR_SSH_USER) to also run the end-to-end ssh check"
  fi

  # ---- Guest console: the evidence for every hop-4 failure --------------
  hdr "VM $name: guest console markers"
  local log
  log="$(docker logs "$vm_c" 2>&1)"
  for marker in 'kontur-net-cmdline' 'ipconfig' 'eth0' 'sshd' 'Kernel panic' 'Cannot open root device'; do
    local hit
    hit="$(printf '%s\n' "$log" | grep -i -- "$marker" | tail -3)"
    if [ -n "$hit" ]; then
      say "  --- $marker ---"
      printf '%s\n' "$hit" | sed 's/^/      /'
    else
      say "  --- $marker: no match in console output ---"
    fi
  done
  say "  (full console: docker logs $vm_c)"
}

if [ "$#" -gt 0 ]; then
  for n in "$@"; do diagnose "$n"; done
else
  shopt -s nullglob
  specs=("$STATE_DIR"/*.json)
  if [ "${#specs[@]}" -eq 0 ]; then
    say "kontur-diag: no VM specs in $STATE_DIR -- grain has not created a sandbox VM yet"
    exit 1
  fi
  for s in "${specs[@]}"; do
    n="$(basename "$s" .json)"
    diagnose "$n"
  done
fi

hdr "summary"
if [ "$fail_count" -eq 0 ]; then
  say "  no failures -- every hop from the host to the guest's sshd checks out"
else
  say "  $fail_count check(s) failed -- fix the first [FAIL] above; the later ones are usually downstream of it"
fi
exit $(( fail_count > 0 ? 1 : 0 ))
