"""The host adapter interface.

This is the seam the design is built around: everything above the hypervisor
is platform-agnostic, and everything that is not lives behind this interface.
The rule that keeps it honest is that *nothing outside an adapter may assume
a platform* — no nftables in the git proxy, no launchd in the controller
image. When something needs a platform-specific fact, it comes through here.

The design named seven operations: create, start, stop, destroy, address,
network_up, egress_policy. Two more are added below — `state` and `list_vms`
— because the health check and the recreate script both need to ask what is
actually running, and having them guess would be worse.
"""

from __future__ import annotations

import ipaddress
from abc import ABC, abstractmethod
from dataclasses import dataclass
from enum import Enum

from typing import Protocol

from ..inventory import Cluster, VmSpec


class VmState(str, Enum):
    ABSENT = "absent"
    STOPPED = "stopped"
    RUNNING = "running"
    UNKNOWN = "unknown"


class EgressMode(str, Enum):
    OPEN = "open"
    ALLOWLIST = "allowlist"


@dataclass(frozen=True)
class VmInfo:
    name: str
    state: VmState
    address: ipaddress.IPv4Address


class Network(Protocol):
    """The private network the VMs share.

    Separate from the VM driver on purpose. Lima may well serve as the driver
    on both Linux and macOS, in which case networking is the *only* thing a
    port has to replace — but that is only structurally true if the driver
    does not reach for a platform's networking directly.
    """

    def up(self, egress: "EgressMode" = ...) -> None: ...

    def apply_rules(self, egress: "EgressMode") -> None: ...


class HostAdapter(ABC):
    """Platform-specific VM lifecycle and networking."""

    def __init__(self, cluster: Cluster, network: Network) -> None:
        self.cluster = cluster
        self.network = network

    # --- networking -------------------------------------------------------
    def network_up(self) -> None:
        """Create the private network and install the filtering policy.

        Idempotent: safe to run on every boot and after any inventory change.
        Delegated, not abstract — the driver has no business knowing whether
        the host filters with nftables or pf.
        """
        self.network.up(EgressMode.OPEN)

    def egress_policy(self, mode: EgressMode) -> None:
        """Set whether sandboxes may reach the internet freely."""
        self.network.apply_rules(mode)

    # --- lifecycle --------------------------------------------------------
    @abstractmethod
    def create(self, spec: VmSpec, provision_script: str | None = None) -> None:
        """Create a VM. Must fail rather than silently adopt an existing one."""

    @abstractmethod
    def start(self, name: str) -> None: ...

    @abstractmethod
    def stop(self, name: str) -> None: ...

    @abstractmethod
    def destroy(self, name: str) -> None:
        """Remove a VM and its disk. Idempotent."""

    @abstractmethod
    def state(self, name: str) -> VmState: ...

    @abstractmethod
    def list_vms(self) -> list[VmInfo]: ...

    # --- addressing -------------------------------------------------------
    def address(self, name: str) -> ipaddress.IPv4Address:
        """Addresses are assigned by the inventory, not discovered.

        Deliberately not abstract. A driver that reported addresses back from
        the hypervisor would make this depend on that hypervisor's output
        format, and would let the firewall rules and the VMs disagree about
        who is who — which is precisely what the anti-spoofing rules exist to
        prevent.
        """
        return self.cluster.address_of(name)

    # --- derived convenience ---------------------------------------------
    def recreate(self, name: str, provision_script: str | None = None) -> None:
        """Destroy and rebuild one VM from its spec.

        The routine maintenance operation: the base image changed, the disk
        filled, the sandbox wedged, or the weekly rotation came round.
        """
        self.destroy(name)
        self.create(self.cluster.spec_of(name), provision_script)
        self.start(name)
