import json
from datetime import datetime, timedelta, timezone

from grain.automation.audit import RecordingAuditLog
from grain.automation.config import AutomationConfig
from grain.automation.core import Orchestrator
from grain.automation.github import ApiResponse, FakeTransport, GitHubClient
from grain.automation.state import AutomationState
from grain.inventory import Cluster
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def issue_json(number: int) -> dict:
    return {
        "number": number, "title": f"issue {number}", "body": "do it",
        "html_url": f"https://github.com/o/r/issues/{number}",
        "labels": [{"name": "grain-agent"}],
    }


def make_orchestrator(*, issues=(), state=None, runner=None):
    cluster = Cluster(sandbox_count=2)
    # `default`, not a one-shot `responses` queue: a sweep pass can fire
    # label-mutation calls before `_dispatch`'s own `list_issues` GET, and
    # those mutations don't care about the body — but if a queued response
    # meant for the GET got consumed by an earlier DELETE, the GET would
    # fall through to an unrelated default. Serving the same body to every
    # call sidesteps that ordering coupling entirely, since only the GET
    # calls' response body is ever actually inspected.
    transport = FakeTransport(
        default=ApiResponse(200, {}, json.dumps(list(issues)).encode())
    )
    github = GitHubClient(transport, token="t")
    config = AutomationConfig(owner="o", repo="r")
    fake_runner = runner if runner is not None else FakeRunner()
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=state if state is not None else AutomationState(),
        base_runner=fake_runner,
        audit=RecordingAuditLog(),
        # Bypass SshRunner's argv wrapping here — that integration is
        # covered by test_automation_ssh.py; these tests target
        # Orchestrator's own decisions against a plain FakeRunner.
        ssh_runner_factory=lambda _sandbox: fake_runner,
    )
    return orchestrator, transport


def test_dispatches_the_lowest_numbered_candidate_first():
    orchestrator, _ = make_orchestrator(issues=[issue_json(2), issue_json(1)])
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments["sandbox-0"].issue == 1


def test_dispatch_uses_only_one_sandbox_for_one_issue():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)])
    orchestrator.run_once(NOW)
    assigned = list(orchestrator.state.assignments)
    assert assigned == ["sandbox-0"]


def test_two_candidates_fill_the_whole_pool():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1), issue_json(2)])
    orchestrator.run_once(NOW)
    assert set(orchestrator.state.assignments) == {"sandbox-0", "sandbox-1"}


def test_more_candidates_than_sandboxes_leaves_the_rest_queued():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1), issue_json(2), issue_json(3)])
    orchestrator.run_once(NOW)
    assert len(orchestrator.state.assignments) == 2
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: no free sandbox" in outcomes


def test_an_issue_already_tracked_as_in_progress_is_not_redispatched():
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], state=state, runner=runner)
    orchestrator.run_once(NOW)
    # Still just the one (untouched) assignment - not a second dispatch.
    assert orchestrator.state.assignments["sandbox-0"].issue == 1
    assert not runner.ran("systemd-run")


def test_rate_limit_stops_further_dispatch_this_run():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1), issue_json(2)])
    orchestrator.config = AutomationConfig(owner="o", repo="r", runs_per_hour=1)
    orchestrator.run_once(NOW)
    assert len(orchestrator.state.assignments) == 1
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: rate limit" in outcomes


def test_a_finished_run_is_swept_and_its_slot_reused_in_the_same_pass():
    state = AutomationState()
    state.assign("sandbox-0", issue=99, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    state.assign("sandbox-1", issue=100, unit="grain-task-sandbox-1",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], state=state, runner=runner)
    orchestrator.run_once(NOW)
    # Both prior runs finished and were freed; the new issue lands on one.
    assert 1 in {a.issue for a in orchestrator.state.assignments.values()}
    assert 99 not in orchestrator.state.in_progress_issues()


def test_a_failed_run_is_requeued_via_labels_not_state():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=failed\nResult=exit-code\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    orchestrator.run_once(NOW)
    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "failed" in outcomes
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 2  # remove in-progress, add trigger back
