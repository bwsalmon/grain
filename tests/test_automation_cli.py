from datetime import datetime, timezone
from pathlib import Path

from grain.automation.state import AutomationState, TriggerKind
from grain.cli import main


def test_status_reports_free_sandboxes_with_no_state_file(tmp_path: Path, capsys):
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "automation", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split() == ["sandbox-0", "free"]
    assert lines[1].split() == ["sandbox-1", "free"]


def test_status_reports_an_assigned_sandbox(tmp_path: Path, capsys):
    state = AutomationState()
    state.assign("sandbox-0", issue=42, unit="grain-task-sandbox-0",
                 now=datetime(2026, 1, 1, tzinfo=timezone.utc))
    state.save(tmp_path / "state" / "automation" / "state.json")

    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "automation", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split()[:2] == ["sandbox-0", "issue"]
    assert "42" in lines[0]
    assert lines[1].split() == ["sandbox-1", "free"]


def test_status_reports_a_pr_triggered_assignment_distinctly(tmp_path: Path, capsys):
    # docs/roadmap.md item 9: an operator reading `automation status` should
    # be able to tell a PR-continuation assignment apart from an
    # issue-triggered one at a glance.
    state = AutomationState()
    state.assign("sandbox-0", issue=99, unit="grain-task-sandbox-0",
                 now=datetime(2026, 1, 1, tzinfo=timezone.utc),
                 kind=TriggerKind.PR, branch="feature-x")
    state.save(tmp_path / "state" / "automation" / "state.json")

    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "automation", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split()[:2] == ["sandbox-0", "pr"]
    assert "99" in lines[0]


def test_github_audit_on_an_empty_secrets_dir_reports_none_found(tmp_path: Path, capsys):
    # No network call happens here — there's nothing to audit, so this is
    # safe to run with no real credential or connectivity configured.
    assert main(["--data-dir", str(tmp_path), "github", "audit"]) == 0
    out = capsys.readouterr().out
    assert "no *.token files found" in out
    assert "secrets" in out and "github" in out


def test_github_audit_prints_every_result_and_flags_a_withheld_scope(
    tmp_path: Path, capsys, monkeypatch,
):
    """`cmd_github_audit`'s own loop and nonzero-on-flagged exit code --
    `audit_token`/`audit_secrets_dir` themselves are covered on their own
    terms by test_automation_credential_audit.py. `RealTransport` is
    replaced with a `FakeTransport` so a classic-PAT-shaped token here
    still needs no real network access.
    """
    from grain.automation.github import ApiResponse, FakeTransport

    secrets_dir = tmp_path / "secrets" / "github"
    secrets_dir.mkdir(parents=True)
    (secrets_dir / "bot.token").write_text("ghp_" + "a" * 36)
    (secrets_dir / "app.token").write_text("github_pat_" + "b" * 20)

    fake_transport = FakeTransport(
        default=ApiResponse(200, {"X-OAuth-Scopes": "repo, workflow"}, b"{}")
    )
    monkeypatch.setattr("grain.cli.RealTransport", lambda: fake_transport)

    exit_code = main(["--data-dir", str(tmp_path), "github", "audit"])
    out = capsys.readouterr().out
    assert "app" in out and "unverifiable" in out
    assert "bot" in out and "flagged" in out
    assert exit_code == 1


def test_host_cleanup_exit_code_is_nonzero_when_a_step_fails(monkeypatch):
    """`cleanup()`'s own step-by-step behavior is covered on its own terms
    by test_automation_cleanup.py; what's untested elsewhere is only
    `cmd_host_cleanup`'s own translation of "a step failed" into a nonzero
    process exit code -- and a real `--dry-run` can't exercise that branch
    at all, since `DryRunRunner` always fakes a successful result.
    """
    import argparse

    import grain.cli as cli_module
    from grain.automation.cleanup import CleanupResult, StepResult
    from grain.inventory import Cluster

    monkeypatch.setattr(cli_module, "build_cluster", lambda args: Cluster(sandbox_count=1))
    monkeypatch.setattr(cli_module, "_runner", lambda args: object())
    monkeypatch.setattr(
        cli_module, "build_ssh_runner", lambda cluster, base_runner, name, args: object()
    )
    monkeypatch.setattr(
        cli_module, "cleanup",
        lambda runner: CleanupResult([StepResult("kind", False, "boom")]),
    )
    args = argparse.Namespace(name="all", ssh_user="debian", ssh_key="/tmp/key")
    assert cli_module.cmd_host_cleanup(args) == 1


# --- build_orchestrator / cmd_automation_run_once ---------------------------
#
# `Orchestrator.run_once` itself (network-touching through `GitHubClient`) is
# exercised on its own terms by test_automation_core.py (a `FakeTransport`)
# and test_live_issue_to_pr.py (the real thing). What's untested elsewhere is
# only `build_orchestrator`'s wiring from `/data`-shaped files into an
# `Orchestrator`, and `cmd_automation_run_once`'s dry-run save-skip -- neither
# of which needs (or may safely make) a real GitHub call, so both are tested
# directly rather than through `main()`.

def _write_automation_json(tmp_path: Path, **overrides) -> None:
    import json as jsonlib

    (tmp_path / "config").mkdir(exist_ok=True)
    (tmp_path / "secrets" / "github").mkdir(parents=True, exist_ok=True)
    body = {"task_owner": "acme", "task_repo": "widgets", "default_target_repo": "acme/widgets"}
    body.update(overrides)
    (tmp_path / "config" / "automation.json").write_text(jsonlib.dumps(body))


def test_build_orchestrator_wires_config_state_and_audit_from_data_dir(tmp_path: Path):
    import argparse

    from grain.automation.github import GitHubClient
    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, state_path = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.config.task_owner == "acme"
    assert isinstance(orchestrator.github, GitHubClient)
    assert state_path == tmp_path / "state" / "automation" / "state.json"
    assert orchestrator.state_path == state_path
    assert orchestrator.gemini_key_config is None  # no gemini-key.json written
    assert orchestrator.janitor_config is None  # no janitor.json written


def test_build_orchestrator_dry_run_wraps_github_and_disables_state_persistence(tmp_path: Path):
    import argparse

    from grain.automation.github import DryRunGitHubClient
    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=True)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert isinstance(orchestrator.github, DryRunGitHubClient)
    assert orchestrator.state_path is None  # a dry run must never persist state


def test_build_orchestrator_loads_gemini_key_config_when_present(tmp_path: Path):
    import argparse
    import json as jsonlib

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    (tmp_path / "config" / "gemini-key.json").write_text(
        jsonlib.dumps({"project_id": "acme-project"})
    )
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.gemini_key_config is not None
    assert orchestrator.gemini_key_config.project_id == "acme-project"


def test_build_orchestrator_loads_gcp_key_config_when_present(tmp_path: Path):
    """bwsalmon/agents#126: mirrors the gemini_key_config test above --
    `gcp_key_config` is `None` unless `/data/config/gcp-key.json` exists,
    the same "absence is the off switch" shape.
    """
    import argparse
    import json as jsonlib

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    (tmp_path / "config" / "gcp-key.json").write_text(
        jsonlib.dumps({"service_account_email": "narrow@p.iam.gserviceaccount.com",
                       "project_id": "acme-project"})
    )
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.gcp_key_config is not None
    assert orchestrator.gcp_key_config.project_id == "acme-project"


def test_build_orchestrator_loads_janitor_config_when_present(tmp_path: Path):
    """bwsalmon/agents#113: mirrors the gemini_key_config test above --
    `janitor_config` is `None` unless `/data/config/janitor.json` exists,
    the same "absence is the off switch" shape.
    """
    import argparse
    import json as jsonlib

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    (tmp_path / "config" / "janitor.json").write_text(
        jsonlib.dumps({"project_id": "acme-project", "ttl_hours": 12})
    )
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.janitor_config is not None
    assert orchestrator.janitor_config.project_id == "acme-project"
    assert orchestrator.janitor_config.ttl_hours == 12


def test_build_orchestrator_loads_scheduled_jobs_config_when_present(tmp_path: Path):
    """bwsalmon/agents#163: mirrors the janitor_config test above --
    `scheduled_jobs_config` is `None` unless `/data/config/scheduled-jobs/`
    exists as a directory, the same "absence is the off switch" shape,
    just over a directory of template files instead of one JSON file.
    """
    import argparse

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    jobs_dir = tmp_path / "config" / "scheduled-jobs"
    jobs_dir.mkdir(parents=True)
    (jobs_dir / "weekly-audit.md").write_text(
        "Title: Weekly audit\nInterval-Hours: 168\n\nPlease audit things."
    )
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.scheduled_jobs_config is not None
    assert [job.name for job in orchestrator.scheduled_jobs_config.jobs] == ["weekly-audit"]


def test_build_orchestrator_leaves_scheduled_jobs_config_unset_when_dir_absent(tmp_path: Path):
    import argparse

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.scheduled_jobs_config is None


def test_build_orchestrator_leaves_gcp_key_config_unset_when_config_absent(tmp_path: Path):
    import argparse

    from grain.cli import build_orchestrator
    from grain.inventory import Cluster
    from grain.run import FakeRunner

    _write_automation_json(tmp_path)
    args = argparse.Namespace(data_dir=str(tmp_path), dry_run=False)
    orchestrator, _ = build_orchestrator(Cluster(sandbox_count=1), FakeRunner(), args)

    assert orchestrator.gcp_key_config is None


def test_run_once_saves_state_when_not_a_dry_run(monkeypatch):
    import argparse

    import grain.cli as cli_module

    calls = []

    class FakeOrchestrator:
        def __init__(self):
            self.state = self

        def run_once(self, now):
            calls.append("run_once")

        def save(self, path):
            calls.append(("save", path))

    fake_orchestrator = FakeOrchestrator()
    state_path = Path("/fake/state.json")
    monkeypatch.setattr(cli_module, "build_cluster", lambda args: object())
    monkeypatch.setattr(cli_module, "_runner", lambda args: object())
    monkeypatch.setattr(
        cli_module, "build_orchestrator",
        lambda cluster, runner, args: (fake_orchestrator, state_path),
    )

    assert cli_module.cmd_automation_run_once(argparse.Namespace(dry_run=False)) == 0
    assert calls == ["run_once", ("save", state_path)]


def test_run_once_does_not_save_state_under_dry_run(monkeypatch):
    import argparse

    import grain.cli as cli_module

    calls = []

    class FakeOrchestrator:
        def __init__(self):
            self.state = self

        def run_once(self, now):
            calls.append("run_once")

        def save(self, path):
            calls.append(("save", path))

    fake_orchestrator = FakeOrchestrator()
    monkeypatch.setattr(cli_module, "build_cluster", lambda args: object())
    monkeypatch.setattr(cli_module, "_runner", lambda args: object())
    monkeypatch.setattr(
        cli_module, "build_orchestrator",
        lambda cluster, runner, args: (fake_orchestrator, Path("/fake/state.json")),
    )

    assert cli_module.cmd_automation_run_once(argparse.Namespace(dry_run=True)) == 0
    assert calls == ["run_once"]
