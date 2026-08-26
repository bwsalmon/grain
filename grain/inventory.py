"""The cluster inventory: the single source of truth for names and addresses.

Everything host-specific derives from here — VM specs, interface names,
addresses, and the firewall rules that pin them. That is deliberate. In the
microvm.nix design this was called out as load-bearing rather than tidy: a
sandbox that got an address without its matching anti-spoof rule would
silently undermine the identity model, and hand-maintained parallel lists
are exactly how that happens.

Addresses are *assigned*, never discovered. The adapter tells the VM what its
address is; it does not ask the hypervisor afterwards. That keeps
`address()` a pure function and removes any dependency on a particular
driver's JSON shape.
"""

from __future__ import annotations

import ipaddress
import tomllib
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path

# Controller service ports, on the controller's private address.
GIT_PROXY_PORT = 8080

# Offsets within the subnet. Host is .1, controller .2, sandboxes from .10.
_HOST_OFFSET = 1
_CONTROLLER_OFFSET = 2
_SANDBOX_OFFSET = 10

# A provisioning script (provision/controller.sh) is plain text with no
# Cluster in scope of its own, but needs the controller's actual private
# address, which depends on `subnet` -- not fixed the way the offsets above
# are. The adapters substitute this for `str(cluster.controller_ip)` when
# they bake a script into a VM's user-data (see adapter/libvirt.py's
# `render_user_data` and adapter/lima.py's `render_instance_config`), so the
# script stays correct on any subnet a deployment configures, not only the
# default.
CONTROLLER_IP_PLACEHOLDER = "__GRAIN_CONTROLLER_IP__"


class Role(str, Enum):
    CONTROLLER = "controller"
    SANDBOX = "sandbox"


@dataclass(frozen=True)
class VmSpec:
    """What the adapter needs in order to create a VM."""

    name: str
    role: Role
    cpus: int
    mem_mb: int
    disk_gb: int
    image: str


@dataclass(frozen=True)
class Cluster:
    """A whole single-node deployment, derived from a handful of numbers."""

    sandbox_count: int = 2
    subnet: ipaddress.IPv4Network = ipaddress.IPv4Network("10.100.0.0/24")
    bridge: str = "br-grain"
    image: str = "debian-12"

    controller_cpus: int = 2
    controller_mem_mb: int = 4096
    controller_disk_gb: int = 40

    # Sized for a kind control plane plus a build; see docs/design.md.
    sandbox_cpus: int = 6
    sandbox_mem_mb: int = 15360
    sandbox_disk_gb: int = 80

    @classmethod
    def load(cls, path: Path) -> "Cluster":
        """Reads overrides from a small TOML file (docs/bootstrap.md Phase
        2), falling back to every dataclass default for whatever key is
        absent -- including the file itself being absent, which is the
        common case before a deployment has named a real base image.
        `tomllib` is stdlib on 3.11+ (pyproject.toml's floor), so this costs
        no dependency.

        This only changes *where* the handful of raw numbers come from --
        the inventory stays the single source of truth for everything
        derived from them (this module's own docstring), and every other
        `Cluster` field keeps working exactly as before when the file is
        absent or omits it.
        """
        if not path.exists():
            return cls()
        raw = dict(tomllib.loads(path.read_text()))
        if "subnet" in raw:
            raw["subnet"] = ipaddress.IPv4Network(raw["subnet"])
        return cls(**raw)

    def __post_init__(self) -> None:
        if self.sandbox_count < 1:
            raise ValueError("sandbox_count must be at least 1")
        # Keep the address arithmetic inside the subnet with room to spare.
        highest = _SANDBOX_OFFSET + self.sandbox_count - 1
        if highest >= self.subnet.num_addresses - 1:
            raise ValueError(
                f"sandbox_count={self.sandbox_count} does not fit in {self.subnet}"
            )

    # --- names ------------------------------------------------------------
    @property
    def controller_name(self) -> str:
        return "controller"

    def sandbox_name(self, index: int) -> str:
        self._check_index(index)
        return f"sandbox-{index}"

    @property
    def sandbox_names(self) -> list[str]:
        return [self.sandbox_name(i) for i in range(self.sandbox_count)]

    @property
    def names(self) -> list[str]:
        return [self.controller_name, *self.sandbox_names]

    # --- addresses --------------------------------------------------------
    @property
    def host_ip(self) -> ipaddress.IPv4Address:
        return self.subnet.network_address + _HOST_OFFSET

    @property
    def controller_ip(self) -> ipaddress.IPv4Address:
        return self.subnet.network_address + _CONTROLLER_OFFSET

    def address_of(self, name: str) -> ipaddress.IPv4Address:
        if name == self.controller_name:
            return self.controller_ip
        return self.subnet.network_address + _SANDBOX_OFFSET + self._index_of(name)

    # --- per-VM host-side facts ------------------------------------------
    def interface_of(self, name: str) -> str:
        """Host-side tap interface name.

        Kept under the 15-character limit Linux imposes on interface names.
        """
        if name == self.controller_name:
            return "gr-ctl"
        return f"gr-sb{self._index_of(name)}"

    # --- specs ------------------------------------------------------------
    def spec_of(self, name: str) -> VmSpec:
        if name == self.controller_name:
            return VmSpec(
                name=name,
                role=Role.CONTROLLER,
                cpus=self.controller_cpus,
                mem_mb=self.controller_mem_mb,
                disk_gb=self.controller_disk_gb,
                image=self.image,
            )
        self._index_of(name)  # validates
        return VmSpec(
            name=name,
            role=Role.SANDBOX,
            cpus=self.sandbox_cpus,
            mem_mb=self.sandbox_mem_mb,
            disk_gb=self.sandbox_disk_gb,
            image=self.image,
        )

    @property
    def specs(self) -> list[VmSpec]:
        return [self.spec_of(n) for n in self.names]

    # --- internals --------------------------------------------------------
    def _check_index(self, index: int) -> None:
        if not 0 <= index < self.sandbox_count:
            raise ValueError(
                f"sandbox index {index} out of range (0..{self.sandbox_count - 1})"
            )

    def _index_of(self, name: str) -> int:
        prefix = "sandbox-"
        if not name.startswith(prefix):
            raise ValueError(f"unknown VM name: {name!r}")
        try:
            index = int(name[len(prefix) :])
        except ValueError:
            raise ValueError(f"unknown VM name: {name!r}") from None
        self._check_index(index)
        return index
