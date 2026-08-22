"""Load-test harness for docs/design.md open question 2 (docs/roadmap.md
item 6): does 4 vCPU hold two sandboxes plus a controller under real `kind`
+ build load?

Boots N sandboxes -- real `provision/sandbox.sh`, so docker and kind are
actually installed; the bare base image has neither -- plus, by default,
the controller (`provision/controller.sh`), through the same
`LibvirtAdapter`/`Cluster` machinery every other live test in this repo
uses, not a parallel VM-boot mechanism. Once every VM is up, it drives real
concurrent load in each sandbox -- `kind create cluster` plus a real
from-source build -- and samples the *host's* qemu processes and overall
load/memory while that runs. `docs/design.md`'s one existing data point
(one sandbox, idle, post-`kind create cluster`: ~7.8Gi guest memory with
6.7Gi free, qemu at ~29% CPU / ~5GB RSS on the host) is the baseline this
needs to beat with a real concurrent number -- see that file's
Implementation plan step 4.

Everything below the CLI entry point is a pure function or a small class
taking its dependencies as plain arguments, the same shape every other
module in this repo uses, so the parsing/aggregation/verdict logic is
unit-testable against captured `ps`/`free`/`/proc/loadavg` shapes without a
real hypervisor -- see tests/test_loadtest.py. The live boot-and-measure
part obviously cannot be a unit test; run it with
`python3 -m tests.loadtest` from the repo root, on a host that already
has KVM, `br-grain`, and the cached base image (the same precondition
tests/test_vm_integration.py gates on).
"""

from __future__ import annotations

import argparse
import contextlib
import os
import re
import statistics
import subprocess
import sys
import threading
import time
from collections.abc import Iterable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from grain.adapter.libvirt import LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster, VmSpec
from grain.run import RealRunner, Result

LIBVIRT_URI = "qemu:///system"
BRIDGE = "br-grain"
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
_REPO_ROOT = Path(__file__).parent.parent
SANDBOX_PROVISION_SCRIPT = _REPO_ROOT / "provision" / "sandbox.sh"
CONTROLLER_PROVISION_SCRIPT = _REPO_ROOT / "provision" / "controller.sh"
SSH_USER = "debian"

# docs/design.md's Host baseline step: "matches the n2-highmem-4 shape (4
# vCPU, 32 GB)". That is a design *target*, not a promise about whatever
# host this actually runs on -- render_report() checks the real host
# against this and says so explicitly either way, per docs/roadmap.md item
# 6's instruction not to silently present a number without that context.
DESIGN_TARGET_CPUS = 4
DESIGN_TARGET_MEM_MB = 32768

# A real, sizeable, dependency-free-to-build C project stands in for "a
# build" -- docs/design.md leaves the choice open. Chosen over cloning a
# real repo like redis live: redis's own deps (hiredis/linenoise/jemalloc)
# are git submodules that GitHub's tag tarballs do not include, which would
# make the build fail on missing headers rather than exercise CPU/memory --
# confirmed by inspection of redis's deps/ layout, not assumed. CPython's
# own source tarball has no submodules, a real ./configure && make -j
# toolchain path, and is large enough (hundreds of translation units) to be
# a genuine multi-minute, multi-core compile on a 2-vCPU sandbox -- not the
# synthetic "spin the CPU" of a stress-testing tool, and not trivial like
# compiling one file.
BUILD_TARBALL_URL = "https://www.python.org/ftp/python/3.12.7/Python-3.12.7.tgz"
BUILD_DIR_NAME = "Python-3.12.7"
BUILD_STAGING_DIR = "/tmp/loadtest-build"
KIND_CLUSTER_NAME = "grain-loadtest"


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    kwargs.setdefault("timeout", 30)
    try:
        return subprocess.run(argv, capture_output=True, text=True, **kwargs)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(
            argv, returncode=124, stdout=exc.stdout or "",
            stderr=(exc.stderr or "") + f"\n[timed out after {kwargs['timeout']}s]",
        )


def host_ready() -> bool:
    """The same cheap, local, no-network gate tests/test_vm_integration.py
    and tests/test_controller_integration.py use -- KVM, libvirt's system
    instance, and the bridge already up.
    """
    if not Path("/dev/kvm").exists():
        return False
    if _run(["virsh", "-c", LIBVIRT_URI, "list"]).returncode != 0:
        return False
    if _run(["ip", "link", "show", BRIDGE]).returncode != 0:
        return False
    for tool in ("qemu-img", "cloud-localds", "ssh", "ssh-keygen"):
        if _run(["which", tool]).returncode != 0:
            return False
    return True


def _wait_for_ssh(address, key_path: Path, timeout: float, user: str = SSH_USER) -> None:
    deadline = time.monotonic() + timeout
    last_err = ""
    while time.monotonic() < deadline:
        result = _run([
            "ssh", "-i", str(key_path), "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
            "-o", "ConnectTimeout=5", f"{user}@{address}", "true",
        ], timeout=10)
        if result.returncode == 0:
            return
        last_err = result.stderr
        time.sleep(3)
    raise TimeoutError(f"{address} never became reachable over SSH: {last_err}")


def _wait_for_provisioning(ssh: SshRunner, timeout: float) -> None:
    """Blocks on cloud-init's own completion signal rather than guessing a
    sleep -- the same mechanism tests/test_controller_integration.py uses.
    Applies equally to a sandbox: provision/sandbox.sh is delivered the
    same way (a raw shebang script via cloud-init's scripts-user module),
    so `cloud-init status --wait` covers it too, not just the controller.
    """
    result = ssh.run(["sudo", "cloud-init", "status", "--wait"], check=False)
    if result.returncode != 0:
        raise RuntimeError(
            f"cloud-init did not finish cleanly: {result.stdout}\n{result.stderr}"
        )


# --- VM lifecycle, with guaranteed cleanup ---------------------------------

class VmLifecycle(Protocol):
    cluster: Cluster

    def create(self, spec: VmSpec, provision_script: str | None = None) -> None: ...
    def start(self, name: str) -> None: ...
    def destroy(self, name: str) -> None: ...


@contextlib.contextmanager
def booted_vms(adapter: VmLifecycle, specs: list[tuple[str, str | None]], *,
                destroy_on_exit: bool = True):
    """Creates then starts every (name, provision_script) pair, and destroys
    every one of them on the way out -- success, an exception during boot,
    or an exception raised by the caller's own body -- regardless of how far
    things got. docs/roadmap.md item 6 requires this: no VM this harness
    creates may be left behind on failure. Cleanup is best-effort per VM --
    one destroy() failing must not stop the others from being attempted,
    and every failure is reported rather than silently swallowed.

    `destroy_on_exit=False` is an explicit escape hatch for debugging this
    harness itself (the CLI's `--keep`), never the default.
    """
    created: list[str] = []
    try:
        for name, script in specs:
            adapter.create(adapter.cluster.spec_of(name), script)
            created.append(name)
        for name, _ in specs:
            adapter.start(name)
        yield created
    finally:
        if destroy_on_exit:
            errors = []
            for name in created:
                try:
                    adapter.destroy(name)
                except Exception as exc:  # noqa: BLE001 -- best-effort cleanup
                    errors.append(f"{name}: {exc}")
            if errors:
                print(f"WARNING: cleanup error(s): {'; '.join(errors)}", file=sys.stderr)


# --- driving the load -------------------------------------------------------

def prestage_build_source(ssh: SshRunner) -> None:
    """Untimed setup, run before the sampled window starts -- mirrors
    provision/sandbox.sh's own choice to pre-pull the kind node image, so
    the measured window is real compute/build load, not network fetch time.
    See BUILD_TARBALL_URL's module-level docstring for what and why.
    """
    ssh.run(["bash", "-c", (
        f"mkdir -p {BUILD_STAGING_DIR} && "
        f"curl -fsSL {BUILD_TARBALL_URL} -o {BUILD_STAGING_DIR}/src.tgz && "
        f"tar -xzf {BUILD_STAGING_DIR}/src.tgz -C {BUILD_STAGING_DIR}"
    )])


def run_kind_create(ssh: SshRunner) -> Result:
    return ssh.run(["kind", "create", "cluster", "--name", KIND_CLUSTER_NAME], check=False)


def run_build(ssh: SshRunner) -> Result:
    # $(nproc) inside the guest, not a value computed on the host -- the
    # same thing a real agent's own build invocation would do.
    build_dir = f"{BUILD_STAGING_DIR}/{BUILD_DIR_NAME}"
    return ssh.run(["bash", "-c", (
        f"cd {build_dir} && ./configure --disable-shared -q && make -j$(nproc)"
    )], check=False)


def summarize_tasks(sandbox_names: Iterable[str],
                     results: dict[str, Result | None]) -> dict[str, str]:
    """Pure, so the "missing/ok/failed" wording is unit-testable without a
    live run. `results` maps "<sandbox>:kind" / "<sandbox>:build" to the
    Result that thread produced, or None if it never finished within the
    load timeout (the thread is still join()-ed with a timeout, not killed
    -- see run() -- so "missing" is a real, reportable outcome, not a bug).
    """
    summary: dict[str, str] = {}
    for name in sandbox_names:
        for kind in ("kind", "build"):
            key = f"{name}:{kind}"
            r = results.get(key)
            if r is None:
                summary[key] = "did not finish within the load timeout"
            elif r.returncode == 0:
                summary[key] = "ok"
            else:
                detail = (r.stderr or r.stdout or "").strip().splitlines()
                tail = detail[-1] if detail else ""
                summary[key] = f"FAILED (exit {r.returncode}): {tail[:200]}"
    return summary


# --- host-side sampling ------------------------------------------------------

@dataclass(frozen=True)
class ProcessSample:
    name: str  # the VM name (matched via qemu's own `-name guest=<name>,`)
    pid: int
    cpu_percent: float
    rss_mb: float


@dataclass(frozen=True)
class HostSample:
    at: float
    load1: float
    load5: float
    load15: float
    mem_total_mb: int
    mem_used_mb: int
    mem_available_mb: int
    processes: list[ProcessSample] = field(default_factory=list)


_GUEST_NAME_RE = re.compile(r"-name\s+guest=([^,\s]+)")


def parse_loadavg(text: str) -> tuple[float, float, float]:
    parts = text.split()
    if len(parts) < 3:
        raise ValueError(f"unexpected /proc/loadavg shape: {text!r}")
    return float(parts[0]), float(parts[1]), float(parts[2])


def parse_free_m(output: str) -> tuple[int, int, int]:
    """`free -m`'s `Mem:` line -> (total, used, available), all MiB.
    `available` (not the bare `free` column) is the number that actually
    answers "how much more could be allocated right now" -- it counts
    reclaimable cache, which `free` alone does not.
    """
    for line in output.splitlines():
        if line.startswith("Mem:"):
            fields = line.split()
            if len(fields) < 7:
                raise ValueError(f"unexpected free -m shape: {line!r}")
            total, used, _free, _shared, _buff_cache, available = fields[1:7]
            return int(total), int(used), int(available)
    raise ValueError(f"no Mem: line in free output: {output!r}")


def parse_qemu_processes(ps_output: str, vm_names: Iterable[str]) -> list[ProcessSample]:
    """`ps_output` is `ps -eo pid=,pcpu=,rss=,args=` -- `rss` in KiB.
    Matched against libvirt's own `-name guest=<domain>,...` qemu argument
    rather than a raw substring search, and filtered to the names this run
    actually knows about, so an unrelated qemu process on a shared host
    (docs/roadmap.md item 9 may run concurrently here) is never counted.
    """
    known = set(vm_names)
    samples: list[ProcessSample] = []
    for line in ps_output.splitlines():
        line = line.strip()
        if not line:
            continue
        parts = line.split(None, 3)
        if len(parts) < 4:
            continue
        pid_s, cpu_s, rss_s, args = parts
        match = _GUEST_NAME_RE.search(args)
        if not match or match.group(1) not in known:
            continue
        samples.append(ProcessSample(
            name=match.group(1), pid=int(pid_s),
            cpu_percent=float(cpu_s), rss_mb=int(rss_s) / 1024,
        ))
    return samples


def take_sample(vm_names: Iterable[str]) -> HostSample:
    load1, load5, load15 = parse_loadavg(Path("/proc/loadavg").read_text())
    free_out = subprocess.run(["free", "-m"], capture_output=True, text=True).stdout
    total, used, available = parse_free_m(free_out)
    env = {**os.environ, "COLUMNS": "1000"}  # ps must not truncate `args`
    ps_out = subprocess.run(
        ["ps", "-eo", "pid=,pcpu=,rss=,args="], capture_output=True, text=True, env=env,
    ).stdout
    return HostSample(
        at=time.time(), load1=load1, load5=load5, load15=load15,
        mem_total_mb=total, mem_used_mb=used, mem_available_mb=available,
        processes=parse_qemu_processes(ps_out, vm_names),
    )


@dataclass
class Sampler:
    vm_names: list[str]
    interval: float = 3.0
    samples: list[HostSample] = field(default_factory=list)
    _stop: threading.Event = field(default_factory=threading.Event, repr=False)
    _thread: threading.Thread | None = field(default=None, repr=False)

    def start(self) -> None:
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()

    def _loop(self) -> None:
        while not self._stop.is_set():
            try:
                self.samples.append(take_sample(self.vm_names))
            except Exception as exc:  # noqa: BLE001 -- one bad sample must not kill the run
                print(f"WARNING: sample failed: {exc}", file=sys.stderr)
            self._stop.wait(self.interval)

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=self.interval + 5)


# --- aggregation, verdict, report -------------------------------------------

@dataclass(frozen=True)
class ProcessStats:
    name: str
    samples: int
    cpu_mean: float
    cpu_max: float
    rss_mean_mb: float
    rss_max_mb: float


def per_process_stats(samples: list[HostSample]) -> list[ProcessStats]:
    by_name: dict[str, list[ProcessSample]] = {}
    for s in samples:
        for p in s.processes:
            by_name.setdefault(p.name, []).append(p)
    stats = []
    for name, procs in sorted(by_name.items()):
        cpu = [p.cpu_percent for p in procs]
        rss = [p.rss_mb for p in procs]
        stats.append(ProcessStats(
            name=name, samples=len(procs),
            cpu_mean=statistics.mean(cpu), cpu_max=max(cpu),
            rss_mean_mb=statistics.mean(rss), rss_max_mb=max(rss),
        ))
    return stats


@dataclass(frozen=True)
class Verdict:
    holds: bool
    reasons: list[str]

    @property
    def headline(self) -> str:
        return "holds" if self.holds else "does not clearly hold"


def evaluate(cpu_count: int, samples: list[HostSample], *,
             load_overload_factor: float = 1.5,
             min_available_mb: float = 512) -> Verdict:
    """Two explicit thresholds -- not a claim that these are the only
    signals that matter, but the two things docs/design.md's open question
    2 is actually asking: is the host queueing work it cannot run right now
    (1-minute load average clearly above the physical core count), and is
    it close to actually running out of memory (available memory near
    zero)? `load_overload_factor` gives load1 room to exceed cpu_count
    somewhat -- some queueing under a genuine concurrent-load spike is
    expected and not itself disqualifying; well past it is.
    """
    if not samples:
        return Verdict(holds=False, reasons=["no samples were collected"])
    peak_load1 = max(s.load1 for s in samples)
    min_available = min(s.mem_available_mb for s in samples)
    reasons: list[str] = []
    holds = True
    if peak_load1 > cpu_count * load_overload_factor:
        holds = False
        reasons.append(
            f"peak 1-minute load average {peak_load1:.2f} is well past "
            f"{cpu_count} vCPU x {load_overload_factor} -- the host was "
            f"genuinely queueing work, not just busy"
        )
    elif peak_load1 > cpu_count:
        reasons.append(
            f"peak 1-minute load average {peak_load1:.2f} exceeded the "
            f"{cpu_count} physical vCPU -- some contention under peak "
            f"concurrent load, short of clear overload"
        )
    if min_available < min_available_mb:
        holds = False
        reasons.append(
            f"available memory dropped to {min_available:.0f} MiB, under "
            f"the {min_available_mb:.0f} MiB floor -- real OOM risk"
        )
    if not reasons:
        reasons.append(
            f"peak 1-minute load average {peak_load1:.2f} stayed at or "
            f"under {cpu_count} vCPU, with {min_available:.0f} MiB "
            f"available memory at its lowest"
        )
    return Verdict(holds=holds, reasons=reasons)


def render_report(*, cpu_count: int, mem_total_mb: int, sandbox_count: int,
                   with_controller: bool, elapsed_s: float,
                   samples: list[HostSample], verdict: Verdict,
                   task_results: dict[str, str]) -> str:
    lines: list[str] = []
    lines.append("# Load-test result: docs/design.md open question 2")
    lines.append("")
    lines.append(
        f"Host: {cpu_count} vCPU, {mem_total_mb} MiB memory. Design target "
        f"(docs/design.md's Host baseline, n2-highmem-4): "
        f"{DESIGN_TARGET_CPUS} vCPU, {DESIGN_TARGET_MEM_MB} MiB."
    )
    if cpu_count == DESIGN_TARGET_CPUS and abs(mem_total_mb - DESIGN_TARGET_MEM_MB) < 4096:
        lines.append(
            "This host matches the design target closely -- the numbers "
            "below are directly informative for the real target, not a "
            "differently-shaped stand-in."
        )
    else:
        lines.append(
            "This host does NOT match the design target -- treat the "
            "numbers below as indicative, not a direct answer for an "
            "actual n2-highmem-4."
        )
    lines.append("")
    lines.append(
        f"Load: {sandbox_count} sandbox(es)"
        + (" + controller" if with_controller else " (no controller)")
        + ", each sandbox running `kind create cluster` and a real "
          "`./configure && make -j$(nproc)` CPython build concurrently, "
          f"for {elapsed_s:.0f}s of sampled wall time."
    )
    lines.append("")
    lines.append("## Task outcomes")
    for key, outcome in sorted(task_results.items()):
        lines.append(f"- {key}: {outcome}")
    lines.append("")
    lines.append("## Host aggregate")
    if samples:
        load1s = [s.load1 for s in samples]
        avail = [s.mem_available_mb for s in samples]
        used = [s.mem_used_mb for s in samples]
        lines.append(
            f"- 1-minute load average: min {min(load1s):.2f}, mean "
            f"{statistics.mean(load1s):.2f}, max {max(load1s):.2f} "
            f"(host has {cpu_count} vCPU)"
        )
        lines.append(
            f"- memory used: min {min(used)} MiB, max {max(used)} MiB "
            f"of {mem_total_mb} MiB total"
        )
        lines.append(
            f"- memory available: min {min(avail)} MiB (lowest point "
            f"observed), max {max(avail)} MiB"
        )
        lines.append(f"- samples: {len(samples)}")
    else:
        lines.append("- no samples collected")
    lines.append("")
    lines.append("## Per-VM qemu process (host side)")
    lines.append("")
    lines.append("| VM | samples | CPU% mean | CPU% max | RSS mean (MiB) | RSS max (MiB) |")
    lines.append("|---|---|---|---|---|---|")
    for stat in per_process_stats(samples):
        lines.append(
            f"| {stat.name} | {stat.samples} | {stat.cpu_mean:.1f} | "
            f"{stat.cpu_max:.1f} | {stat.rss_mean_mb:.0f} | {stat.rss_max_mb:.0f} |"
        )
    lines.append("")
    lines.append(f"## Verdict: {verdict.headline}")
    for reason in verdict.reasons:
        lines.append(f"- {reason}")
    lines.append("")
    return "\n".join(lines)


# --- CLI / orchestration -----------------------------------------------------

def build_arg_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="loadtest",
        description=(
            "Boot real sandbox VMs (+ the controller, by default) and drive "
            "concurrent kind + build load, to answer docs/design.md open "
            "question 2 empirically. See docs/roadmap.md item 6."
        ),
    )
    p.add_argument("--sandboxes", type=int, default=2,
                    help="number of sandbox VMs (default: 2, matching the design)")
    p.add_argument("--no-controller", dest="with_controller", action="store_false",
                    default=True, help="skip booting the controller VM")
    p.add_argument("--interval", type=float, default=3.0,
                    help="host sample interval in seconds (default: 3)")
    p.add_argument("--boot-timeout", type=float, default=180.0,
                    help="seconds to wait for SSH to come up, per VM")
    p.add_argument("--provision-timeout", type=float, default=1200.0,
                    help="seconds to wait for cloud-init/provisioning, per VM")
    p.add_argument("--load-timeout", type=float, default=1800.0,
                    help="seconds to wait for the concurrent kind+build load to finish")
    p.add_argument("--config-dir", default="/var/lib/grain/instances/loadtest",
                    help="LibvirtAdapter's per-instance file directory")
    p.add_argument("--out", default=None,
                    help="path to also write the report to (always printed to stdout)")
    p.add_argument("--keep", action="store_true",
                    help="do not destroy the VMs afterward (debugging only)")
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_arg_parser().parse_args(argv)

    if not host_ready():
        print("host not ready: need /dev/kvm, qemu:///system, and br-grain up",
              file=sys.stderr)
        return 1
    if not BASE_IMAGE.exists():
        print(f"base image not found at {BASE_IMAGE}", file=sys.stderr)
        return 1

    cpu_count = os.cpu_count() or 1
    mem_total_mb, _, _ = parse_free_m(
        subprocess.run(["free", "-m"], capture_output=True, text=True).stdout
    )

    config_dir = Path(args.config_dir)
    config_dir.mkdir(parents=True, exist_ok=True)
    key_dir = config_dir / "ssh"
    key_dir.mkdir(parents=True, exist_ok=True)
    private_key = key_dir / "id_ed25519"
    public_key = key_dir / "id_ed25519.pub"
    if not private_key.exists():
        result = _run(["ssh-keygen", "-t", "ed25519", "-f", str(private_key), "-N", "", "-q"])
        if result.returncode != 0:
            print(f"could not generate a throwaway ssh key: {result.stderr}", file=sys.stderr)
            return 1

    cluster = Cluster(sandbox_count=args.sandboxes, image=str(BASE_IMAGE))
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir, ssh_public_key_path=public_key,
    )

    specs: list[tuple[str, str | None]] = []
    if args.with_controller:
        specs.append((cluster.controller_name, CONTROLLER_PROVISION_SCRIPT.read_text()))
    for name in cluster.sandbox_names:
        specs.append((name, SANDBOX_PROVISION_SCRIPT.read_text()))

    print(f"booting {', '.join(n for n, _ in specs)} ...")

    try:
        with booted_vms(adapter, specs, destroy_on_exit=not args.keep) as created:
            ssh_runners: dict[str, SshRunner] = {}
            for name in created:
                ssh_runners[name] = SshRunner(
                    inner=RealRunner(), user=SSH_USER,
                    address=cluster.address_of(name), key_path=private_key,
                )
                print(f"waiting for {name} to answer SSH ...")
                _wait_for_ssh(cluster.address_of(name), private_key, args.boot_timeout)
                print(f"waiting for {name}'s provisioning to finish "
                      f"(this is the slow part -- docker/kind install for a "
                      f"sandbox) ...")
                _wait_for_provisioning(ssh_runners[name], args.provision_timeout)

            sandbox_runners = {n: r for n, r in ssh_runners.items() if n in cluster.sandbox_names}
            for name, ssh in sandbox_runners.items():
                print(f"pre-staging the build source on {name} (untimed) ...")
                prestage_build_source(ssh)

            print("starting the sampler and the concurrent kind+build load ...")
            sampler = Sampler(vm_names=created, interval=args.interval)
            sampler.start()
            started_at = time.monotonic()

            results: dict[str, Result] = {}
            lock = threading.Lock()

            def worker(key: str, fn) -> None:
                result = fn()
                with lock:
                    results[key] = result

            threads = []
            for name, ssh in sandbox_runners.items():
                threads.append(threading.Thread(
                    target=worker, args=(f"{name}:kind", lambda s=ssh: run_kind_create(s))))
                threads.append(threading.Thread(
                    target=worker, args=(f"{name}:build", lambda s=ssh: run_build(s))))
            for t in threads:
                t.start()
            for t in threads:
                t.join(timeout=args.load_timeout)

            elapsed = time.monotonic() - started_at
            sampler.stop()

            task_results = summarize_tasks(sandbox_runners.keys(), results)
            verdict = evaluate(cpu_count, sampler.samples)
            report = render_report(
                cpu_count=cpu_count, mem_total_mb=mem_total_mb,
                sandbox_count=args.sandboxes, with_controller=args.with_controller,
                elapsed_s=elapsed, samples=sampler.samples, verdict=verdict,
                task_results=task_results,
            )
            print(report)
            if args.out:
                Path(args.out).write_text(report)
            return 0 if verdict.holds else 2
    except Exception as exc:  # noqa: BLE001 -- report, don't traceback; cleanup already ran
        print(f"load test failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
