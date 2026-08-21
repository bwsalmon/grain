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
    lima.py             VM lifecycle via limactl
    net_linux.py        bridge, nftables ruleset, metadata DNAT
  cli.py                `grain host …`
```

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

**Not verified** — no hypervisor was available:

- **Whether Lima can attach a guest to a host bridge with a fixed address on
  Linux.** This is [open question 1](design.md#open-questions) and the
  `networks:` stanza in `lima.py` is marked `UNVERIFIED` in the source. If
  it cannot, a libvirt driver replaces `lima.py` and nothing else changes —
  which is the interface earning its keep.
- Lima's `list --json` field names, and its exact status strings.
- That a Debian guest comes up on the assigned address at all.

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
network_up()   egress_policy(mode)
```

`state` and `list_vms` are additions to the seven operations the design
named — the health check and the recreate flow both need to ask what is
actually running, and having them guess would be worse.

A macOS port implements `Network` with `socket_vmnet` and `pf`, and reuses
`lima.py` if Lima behaves the same there. The measure of whether the design
succeeded is that the port touches nothing else.
