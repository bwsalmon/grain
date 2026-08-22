"""Unit tests for tests/loadtest.py -- docs/roadmap.md item 6.

The live boot-and-measure path (booting real VMs, driving real kind/build
load, sampling a real host) is exercised by actually running the harness,
not by a unit test -- see the commit that lands this file for the recorded
numbers. What's covered here is everything that can be checked without a
hypervisor: the ps/free/loadavg parsers against captured real-shaped
output, the aggregation and verdict/threshold logic, argument parsing, and
-- following this repo's Fake*/Runner-injection pattern (see
tests/test_libvirt.py) -- that VM cleanup always runs, including when the
boot or the load-driving body raises.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import pytest

from grain.inventory import Cluster
from grain.run import Result
from loadtest import (
    HostSample, ProcessSample, Verdict, booted_vms, build_arg_parser,
    evaluate, parse_free_m, parse_loadavg, parse_qemu_processes,
    per_process_stats, render_report, summarize_tasks,
)

# --- parsers, against real-shaped captured output --------------------------

def test_parse_loadavg_reads_the_first_three_fields():
    text = "0.42 0.31 0.20 3/512 12345\n"
    assert parse_loadavg(text) == (0.42, 0.31, 0.20)


def test_parse_loadavg_rejects_a_short_line():
    with pytest.raises(ValueError):
        parse_loadavg("garbage\n")


FREE_M_OUTPUT = (
    "               total        used        free      shared  buff/cache   available\n"
    "Mem:           31877        1234       26000         576        3600       30123\n"
    "Swap:              0           0           0\n"
)


def test_parse_free_m_reads_total_used_available():
    assert parse_free_m(FREE_M_OUTPUT) == (31877, 1234, 30123)


def test_parse_free_m_rejects_missing_mem_line():
    with pytest.raises(ValueError):
        parse_free_m("Swap:  0  0  0\n")


def _ps_line(pid: int, cpu: float, rss_kb: int, guest: str) -> str:
    return (
        f"{pid} {cpu} {rss_kb} /usr/bin/qemu-system-x86_64 -name "
        f"guest={guest},debug-threads=on -S -machine q35"
    )


def test_parse_qemu_processes_matches_known_names_only():
    ps_out = "\n".join([
        _ps_line(101, 45.0, 5_000_000, "sandbox-0"),
        _ps_line(102, 12.5, 4_200_000, "controller"),
        _ps_line(103, 99.0, 1_000_000, "someone-elses-vm"),
        "not a qemu line at all",
    ])
    samples = parse_qemu_processes(ps_out, ["sandbox-0", "controller"])
    assert {s.name for s in samples} == {"sandbox-0", "controller"}
    by_name = {s.name: s for s in samples}
    assert by_name["sandbox-0"].pid == 101
    assert by_name["sandbox-0"].cpu_percent == 45.0
    assert by_name["sandbox-0"].rss_mb == pytest.approx(5_000_000 / 1024)


def test_parse_qemu_processes_ignores_unrelated_qemu_processes():
    # A concurrently-running item 9 task on this shared host, for instance.
    ps_out = _ps_line(200, 50.0, 1_000, "not-ours")
    assert parse_qemu_processes(ps_out, ["sandbox-0"]) == []


# --- aggregation -------------------------------------------------------------

def _sample(load1: float, available: int, processes: list[ProcessSample]) -> HostSample:
    return HostSample(
        at=0.0, load1=load1, load5=load1, load15=load1,
        mem_total_mb=32000, mem_used_mb=32000 - available, mem_available_mb=available,
        processes=processes,
    )


def test_per_process_stats_aggregates_mean_and_max_per_vm():
    samples = [
        _sample(1.0, 20000, [
            ProcessSample("sandbox-0", 1, 40.0, 4000.0),
            ProcessSample("sandbox-1", 2, 60.0, 5000.0),
        ]),
        _sample(1.0, 20000, [
            ProcessSample("sandbox-0", 1, 80.0, 4200.0),
            ProcessSample("sandbox-1", 2, 20.0, 5100.0),
        ]),
    ]
    stats = {s.name: s for s in per_process_stats(samples)}
    assert stats["sandbox-0"].samples == 2
    assert stats["sandbox-0"].cpu_mean == 60.0
    assert stats["sandbox-0"].cpu_max == 80.0
    assert stats["sandbox-0"].rss_mean_mb == pytest.approx(4100.0)
    assert stats["sandbox-1"].cpu_mean == 40.0


# --- verdict / thresholds ----------------------------------------------------

def test_evaluate_holds_when_load_and_memory_stay_comfortable():
    samples = [_sample(2.0, 20000, []), _sample(3.0, 18000, [])]
    v = evaluate(4, samples)
    assert v.holds
    assert v.headline == "holds"


def test_evaluate_fails_on_a_clear_load_overload():
    # 4 vCPU host, load well past 4 * 1.5 = 6.
    samples = [_sample(9.0, 20000, [])]
    v = evaluate(4, samples)
    assert not v.holds
    assert any("queueing" in r for r in v.reasons)


def test_evaluate_notes_but_does_not_fail_mild_overload():
    # Load above cpu_count but not past the overload factor.
    samples = [_sample(4.5, 20000, [])]
    v = evaluate(4, samples)
    assert v.holds
    assert any("contention" in r for r in v.reasons)


def test_evaluate_fails_when_available_memory_gets_dangerously_low():
    samples = [_sample(1.0, 20000, []), _sample(1.0, 100, [])]
    v = evaluate(4, samples, min_available_mb=512)
    assert not v.holds
    assert any("OOM" in r for r in v.reasons)


def test_evaluate_with_no_samples_never_claims_success():
    v = evaluate(4, [])
    assert not v.holds
    assert v.reasons == ["no samples were collected"]


# --- task summary -------------------------------------------------------------

def test_summarize_tasks_reports_ok_failed_and_missing():
    results = {
        "sandbox-0:kind": Result(["kind"], 0, "cluster created", ""),
        "sandbox-0:build": Result(["make"], 2, "", "error: some.c:1: fail\n"),
        # sandbox-1:kind and sandbox-1:build are absent -- never finished.
    }
    summary = summarize_tasks(["sandbox-0", "sandbox-1"], results)
    assert summary["sandbox-0:kind"] == "ok"
    assert "FAILED (exit 2)" in summary["sandbox-0:build"]
    assert "error: some.c:1: fail" in summary["sandbox-0:build"]
    assert summary["sandbox-1:kind"] == "did not finish within the load timeout"
    assert summary["sandbox-1:build"] == "did not finish within the load timeout"


# --- report rendering (smoke) -------------------------------------------------

def test_render_report_flags_a_host_that_does_not_match_the_design_target():
    report = render_report(
        cpu_count=8, mem_total_mb=16000, sandbox_count=2, with_controller=True,
        elapsed_s=120.0, samples=[_sample(1.0, 10000, [])],
        verdict=Verdict(holds=True, reasons=["fine"]), task_results={"sandbox-0:kind": "ok"},
    )
    assert "does NOT match the design target" in report
    assert "## Verdict: holds" in report


def test_render_report_confirms_a_matching_host():
    report = render_report(
        cpu_count=4, mem_total_mb=31877, sandbox_count=2, with_controller=True,
        elapsed_s=60.0, samples=[], verdict=Verdict(holds=False, reasons=["no samples were collected"]),
        task_results={},
    )
    assert "matches the design target closely" in report
    assert "## Verdict: does not clearly hold" in report


# --- argument parsing ----------------------------------------------------------

def test_arg_parser_defaults():
    args = build_arg_parser().parse_args([])
    assert args.sandboxes == 2
    assert args.with_controller is True
    assert args.keep is False
    assert args.interval == 3.0


def test_arg_parser_no_controller_and_sandbox_count():
    args = build_arg_parser().parse_args(["--sandboxes", "3", "--no-controller", "--keep"])
    assert args.sandboxes == 3
    assert args.with_controller is False
    assert args.keep is True


# --- cleanup always runs, via a Fake VmLifecycle (this repo's Fake*/
# --- Runner-injection pattern -- see tests/test_libvirt.py) -----------------

@dataclass
class _FakeAdapter:
    cluster: Cluster
    fail_create_on: str | None = None
    fail_destroy_on: str | None = None
    created: list[str] = field(default_factory=list)
    started: list[str] = field(default_factory=list)
    destroyed: list[str] = field(default_factory=list)

    def create(self, spec, provision_script=None) -> None:
        if spec.name == self.fail_create_on:
            raise RuntimeError(f"boom creating {spec.name}")
        self.created.append(spec.name)

    def start(self, name: str) -> None:
        self.started.append(name)

    def destroy(self, name: str) -> None:
        if name == self.fail_destroy_on:
            raise RuntimeError(f"boom destroying {name}")
        self.destroyed.append(name)


@pytest.fixture
def cluster() -> Cluster:
    return Cluster(sandbox_count=2)


def test_booted_vms_destroys_everything_created_on_the_happy_path(cluster):
    adapter = _FakeAdapter(cluster=cluster)
    specs = [("controller", "script-a"), ("sandbox-0", "script-b")]
    with booted_vms(adapter, specs) as created:
        assert created == ["controller", "sandbox-0"]
        assert adapter.started == ["controller", "sandbox-0"]
    assert adapter.destroyed == ["controller", "sandbox-0"]


def test_booted_vms_destroys_only_what_was_actually_created_on_a_create_failure(cluster):
    adapter = _FakeAdapter(cluster=cluster, fail_create_on="sandbox-0")
    specs = [("controller", None), ("sandbox-0", None)]
    with pytest.raises(RuntimeError, match="boom creating sandbox-0"):
        with booted_vms(adapter, specs):
            pytest.fail("body must not run when create() fails")
    assert adapter.destroyed == ["controller"]


def test_booted_vms_cleans_up_when_the_caller_body_raises(cluster):
    adapter = _FakeAdapter(cluster=cluster)
    specs = [("controller", None), ("sandbox-0", None)]
    with pytest.raises(RuntimeError, match="load driving blew up"):
        with booted_vms(adapter, specs):
            raise RuntimeError("load driving blew up")
    assert adapter.destroyed == ["controller", "sandbox-0"]


def test_booted_vms_keeps_vms_when_destroy_on_exit_is_false(cluster):
    adapter = _FakeAdapter(cluster=cluster)
    specs = [("sandbox-0", None)]
    with booted_vms(adapter, specs, destroy_on_exit=False):
        pass
    assert adapter.destroyed == []


def test_booted_vms_attempts_every_destroy_even_if_one_fails(cluster, capsys):
    adapter = _FakeAdapter(cluster=cluster, fail_destroy_on="controller")
    specs = [("controller", None), ("sandbox-0", None)]
    with booted_vms(adapter, specs):
        pass
    # sandbox-0's destroy still ran despite controller's failing.
    assert adapter.destroyed == ["sandbox-0"]
    assert "boom destroying controller" in capsys.readouterr().err
