import json
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

from grain.automation.audit import RecordingAuditLog
from grain.automation.config import AutomationConfig
from grain.automation.core import Orchestrator
from grain.automation.github import ApiResponse, FakeTransport, GitHubClient
from grain.automation.state import AutomationState
from grain.inventory import Cluster
from grain.proxy.tokens import SandboxTokenStore
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def issue_json(number: int) -> dict:
    return {
        "number": number, "title": f"issue {number}", "body": "do it",
        "html_url": f"https://github.com/o/r/issues/{number}",
        "labels": [{"name": "grain-agent"}],
    }


def pr_flow_response(pr_number: int) -> list[ApiResponse]:
    """The three responses one `_finish_succeeded` call consumes, in exact
    call order: `branch_exists` (200 -> the branch is really there),
    `create_pull_request` (201, with the fields `GitHubClient` reads back),
    then `remove_label` (in-progress comes off). `FakeTransport.responses`
    is a strict FIFO queue regardless of which call consumes each entry, so
    a test with more than one succeeded outcome needs this whole triple per
    outcome, in order, or a later call silently eats an earlier outcome's
    response.
    """
    return [
        ApiResponse(200, {}, b"{}"),
        ApiResponse(201, {}, json.dumps(
            {"number": pr_number, "html_url": f"https://github.com/o/r/pull/{pr_number}"}
        ).encode()),
        ApiResponse(200, {}, b"{}"),
    ]


def make_orchestrator(*, issues=(), state=None, runner=None, token_store=None):
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
    if token_store is None:
        token_store = SandboxTokenStore(Path(tempfile.mkdtemp()) / "sandbox-tokens.json")
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=state if state is not None else AutomationState(),
        base_runner=fake_runner, token_store=token_store,
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
    orchestrator, transport = make_orchestrator(issues=[issue_json(1)], state=state, runner=runner)
    transport.responses.extend(pr_flow_response(1) + pr_flow_response(2))
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


# --- PR creation on a successful run (docs/roadmap.md item 2) -------------

def test_a_succeeded_run_verifies_the_branch_then_opens_a_pr():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(pr_flow_response(42))

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    branch_call = transport.calls[0]
    assert branch_call["method"] == "GET"
    assert branch_call["path"] == "/repos/o/r/branches/grain%2Fissue-5"
    pr_call = transport.calls[1]
    assert pr_call["method"] == "POST"
    assert pr_call["path"] == "/repos/o/r/pulls"
    sent = json.loads(pr_call["body"])
    assert sent["head"] == "grain/issue-5"
    assert sent["base"] == "main"
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("opened PR #42" in o for o in outcomes)
    # The in-progress label comes off; the trigger label is never re-added
    # for a genuinely finished run.
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    label_mutations = [c for c in mutating if "labels" in c["path"]]
    assert len(label_mutations) == 1


def test_a_succeeded_run_with_no_pushed_branch_is_requeued_not_dropped():
    # The unit exited zero, but the agent never pushed (or pushed somewhere
    # other than the branch dispatch() told it to) — this must not look
    # like a silent success: no PR, and the issue goes back on the queue.
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(404, {}, b"not found"))

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("does not exist" in o for o in outcomes)
    # No PR call was ever made — no POST to the pulls endpoint.
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 2  # remove in-progress, add trigger back


# --- workspace/token wiring into dispatch() (docs/roadmap.md item 2) ------

def test_dispatch_points_the_workspace_clone_at_the_git_proxy():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)])
    orchestrator.run_once(NOW)
    runner = orchestrator.base_runner
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert clone_calls
    script = clone_calls[0][2]
    # Cluster(sandbox_count=2)'s default subnet puts the controller at
    # 10.100.0.2; GIT_PROXY_PORT is 8080 — never GitHub directly.
    assert "http://10.100.0.2:8080/o/r.git" in script


def test_dispatch_mints_a_sandbox_token_and_configures_the_credential_helper(tmp_path):
    token_store = SandboxTokenStore(tmp_path / "sandbox-tokens.json")
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], token_store=token_store)
    orchestrator.run_once(NOW)

    token = json.loads((tmp_path / "sandbox-tokens.json").read_text())["sandbox-0"]
    assert token
    runner = orchestrator.base_runner
    credential_dd = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "dd" and argv[1] == "of=/home/debian/.git-credentials"
    )
    assert token in credential_dd


# --- health visibility from the sweep pass (docs/roadmap.md item 5) -------

def test_an_unhealthy_freed_sandbox_is_logged_but_still_reused():
    # No health-check extras scripted: systemd/docker/disk probes over
    # `runner` fall through to FakeRunner's empty-output default, which
    # fails to parse -> degraded -> a health-warning audit line, per
    # sweeper.py's own docstring on why this is visibility, not a new
    # dispatch-eligibility state.
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(pr_flow_response(42))

    orchestrator.run_once(NOW)

    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("health warning:") for o in outcomes)
    warning_entries = [e for e in orchestrator.audit.entries
                        if e["outcome"].startswith("health warning:")]
    assert warning_entries[0]["sandbox"] == "sandbox-0"
    # Still freed and still gets its PR — health doesn't block the sweep's
    # own success handling.
    assert "sandbox-0" not in orchestrator.state.assignments
    assert any("opened PR #42" in o for o in outcomes)


def test_dispatch_reuses_the_same_token_across_dispatches_to_one_sandbox(tmp_path):
    token_store = SandboxTokenStore(tmp_path / "sandbox-tokens.json")
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], token_store=token_store)
    orchestrator.run_once(NOW)
    first_token = json.loads((tmp_path / "sandbox-tokens.json").read_text())["sandbox-0"]

    # Free the slot and dispatch a second issue to the same (only free) sandbox.
    orchestrator.state.release("sandbox-0")
    orchestrator.run_once(NOW)
    second_token = json.loads((tmp_path / "sandbox-tokens.json").read_text())["sandbox-0"]

    assert first_token == second_token
