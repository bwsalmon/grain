# Host adapter — implementation notes

The [design](design.md#the-host-adapter) puts everything platform-specific
behind one interface, so that moving between Linux and macOS costs a module
rather than a rewrite. This is that module.

```
grain/
  inventory.py          names, addresses, ports, specs — one source of truth
  run.py                command execution behind an interface (Real/DryRun/Fake)
  adapter/
    base.py             HostAdapter + Network interfaces, shared types
    lima.py             VM lifecycle via limactl — unused on Linux, see below
    libvirt.py          VM lifecycle via virsh — the Linux driver
    net_linux.py        bridge, nftables ruleset, metadata DNAT
  cli.py                `grain host …`
```

## Lima on Linux: verified, and rejected

Open question 1 asked whether Lima could attach a guest to a host bridge
with a fixed address on Linux. Answer: no, verified against Lima 2.2.0 on
the target host. `limactl create` rejects the exact `networks: - lima:
grain` stanza `lima.py` renders:

```
field `networks[0].lima` is only supported on macOS right now
```

`limactl network create grain --interface br-grain --mode bridged` happily
*writes* a network entry — the CLI doesn't validate against the runtime —
but Lima's bridged/shared/host modes are all implemented via
`socket_vmnet`, a macOS daemon (its own `networks.yaml` says so: "macOS
only; ignored on other platforms"). There is no code path that attaches a
Lima guest to an arbitrary existing Linux bridge with a fixed address.

`libvirt.py` is the replacement, per the design's own contingency. `lima.py`
is kept but unused on Linux — Lima's bridged mode is real on macOS, so it
may still serve as that platform's driver.

## The libvirt driver: two things that don't show up in unit tests

Both surfaced only by actually booting a guest — worth recording since nothing
in the unit tests (which mock the runner) would have caught either:

- **`virsh undefine --remove-all-storage` silently refuses plain files.**
  It only manages storage-pool volumes; a disk or seed ISO referenced by a
  bare path is left on disk with an error printed, not deleted. `destroy()`
  removes the files it created itself instead of relying on that flag.
- **The NoCloud seed must attach over virtio, not as a SATA/IDE cdrom.**
  Debian's cloud-optimized kernel has no AHCI/ATA driver at all — a
  `bus='sata' device='cdrom'` seed is never enumerated, so `blkid` and
  `ds-identify` see nothing and cloud-init never runs (no `/var/lib/cloud`,
  no `/var/log/cloud-init.log`, and even the default `debian` user is never
  created — confirmed by mounting the guest disk offline with `qemu-nbd`
  after a boot that never found a datasource). Attaching the same seed as a
  read-only `virtio-blk` disk (`bus='virtio'`) fixes it: cloud-init finds
  it immediately and configures the assigned static address from
  `network-config` before the first login prompt appears.

Verified live end-to-end on this host: a real KVM sandbox VM, attached to
`br-grain` via the exact tap name the firewall's anti-spoofing rules
expect, comes up at its inventory-assigned address
(`cloud-init`'s own boot log shows `eth0 10.100.0.10/24`, matching
`Cluster.address_of("sandbox-0")` exactly) and answers ping from the host.

## What is verified, and what is not

**Verified here**, by 49 tests including three that apply the real ruleset
to a real kernel:

- The nftables ruleset **parses and is accepted by the kernel** (`nft -c`
  and a live apply/teardown), is **idempotent on re-apply**, and switching
  egress mode removes the masquerade while keeping each sandbox's metadata
  route.
- Ordering: anti-spoofing precedes every accept; the intra-subnet drop
  follows the specific accepts. Both matter — an accept before the
  anti-spoof rules would let one sandbox impersonate another for that
  traffic, and a drop before the accepts would break the proxy.
- The negative cases the design insists on: no rule permits sandbox-to-
  sandbox traffic, and **no sandbox is permitted another sandbox's metadata
  port**. One test scans every accept rule for a sandbox-to-sandbox pair
  rather than trusting the intended rules to be the only ones.
- `address()` never shells out, so the firewall and the VMs cannot disagree
  about who is who.
- Lifecycle safety: `create` refuses to adopt an existing VM, `destroy` and
  `stop` are idempotent, and foreign Lima instances on the same host are
  ignored rather than managed.

**Now also verified**, once a hypervisor was available (see above for
detail): open question 1 (Lima on Linux) came back negative; the `libvirt`
driver that replaced it has booted a real Debian guest, attached it to
`br-grain` under its assigned tap name, and confirmed it answers at its
assigned address — including a full `virsh create → start → destroy` cycle
with no files left behind.

**Still not verified**:

- Lima's `list --json` field names and status strings — moot on Linux now,
  but relevant again if `lima.py` becomes the macOS driver.
- The controller VM: only a sandbox-shaped VM has been booted so far, not
  the controller image or its services.

## Using it

```sh
# Read the firewall policy without applying anything.
python3 -m grain.cli --sandboxes 2 host rules

# See every command a real run would execute, execute none of them.
python3 -m grain.cli --dry-run host up

# For real.
sudo python3 -m grain.cli host up
sudo python3 -m grain.cli host create all --provision provision/sandbox.sh
python3 -m grain.cli host status
```

`rules` and `--dry-run` exist for one reason: this program rewrites the
firewall of a machine you may only be able to reach *through* that firewall.
Reading the exact ruleset first is cheap.

## Two deliberate choices

**Addresses are assigned, never discovered.** The inventory decides them;
the adapter tells the VM. Asking the hypervisor afterwards would couple us
to its output format and, worse, would allow the firewall rules and the VMs
to disagree — which is exactly what the anti-spoofing rules exist to
prevent.

**The host's own INPUT chain is not managed.** On a cloud host the provider
firewall is the inbound control, and generating a default-drop INPUT policy
on a remote machine is a good way to lose the machine.
`render_host_input_rules` produces one for hosts that need it, and nothing
applies it automatically. A test asserts `network_up` never installs it.

## Extending it

The interface is deliberately small:

```python
create(spec, provision_script=None)   start(name)   stop(name)
destroy(name)   state(name)   list_vms()   address(name)
network_up(repo_dir)   egress_policy(mode)
```

`state` and `list_vms` are additions to the seven operations the design
named — the health check and the recreate flow both need to ask what is
actually running, and having them guess would be worse.

A macOS port implements `Network` with `socket_vmnet` and `pf`, and reuses
`lima.py` if Lima behaves the same there. The measure of whether the design
succeeded is that the port touches nothing else.
