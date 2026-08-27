import json
import re
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

import grain.automation.capture as capture_module
import grain.automation.core as core_module
from grain.automation.audit import RecordingAuditLog
from grain.automation.config import AutomationConfig
from grain.automation.core import Orchestrator
from grain.automation.dispatch import (
    CONTROLLER_AGENT_SSH_KEY_PATH, GCP_KEY_PATH, GEMINI_KEY_PATH, unit_name,
)
from grain.automation.gcp_keys import GcpKeyConfig
from grain.automation.gemini_keys import GeminiKeyConfig
from grain.automation.github import (
    ApiResponse, Comment, FakeTransport, GitHubClient, GitHubError, Issue,
)
from grain.automation.scratch_repo import ScratchRepoConfig
from grain.automation.history import NullSessionHistory, RecordingSessionHistory
from grain.automation.janitor import JanitorConfig
from grain.automation.scheduled_jobs import ScheduledJob, ScheduledJobsConfig
from grain.automation.ssh import SshRunner
from grain.automation.state import AutomationState, OpenPullRequest, TriggerKind
from grain.inventory import Cluster
from grain.proxy.allowlist import Allowlist
from grain.proxy.credentials import CredentialSet
from grain.proxy.tokens import SandboxCredentialStore, SandboxTokenStore
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def issue_json(number: int, body: str = "do it", labels=("grain-agent",)) -> dict:
    return {
        # "id" isn't read by list_issues itself, but the same fixture also
        # serves as FakeTransport's shared default response for whichever
        # GET call falls through to it -- including _dispatch's new
        # list_comments call (docs/roadmap.md item 12), which does read
        # "id". Present so that fallback doesn't KeyError in tests that
        # never queue a dedicated comments response.
        "id": number, "number": number, "title": f"issue {number}", "body": body,
        "html_url": f"https://github.com/o/r/issues/{number}",
        "labels": [{"name": label} for label in labels],
    }


def pr_trigger_json(number: int) -> dict:
    """The `/issues?labels=...` listing shape for a labelled PR — what
    `list_pull_requests`'s own preliminary call filters *to* (the opposite
    of `list_issues`'s filter). Missing `head`/`base`, same as a real PR item
    on this endpoint — that's exactly why hydration via a separate
    `get_pull_request` call is needed.
    """
    return {
        "number": number, "title": f"pr {number}", "body": "please review",
        "html_url": f"https://github.com/o/r/pull/{number}",
        "labels": [{"name": "grain-agent"}],
        "pull_request": {"url": "..."},
    }


def pr_detail_json(number: int, head_ref: str = "feature-x", base_ref: str = "main",
                    mergeable: bool | None = None) -> dict:
    return {
        "number": number, "title": f"pr {number}", "body": "please review",
        "html_url": f"https://github.com/o/r/pull/{number}",
        "head": {"ref": head_ref, "sha": "abc123", "label": f"o:{head_ref}"},
        "base": {"ref": base_ref, "label": f"o:{base_ref}"},
        "mergeable": mergeable,
    }


DEFAULT_COMMIT_MESSAGE = (
    "Fix the frobnicator\n\nRewrites the frobnicator to handle empty input "
    "instead of raising."
)


def branch_json(message: str = DEFAULT_COMMIT_MESSAGE, sha: str = "deadbeef") -> dict:
    """The `GET .../branches/{branch}` response shape `get_branch_head`
    reads -- `commit.sha` and `commit.commit.message`, nested exactly as
    GitHub's own branches endpoint returns them (bwsalmon/agents#79).
    """
    return {"commit": {"sha": sha, "commit": {"message": message}}}


def pr_flow_response(pr_number: int, *, commit_message: str = DEFAULT_COMMIT_MESSAGE) -> list[ApiResponse]:
    """The six responses one `_finish_succeeded_issue` call consumes, in
    exact call order: `get_branch_head` (200 -> the branch is really there,
    with its head commit's own message, bwsalmon/agents#79),
    `get_issue` (the title `create_pull_request`'s own title folds in),
    `create_pull_request` (201, with the fields `GitHubClient` reads back),
    `add_label` (bwsalmon/agents#54 -- `completed_label` goes on the moment
    the PR exists), `remove_label` (in-progress comes off), then a second
    `remove_label` (bwsalmon/agents#95 -- the per-sandbox agent label comes
    off too).
    `FakeTransport.responses` is a strict FIFO queue regardless of which
    call consumes each entry, so a test with more than one succeeded
    outcome needs this whole handful per outcome, in order, or a later call
    silently eats an earlier outcome's response.

    The task issue itself is *not* closed here any more (bwsalmon/agents#54):
    that now waits on the PR itself closing, polled once per `run_once` by
    `_close_finished_prs` -- see `open_pr_response` for the extra, separate
    response that pass consumes, later in the *same* `run_once` this
    handful runs in, for every open-PR record a test's outcome(s) produce.
    """
    return [
        ApiResponse(200, {}, json.dumps(branch_json(commit_message)).encode()),
        ApiResponse(200, {}, json.dumps(issue_json(pr_number)).encode()),
        ApiResponse(201, {}, json.dumps(
            {"number": pr_number, "html_url": f"https://github.com/o/r/pull/{pr_number}"}
        ).encode()),
        ApiResponse(200, {}, b"{}"),
        ApiResponse(200, {}, b"{}"),
        ApiResponse(200, {}, b"{}"),
    ]


def open_pr_response(pr_number: int, *, state: str = "open",
                      mergeable: bool | None = None) -> ApiResponse:
    """One `get_pull_request` response, for `_close_finished_prs`'s own poll
    (bwsalmon/agents#54) of an open-PR record `_finish_succeeded_issue`
    just wrote. That poll runs once per `run_once`, after every outcome in
    the sweep has already been finished -- so in a test scripting `N`
    successful fresh-PR outcomes via `pr_flow_response`, this many more
    responses are appended *after* all of them, one per outcome, in the
    same order those outcomes were finished in.
    """
    return ApiResponse(200, {}, json.dumps(
        {**pr_detail_json(pr_number, mergeable=mergeable), "state": state}
    ).encode())


def completion_priming_response() -> ApiResponse:
    """One `list_comments` response for `_restart_commented_completions`'s
    own poll (bwsalmon/agents#135) of a `completed_issues` record a
    same-cycle `_finish_succeeded_issue`/`_finish_succeeded_pr`/
    `_finish_no_changes` call just wrote -- that poll runs once per
    `run_once`, right after every outcome in the sweep has already been
    finished (`_close_finished_prs`'s own `open_pr_response` docstring has
    the same "later in the same run_once" story), and the very first poll
    a fresh record ever gets always just primes its baseline rather than
    restarting anything, so an empty comment thread serves every such test
    just as well as a populated one would.
    """
    return ApiResponse(200, {}, b"[]")


class OrchestratorTransport(FakeTransport):
    """`FakeTransport` plus one always-served route: `GET /repos/{owner}/{repo}`,
    the target repo's default branch, which every dispatch now reads
    (`core.py`'s `_resolve_target`) and every PR creation falls back to.

    Answered directly, *without* consuming the `responses` queue: that queue
    is a strict FIFO regardless of which call takes each entry, so a test
    scripting a sweep's exact response sequence (`pr_flow_response`) would
    otherwise have a dispatch-time repo read silently eat the first of them.
    The call is still recorded in `calls`, so a test can assert on it.
    """

    # Overridable so one test can make that route 404 (a target repo the
    # credential can't see) without scripting the whole queue around it.
    repo_status: int = 200
    # bwsalmon/agents#83: `_pr_health`'s `list_check_runs` read fires for
    # *every* still-open PR `_close_finished_prs` polls, the same
    # "unconditional, unrelated to what most tests are actually testing"
    # shape the default-branch route above already has -- answered here,
    # bypassing the queue, so the many existing tests that never heard of
    # check runs don't all need a response spliced into their exact call
    # order. Empty (no checks configured) reads as healthy either way
    # `_pr_health` looks at it: not pending, not failing. A test about
    # conflicts or failing checks specifically overrides this attribute.
    check_runs_body: bytes = b'{"total_count": 0, "check_runs": []}'
    # bwsalmon/agents#139: `_restart_orphaned_in_progress` runs this same
    # "unconditional, unrelated to what most tests are actually testing"
    # `list_issues` call every single `run_once`, regardless of state --
    # answered here, bypassing the queue, for the identical reason the two
    # routes above already are: without this, every existing test scripting
    # an exact call sequence would need a spliced-in response for a listing
    # it never asked to exercise. Empty by default (no orphaned in-progress
    # issue), the same "the common case needs no test to know this exists"
    # shape `check_runs_body` already has; a test about this feature
    # specifically overrides it.
    in_progress_issues_body: bytes = b"[]"

    def request(self, *, method: str, path: str, headers: dict, body):
        if method == "GET" and re.fullmatch(r"/repos/[^/]+/[^/]+", path):
            self.calls.append(
                {"method": method, "path": path, "headers": dict(headers), "body": body}
            )
            if self.repo_status != 200:
                return ApiResponse(self.repo_status, {}, b"not found")
            return ApiResponse(200, {}, b'{"default_branch": "main"}')
        if method == "GET" and re.search(r"/commits/[^/]+/check-runs", path):
            self.calls.append(
                {"method": method, "path": path, "headers": dict(headers), "body": body}
            )
            return ApiResponse(200, {}, self.check_runs_body)
        if method == "GET" and re.search(r"/issues\?labels=grain-agent-in-progress&", path):
            self.calls.append(
                {"method": method, "path": path, "headers": dict(headers), "body": body}
            )
            return ApiResponse(200, {}, self.in_progress_issues_body)
        return super().request(method=method, path=path, headers=headers, body=body)


def allowlist_of(repos) -> Allowlist:
    """A real `Allowlist` over a real file — it re-reads on every check, so
    a test that needs to widen or narrow it can just rewrite the file.
    """
    path = Path(tempfile.mkdtemp()) / "repo-allowlist.json"
    path.write_text(json.dumps(list(repos)))
    return Allowlist(path)


def credentials_with(*names: str) -> CredentialSet:
    path = Path(tempfile.mkdtemp())
    for name in names:
        (path / f"{name}.token").write_text(f"{name}-token-value")
    return CredentialSet(path)


def make_orchestrator(*, issues=(), state=None, runner=None, token_store=None,
                       history=None, allowed=("o/r",), gemini_key_config=None,
                       credentials=None, credential_store=None, state_path=None,
                       gcp_key_config=None, janitor_config=None, scratch_repo_config=None,
                       scheduled_jobs_config=None):
    cluster = Cluster(sandbox_count=2)
    # `default`, not a one-shot `responses` queue: a sweep pass can fire
    # label-mutation calls before `_dispatch`'s own `list_issues` GET, and
    # those mutations don't care about the body — but if a queued response
    # meant for the GET got consumed by an earlier DELETE, the GET would
    # fall through to an unrelated default. Serving the same body to every
    # call sidesteps that ordering coupling entirely, since only the GET
    # calls' response body is ever actually inspected.
    transport = OrchestratorTransport(
        default=ApiResponse(200, {}, json.dumps(list(issues)).encode())
    )
    github = GitHubClient(transport, token="t")
    # `default_target_repo`, so a plain `issue_json(...)` with no `/repo`
    # line still dispatches to "o/r" -- the single-repo shape these tests
    # were written against, and a real deployment's own migration path.
    # Tests about the task/target split write the directive explicitly.
    config = AutomationConfig(task_owner="o", task_repo="r",
                               default_target_repo="o/r")
    fake_runner = runner if runner is not None else FakeRunner()
    if token_store is None:
        token_store = SandboxTokenStore(Path(tempfile.mkdtemp()) / "sandbox-tokens.json")
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=state if state is not None else AutomationState(),
        base_runner=fake_runner, token_store=token_store,
        allowlist=allowlist_of(allowed),
        audit=RecordingAuditLog(), history=history,
        # Bypass SshRunner's argv wrapping here — that integration is
        # covered by test_automation_ssh.py; these tests target
        # Orchestrator's own decisions against a plain FakeRunner.
        ssh_runner_factory=lambda _sandbox: fake_runner,
        gemini_key_config=gemini_key_config,
        gcp_key_config=gcp_key_config, janitor_config=janitor_config,
        scratch_repo_config=scratch_repo_config,
        scheduled_jobs_config=scheduled_jobs_config,
        credentials=credentials, credential_store=credential_store,
        # bwsalmon/agents#51: most tests leave this unset, which makes
        # `_save_state` a no-op -- exactly the pre-existing behaviour, since
        # those tests only ever assert against `orchestrator.state` in
        # memory. Tests about surviving a controller crash/VM restart pass
        # a real path and read it back independently of the in-memory
        # `Orchestrator` to prove the write actually landed on disk.
        state_path=state_path,
    )
    return orchestrator, transport


def test_orchestrator_defaults_audit_and_history_to_no_op_implementations_when_none_given():
    from grain.automation.audit import NullAuditLog
    from grain.automation.history import NullSessionHistory

    transport = OrchestratorTransport(default=ApiResponse(200, {}, b"[]"))
    orchestrator = Orchestrator(
        cluster=Cluster(sandbox_count=1),
        github=GitHubClient(transport, token="t"),
        config=AutomationConfig(task_owner="o", task_repo="r", default_target_repo="o/r"),
        state=AutomationState(),
        base_runner=FakeRunner(),
        token_store=SandboxTokenStore(Path(tempfile.mkdtemp()) / "sandbox-tokens.json"),
        allowlist=allowlist_of(("o/r",)),
        # audit/history both left at their None default.
    )
    assert isinstance(orchestrator.audit, NullAuditLog)
    assert isinstance(orchestrator.history, NullSessionHistory)
    # Both are no-ops: recording through them must not raise.
    orchestrator.audit.record(sandbox=None, issue=None, outcome="ignored")


def test_ssh_runner_for_builds_a_real_sshrunner_when_no_factory_is_injected():
    """Every other test in this file passes `ssh_runner_factory` to bypass
    `_ssh_runner_for`'s own `SshRunner` construction -- `SshRunner`'s argv
    wrapping is covered on its own terms by test_automation_ssh.py, and
    tests here target `Orchestrator`'s own decisions against a plain
    `FakeRunner`. This one checks the production default path itself:
    `Orchestrator` wires up a real `SshRunner` correctly when nothing
    overrides it.
    """
    orchestrator, _ = make_orchestrator()
    orchestrator.ssh_runner_factory = None
    runner = orchestrator._ssh_runner_for("sandbox-0")
    assert isinstance(runner, SshRunner)
    assert runner.user == orchestrator.config.ssh_user
    assert runner.address == orchestrator.cluster.address_of("sandbox-0")
    assert runner.key_path == orchestrator.config.ssh_key_path


def test_dispatches_the_lowest_numbered_candidate_first():
    orchestrator, _ = make_orchestrator(issues=[issue_json(2), issue_json(1)])
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments["sandbox-0"].issue == 1


def test_dispatch_uses_only_one_sandbox_for_one_issue():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)])
    orchestrator.run_once(NOW)
    assigned = list(orchestrator.state.assignments)
    assert assigned == ["sandbox-0"]


def test_dispatch_strips_the_needs_approval_label_once_a_human_approves():
    """A human approves a grain-suggested fix task the same way they start
    any other task -- applying trigger_label -- and may leave
    needs_approval_label on it rather than removing it themselves.
    `_dispatch` strips it the moment it dispatches, so the issue doesn't
    keep reading "needs approval" once a human's approval is exactly what
    just got it dispatched (bwsalmon/agents#83).
    """
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(1, labels=("grain-agent", "grain-agent-needs-approval"))]
    )
    orchestrator.run_once(NOW)
    delete_calls = [c for c in transport.calls if c["method"] == "DELETE"]
    assert any(
        c["path"] == "/repos/o/r/issues/1/labels/grain-agent-needs-approval"
        for c in delete_calls
    )


def test_a_dispatch_failure_does_not_crash_the_rest_of_the_cycle():
    """A `CommandError` from dispatch()/dispatch_pr() (an SSH/auth failure
    reaching one sandbox, e.g. a stale git-proxy token) must not take down
    every other candidate queued this cycle (docs/next-session.md, found
    live). Both candidates here hit the identical failing runner, so both
    fail -- the point is that the *second* one is even attempted at all,
    proving the loop continues past the first failure rather than crashing.
    """
    runner = FakeRunner()
    runner.expect("bash -c", returncode=128, stderr="fatal: Authentication failed")
    orchestrator, _ = make_orchestrator(issues=[issue_json(1), issue_json(2)], runner=runner)

    orchestrator.run_once(NOW)  # must not raise

    # Neither dispatch actually succeeded, so neither sandbox was assigned
    # and neither issue's labels were touched -- both stay eligible for a
    # retry on a later cycle, exactly like a fresh, untouched candidate.
    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    failures = [o for o in outcomes if "dispatch failed" in o]
    assert len(failures) == 2
    assert any("Authentication failed" in f for f in failures)


def test_a_non_command_error_from_dispatch_still_raises(monkeypatch):
    """Only a `CommandError` is tolerated -- anything else is a real bug,
    not dispatch()'s one expected failure mode, and must still surface.
    """
    monkeypatch.setattr(
        core_module, "dispatch",
        lambda *args, **kwargs: (_ for _ in ()).throw(ValueError("not a CommandError")),
    )
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)])

    with pytest.raises(ValueError):
        orchestrator.run_once(NOW)


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
    orchestrator, transport = make_orchestrator(issues=[issue_json(1)], state=state, runner=runner)
    # bwsalmon/agents#82: still-active-and-in-budget now costs the sweep one
    # `get_issue` call (cancel-on-close) before `_dispatch`'s own listing —
    # answered open, so this stays exactly the "left alone" case it always
    # was.
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(1)).encode()))
    orchestrator.run_once(NOW)
    # Still just the one (untouched) assignment - not a second dispatch.
    assert orchestrator.state.assignments["sandbox-0"].issue == 1
    assert not runner.ran("systemd-run")


def test_rate_limit_stops_further_dispatch_this_run():
    orchestrator, _ = make_orchestrator(issues=[issue_json(1), issue_json(2)])
    orchestrator.config = AutomationConfig(task_owner="o", task_repo="r",
                                            default_target_repo="o/r", runs_per_hour=1)
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
    transport.responses.extend(
        pr_flow_response(1) + pr_flow_response(2)
        + [completion_priming_response(), completion_priming_response()]
        + [open_pr_response(1), open_pr_response(2)]
    )
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
    assert len(mutating) == 3  # remove in-progress, add trigger back, remove agent label


def test_a_requeue_tolerates_a_404_from_add_label_for_a_stale_assignment():
    """A leftover `AutomationState` assignment can point at an issue number
    that doesn't exist in the *currently configured* repo -- e.g. after
    `controller configure` points a live deployment at a different repo
    while an old assignment is still on file (docs/next-session.md, found
    live). That must not crash the whole sweep before `_dispatch` ever runs.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=failed\nResult=exit-code\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),  # remove_label: fine either way
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # add_label: no such issue here
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "stale assignment" in o for o in outcomes)


def test_a_requeue_still_raises_a_non_404_github_error():
    """Only a 404 is treated as "stale assignment, move on" -- a genuine
    API failure (5xx, auth) must still surface, not be silently swallowed
    alongside it.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=failed\nResult=exit-code\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),
        ApiResponse(500, {}, b"internal error"),
    ])

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


# --- cancel-on-close (bwsalmon/agents#82) -----------------------------------

def test_a_still_active_run_whose_issue_closed_is_cancelled_not_requeued():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        # get_issue (cancel-on-close poll): closed
        ApiResponse(200, {}, json.dumps({**issue_json(5), "state": "closed"}).encode()),
        ApiResponse(200, {}, b"{}"),  # remove_label: in_progress comes off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("cancelled" in o and "closed" in o for o in outcomes)
    assert "failed" not in outcomes and "stranded" not in outcomes
    # Only the label cleanup -- no re-add of the trigger label, unlike a
    # requeue: a closed issue must not come back for redispatch.
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 2  # remove in-progress, remove agent label


def test_a_still_active_run_whose_issue_is_still_open_is_left_running():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(5)).encode()))

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    assert not runner.ran("sudo systemctl stop")


def test_a_still_active_run_has_its_agent_label_refreshed_every_tick():
    """bwsalmon/agents#95: an assignment `_sweep` leaves standing gets its
    per-sandbox agent label re-applied every `run_once` cycle, not just
    once at dispatch -- so a task sitting mid-flight for many cycles keeps
    saying which sandbox is doing the work even if the label was ever
    knocked off, or an earlier cycle's own call to apply it failed partway.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(5)).encode()))

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    add_calls = [
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    ]
    assert any(json.loads(c["body"]) == {"labels": ["grain-agent-working-0"]} for c in add_calls)


def test_a_cancel_on_close_poll_tolerates_a_404_for_a_stale_assignment():
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(404, {}, b'{"message": "Not Found"}'))

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" in orchestrator.state.assignments


def test_a_cancel_on_close_poll_still_raises_a_non_404_github_error():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(500, {}, b"internal error"))

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_a_cancellation_label_cleanup_tolerates_a_404_for_a_stale_assignment():
    # `GitHubClient.remove_label` already treats a 404 as "label already
    # off" and never raises for it -- so a stale assignment naming a
    # since-deleted issue must not crash the cycle here either.
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps({**issue_json(5), "state": "closed"}).encode()),
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # remove_label: no such issue here
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("cancelled" in o for o in outcomes)


# --- ask_question (docs/roadmap.md item 12) --------------------------------

def test_a_succeeded_run_that_asked_a_question_posts_a_comment_not_a_pr(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    question_file = tmp_path / "question.txt"
    question_file.write_text("Should I use approach A or B?")
    monkeypatch.setattr(core_module, "question_path", lambda unit: str(question_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),  # create_comment
        ApiResponse(200, {}, b"{}"),  # remove_label (in-progress off)
        ApiResponse(200, {}, b"{}"),  # add_label (awaiting-reply on)
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    comment_call = next(
        c for c in transport.calls if c["method"] == "POST" and c["path"].endswith("/comments")
    )
    comment_body = json.loads(comment_call["body"])["body"]
    assert "Should I use approach A or B?" in comment_body
    # docs/roadmap.md item 14: distinguishes this from a human's own comment.
    assert "Posted automatically by grain-agent" in comment_body
    # No branch was ever checked, and no PR was opened -- the question path
    # short-circuits before either.
    assert not any(c["path"].startswith("/repos/o/r/branches") for c in transport.calls)
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    # The trigger label is never re-added directly -- only the
    # awaiting-reply label goes on. (docs/roadmap.md item 13's
    # _promote_answered_questions is what re-adds the trigger label later,
    # once a trusted reply shows up.)
    label_posts = [
        c for c in transport.calls if c["method"] == "POST" and c["path"].endswith("/labels")
    ]
    assert len(label_posts) == 1
    assert json.loads(label_posts[0]["body"]) == {"labels": ["grain-agent-awaiting-reply"]}
    # Recorded so a later reply can be matched against this exact comment.
    pending = orchestrator.state.pending_questions["5"]
    assert pending.question_comment_id == 555
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("asked a question" in o and "Should I use approach A or B?" in o
               for o in outcomes)


def test_a_question_comment_tolerates_a_404_for_a_stale_assignment(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    question_file = tmp_path / "question.txt"
    question_file.write_text("does this issue even exist?")
    monkeypatch.setattr(core_module, "question_path", lambda unit: str(question_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(404, {}, b'{"message": "Not Found"}'))

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "stale assignment" in o for o in outcomes)
    # Best-effort only: no label call was even attempted once the comment
    # itself 404'd -- there's no human to notify either way.
    assert not any(c["method"] == "DELETE" for c in transport.calls)
    assert not any(
        c["method"] == "POST" and c["path"].endswith("/labels") for c in transport.calls
    )
    assert orchestrator.state.pending_questions == {}


def test_a_question_comment_raises_on_a_non_404_github_error(monkeypatch, tmp_path):
    """Only a 404 is treated as "stale assignment, move on" here too -- a
    genuine API failure must still surface.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    question_file = tmp_path / "question.txt"
    question_file.write_text("does this issue even exist?")
    monkeypatch.setattr(core_module, "question_path", lambda unit: str(question_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(500, {}, b"internal error"))

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_a_pr_triggered_success_that_asked_a_question_also_posts_a_comment(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x",
                 target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    question_file = tmp_path / "question.txt"
    question_file.write_text("which review comment should I prioritize?")
    monkeypatch.setattr(core_module, "question_path", lambda unit: str(question_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(201, {}, json.dumps({"id": 777}).encode()),  # create_comment
        ApiResponse(200, {}, b"{}"),  # remove_label
        ApiResponse(200, {}, b"{}"),  # add_label (awaiting-reply on)
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    comment_call = next(
        c for c in transport.calls if c["method"] == "POST" and c["path"].endswith("/comments")
    )
    assert "which review comment should I prioritize?" in json.loads(comment_call["body"])["body"]
    pending = orchestrator.state.pending_questions["9"]
    assert pending.question_comment_id == 777
    assert pending.kind is TriggerKind.PR
    assert pending.branch == "feature-x"


# --- comment_on_issue (bwsalmon/agents#50, reworked by bwsalmon/agents#89) --

def test_a_succeeded_run_with_no_pushed_branch_and_a_comment_posts_it_and_tags_completed(
    monkeypatch, tmp_path,
):
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    comment_file = tmp_path / "comment.txt"
    comment_file.write_text("Investigated X; no code change was needed.")
    monkeypatch.setattr(core_module, "comment_path", lambda unit: str(comment_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(404, {}, b"not found"),  # get_branch_head: no branch pushed
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),  # create_comment
        ApiResponse(200, {}, b"{}"),  # add_label (completed)
        ApiResponse(200, {}, b"{}"),  # remove_label (in-progress off)
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    # bwsalmon/agents#89: the branch is checked first, before the comment
    # is ever consulted -- this is what makes a pushed branch always win.
    branch_call = transport.calls[0]
    assert branch_call["method"] == "GET"
    assert branch_call["path"] == "/repos/o/r/branches/grain%2Fissue-5"
    comment_call = next(
        c for c in transport.calls if c["method"] == "POST" and c["path"].endswith("/comments")
    )
    comment_body = json.loads(comment_call["body"])["body"]
    assert "Investigated X; no code change was needed." in comment_body
    assert "Posted automatically by grain-agent" in comment_body
    # bwsalmon/agents#54: a no-branch finish is never auto-closed -- only
    # tagged, so a human decides whether the comment answers the task.
    assert not any(c["method"] == "PATCH" for c in transport.calls)
    completed_call = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    )
    assert json.loads(completed_call["body"]) == {"labels": ["grain-agent-completed"]}
    # No PR was opened -- there was nothing to open one for.
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("finished with no changes" in o and "Investigated X" in o for o in outcomes)


def test_a_succeeded_run_with_a_pushed_branch_opens_a_pr_even_if_the_agent_also_left_a_comment(
    monkeypatch, tmp_path,
):
    """bwsalmon/agents#89: the tool that is now `comment_on_issue` used to
    be called `complete_analysis` and, if the agent called it at all, made
    `core.py` skip the branch check outright -- so an agent that pushed
    real commits and then also (mistakenly) called it had its own PR
    silently dropped. The branch is now checked first and decides
    everything on its own; a leftover comment file only matters once that
    branch turns out to have nothing on it, so it must not stop a real PR
    from opening.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    comment_file = tmp_path / "comment.txt"
    comment_file.write_text("I also pushed a fix for this.")
    monkeypatch.setattr(core_module, "comment_path", lambda unit: str(comment_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    # The exact same five-response PR flow a plain succeeded run consumes
    # (`pr_flow_response`'s own docstring) -- if the comment file being
    # present made an extra call happen anywhere in here, one of these
    # responses would land on the wrong call and this would fail loudly.
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    pr_call = next(
        c for c in transport.calls if c["method"] == "POST" and c["path"] == "/repos/o/r/pulls"
    )
    assert json.loads(pr_call["body"])["head"] == "grain/issue-5"
    assert not any(
        c["method"] == "POST" and c["path"].endswith("/comments") for c in transport.calls
    )
    assert orchestrator.state.open_pull_requests == {
        "5": OpenPullRequest(issue=5, target_owner="o", target_repo="r", pr_number=42),
    }


def test_a_no_branch_comment_tolerates_a_404_for_a_stale_assignment(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    comment_file = tmp_path / "comment.txt"
    comment_file.write_text("does this issue even exist?")
    monkeypatch.setattr(core_module, "comment_path", lambda unit: str(comment_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(404, {}, b"not found"),  # get_branch_head: no branch pushed
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # create_comment
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "stale assignment" in o for o in outcomes)
    # Best-effort only: no close/label call was even attempted once the
    # comment itself 404'd -- there's no issue left to close either way.
    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert not any(c["method"] == "DELETE" for c in transport.calls)


def test_a_no_branch_comment_raises_on_a_non_404_github_error(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    comment_file = tmp_path / "comment.txt"
    comment_file.write_text("does this issue even exist?")
    monkeypatch.setattr(core_module, "comment_path", lambda unit: str(comment_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(404, {}, b"not found"),  # get_branch_head: no branch pushed
        ApiResponse(500, {}, b"internal error"),  # create_comment
    ])

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_dispatch_fetches_the_issue_conversation_for_the_prompt():
    """`_dispatch` must fetch the top-level comment thread for every issue
    dispatch, not just PRs -- otherwise a redispatch after a human answers
    a prior `ask_question` call would never see the reply.
    """
    state = AutomationState()
    runner = FakeRunner()
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)], state=state, runner=runner)

    orchestrator.run_once(NOW)

    comment_gets = [
        c for c in transport.calls
        if c["method"] == "GET" and c["path"].startswith("/repos/o/r/issues/5/comments")
    ]
    assert len(comment_gets) == 1


def comment_json_for(id_: int, *, user: str = "someone", body: str = "a reply",
                      author_association: str = "NONE") -> dict:
    return {"id": id_, "user": {"login": user}, "body": body,
            "author_association": author_association}


# --- auto-redispatch after a trusted reply (docs/roadmap.md item 13) ------

def test_a_trusted_reply_promotes_the_question_and_redispatches_in_the_same_run():
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)], state=state)
    # The first GET this run makes is _promote_answered_questions's own
    # list_comments call (there is nothing in state.assignments for _sweep
    # to act on) -- everything after falls through to the shared `issues`
    # default, which _dispatch's own list_issues/list_pull_requests need.
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="maintainer", body="go ahead",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.pending_questions == {}
    assert orchestrator.state.assignments["sandbox-0"].issue == 5
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("maintainer" in o and "requeued" in o for o in outcomes)


def test_an_untrusted_reply_does_not_promote_the_question():
    """A random public commenter (author_association "NONE") must not be
    able to redispatch the agent just by replying -- that would reopen the
    exact prompt-injection gate the trigger label exists to close.
    """
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="rando", body="do whatever you want",
                          author_association="NONE"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert "5" in orchestrator.state.pending_questions
    assert orchestrator.state.assignments == {}


def test_no_new_comment_yet_leaves_the_question_pending():
    # The only comment present is the question itself (id == the recorded
    # baseline, not greater than it) -- nothing has actually been added
    # since it was posted.
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(100, user="grain-agent-bot", body="the question itself",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert "5" in orchestrator.state.pending_questions
    assert orchestrator.state.assignments == {}


def test_a_404_while_checking_for_a_reply_clears_the_pending_question():
    state = AutomationState()
    state.record_pending_question(201, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(404, {}, b'{"message": "Not Found"}'))

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.pending_questions == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "201" in o for o in outcomes)


def test_a_non_404_error_while_checking_for_a_reply_still_raises():
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(500, {}, b"boom"))

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


# --- restart on comment after completion (bwsalmon/agents#135) -----------

def test_a_trusted_comment_restarts_a_completed_issue_and_redispatches_in_the_same_run():
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)], state=state)
    # The first GET this run makes is _restart_commented_completions's own
    # list_comments call (nothing in state.assignments/pending_questions for
    # the earlier passes to act on) -- everything after falls through to
    # the shared `issues` default, which _dispatch's own list_issues/
    # list_comments need.
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="maintainer", body="please also handle Y",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.completed_issues == {}
    assert orchestrator.state.assignments["sandbox-0"].issue == 5
    reopen_call = next(
        c for c in transport.calls
        if c["method"] == "PATCH" and c["path"] == "/repos/o/r/issues/5"
    )
    assert json.loads(reopen_call["body"]) == {"state": "open"}
    completed_removed = next(
        c for c in transport.calls
        if c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/5/labels/grain-agent-completed"
    )
    assert completed_removed
    trigger_added = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    )
    assert json.loads(trigger_added["body"]) == {"labels": ["grain-agent"]}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("maintainer" in o and "reopened" in o for o in outcomes)


def test_an_untrusted_comment_does_not_restart_a_completed_issue():
    """Same prompt-injection concern `_promote_answered_questions` already
    guards against: a random public commenter must not be able to restart
    the agent set on a completed issue just by leaving a comment.
    """
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="rando", body="please reopen this",
                          author_association="NONE"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert "5" in orchestrator.state.completed_issues
    assert orchestrator.state.assignments == {}
    assert not any(c["method"] == "PATCH" for c in transport.calls)


def test_no_new_comment_leaves_a_completed_issue_alone():
    # The only comment present is the one already accounted for by the
    # recorded baseline (id == baseline, not greater than it).
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(100, user="grain-agent-bot", body="the pr I opened",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.completed_issues["5"].baseline_comment_id == 100
    assert orchestrator.state.assignments == {}


def test_the_first_poll_after_completion_only_primes_the_baseline():
    """`CompletedIssue.baseline_comment_id` starts `None` -- the very first
    poll after completion has nothing fair to compare a first read
    against (a comment already on the issue when it finished isn't a
    reply to the completion), so it must only ever prime, never restart.
    """
    state = AutomationState()
    state.record_completed_issue(5)  # baseline still None -- freshly completed
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(50, user="maintainer", body="looks good",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.completed_issues["5"].baseline_comment_id == 50
    assert orchestrator.state.assignments == {}
    assert not any(c["method"] == "PATCH" for c in transport.calls)


def test_the_automation_comment_from_a_no_changes_finish_does_not_restart_its_own_task(
    monkeypatch, tmp_path,
):
    """bwsalmon/agents#135: `_finish_no_changes` posts its own automation
    comment before `completed_label` goes on. The same-cycle priming poll
    (`_restart_commented_completions`) must fold that comment into the
    baseline it primes, not mistake it for a human's reply and restart the
    very task that just finished.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    comment_file = tmp_path / "comment.txt"
    comment_file.write_text("Investigated X; no code change was needed.")
    monkeypatch.setattr(core_module, "comment_path", lambda unit: str(comment_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(404, {}, b"not found"),  # get_branch_head: no branch pushed
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),  # create_comment
        ApiResponse(200, {}, b"{}"),  # add_label (completed)
        ApiResponse(200, {}, b"{}"),  # remove_label (in-progress off)
        ApiResponse(200, {}, b"{}"),  # remove_label (agent label off)
        # _restart_commented_completions's own priming poll -- sees the
        # automation comment it just posted (id 555) already there.
        ApiResponse(200, {}, json.dumps([
            comment_json_for(555, user="grain-agent-bot",
                              body="This task has been completed with no code change",
                              author_association="OWNER"),
        ]).encode()),
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.completed_issues["5"].baseline_comment_id == 555
    assert orchestrator.state.assignments == {}
    assert not any(c["method"] == "PATCH" for c in transport.calls)


def test_a_404_while_checking_a_completed_issue_for_a_comment_clears_the_record():
    state = AutomationState()
    state.record_completed_issue(201)
    state.prime_completed_baseline(201, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(404, {}, b'{"message": "Not Found"}'))

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.completed_issues == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "201" in o for o in outcomes)


def test_a_non_404_error_while_checking_a_completed_issue_still_raises():
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(500, {}, b"boom"))

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_reopening_a_completed_issue_tolerates_a_404_for_a_stale_record():
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([
            comment_json_for(101, user="maintainer", body="go ahead",
                              author_association="OWNER"),
        ]).encode()),
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # reopen_issue
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.completed_issues == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("maintainer" in o and "not found" in o for o in outcomes)
    # Best-effort only: no label mutation was even attempted once the
    # reopen itself 404'd.
    assert not any(c["method"] in ("POST", "DELETE") for c in transport.calls)


def test_restarting_a_completed_issue_drops_its_stale_open_pr_record():
    """bwsalmon/agents#54's open-PR tracking, if this issue still has an
    entry, would otherwise let a later `_close_finished_prs` close the
    issue this restart just reopened the moment that old, now-beside-the-
    point PR itself closes.
    """
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="maintainer", body="one more thing",
                          author_association="OWNER"),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.open_pull_requests == {}


# --- restart orphaned in-progress issues on a lost state (bwsalmon/agents#139) --

def test_an_orphaned_in_progress_issue_is_requeued_to_the_trigger_label():
    """No `Assignment` anywhere in `AutomationState` for issue #5 -- the
    shape a wiped or never-loaded `state.json` leaves behind, e.g. after
    grain itself is restarted or reformatted -- but GitHub still shows it
    `in_progress_label`-ed from before that happened. `_dispatch`'s own
    poll only ever lists `trigger_label`, so nothing else in `run_once`
    would ever notice this issue again; `_restart_orphaned_in_progress`
    is the only pass that reads `in_progress_label` itself rather than
    trusting local state to already know about it.
    """
    orchestrator, transport = make_orchestrator(issues=[])
    transport.in_progress_issues_body = json.dumps([
        issue_json(5, labels=("grain-agent-in-progress", "grain-agent-working-0")),
    ]).encode()

    orchestrator.run_once(NOW)

    calls = transport.calls
    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/5/labels/grain-agent-in-progress"
        for c in calls
    )
    add_trigger = next(
        c for c in calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    )
    assert json.loads(add_trigger["body"]) == {"labels": ["grain-agent"]}
    # Every sandbox's working label is stripped, not just the one this
    # issue happened to still carry -- the assignment that would have said
    # which sandbox that was is exactly what's gone.
    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/5/labels/grain-agent-working-0"
        for c in calls
    )
    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/5/labels/grain-agent-working-1"
        for c in calls
    )
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("no known assignment" in o and "requeued" in o for o in outcomes)


def test_a_tracked_in_progress_issue_is_left_alone_by_the_orphan_restart():
    """The same `in_progress_label` listing, but this time
    `AutomationState` does have an `Assignment` for it -- an ordinary
    still-running task, not a lost one. This must never touch its labels;
    that is `_sweep`'s decision to make once the unit itself actually
    finishes, not something a lost-state fallback should preempt.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.in_progress_issues_body = json.dumps([
        issue_json(5, labels=("grain-agent-in-progress", "grain-agent-working-0")),
    ]).encode()
    # The cancel-on-close poll's own `get_issue` read, for the still-active
    # assignment `_sweep` leaves standing.
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(5)).encode()))

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    # `_refresh_agent_labels` re-applying the still-live working label is
    # expected and unrelated -- what must never happen is anything treating
    # this as an orphan: the in-progress label coming off, or the trigger
    # label going back on.
    assert not any(
        c["method"] == "DELETE" and c["path"].endswith("/labels/grain-agent-in-progress")
        for c in transport.calls
    )
    assert not any(
        c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
        and json.loads(c["body"]) == {"labels": ["grain-agent"]}
        for c in transport.calls
    )


def test_an_orphaned_in_progress_issue_tolerates_a_404_when_re_adding_the_trigger_label():
    """The issue named by a stale `in_progress_label` listing can vanish
    (or its number get reused into a PR, or the repo change out from under
    it) in the instant between that read and this write -- the same
    "stale listing/assignment" tolerance every other GitHub-facing call in
    this module already extends, not a reason to crash the whole cycle.
    """
    orchestrator, transport = make_orchestrator(issues=[])
    transport.in_progress_issues_body = json.dumps([
        issue_json(5, labels=("grain-agent-in-progress",)),
    ]).encode()
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),  # remove_label: in_progress_label
        ApiResponse(200, {}, b"{}"),  # remove_label: sandbox-0's agent label
        ApiResponse(200, {}, b"{}"),  # remove_label: sandbox-1's agent label
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # add_label: trigger_label
    ])

    orchestrator.run_once(NOW)  # must not raise

    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("no known assignment" in o and "not found" in o for o in outcomes)


def test_an_orphaned_in_progress_issue_is_redispatched_in_the_same_run():
    """End-to-end: once the trigger label is back on, the exact same
    `run_once` cycle's own `_dispatch` pass -- which runs after this --
    picks the issue straight back up, the same "recovered without waiting
    for a second cron tick" property bwsalmon/agents#51's crash-recovery
    fix already gives a *tracked* stranded assignment.
    """
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)])
    transport.in_progress_issues_body = json.dumps([
        issue_json(5, labels=("grain-agent-in-progress",)),
    ]).encode()
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),  # remove_label: in_progress_label
        ApiResponse(200, {}, b"{}"),  # remove_label: sandbox-0's agent label
        ApiResponse(200, {}, b"{}"),  # remove_label: sandbox-1's agent label
        ApiResponse(200, {}, b"{}"),  # add_label: trigger_label
        # _dispatch's own list_issues(trigger_label) poll -- spelled out
        # rather than left to fall through to `default`, since the four
        # calls above already emptied the queue by the time it would run.
        ApiResponse(200, {}, json.dumps([issue_json(5)]).encode()),
        ApiResponse(200, {}, b"[]"),  # _dispatch's own list_comments
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments["sandbox-0"].issue == 5


# --- PR creation on a successful run (docs/roadmap.md item 2) -------------

def test_a_succeeded_run_verifies_the_branch_then_opens_a_pr():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    branch_call = transport.calls[0]
    assert branch_call["method"] == "GET"
    assert branch_call["path"] == "/repos/o/r/branches/grain%2Fissue-5"
    pr_call = next(c for c in transport.calls if c["method"] == "POST" and c["path"] == "/repos/o/r/pulls")
    assert pr_call["path"] == "/repos/o/r/pulls"
    sent = json.loads(pr_call["body"])
    assert sent["head"] == "grain/issue-5"
    assert sent["base"] == "main"
    # docs/roadmap.md item 14: every PR grain-agent opens carries a
    # consistent, visible marker distinguishing it from a human-authored one.
    assert "🤖" in sent["title"]
    # bwsalmon/agents#4: the issue's own title rides along, not just its
    # number, so a PR is identifiable from the list view alone.
    assert sent["title"] == "🤖 grain: o/r#5: issue 42"
    # bwsalmon/agents#79: the body leads with the pushed branch's own head
    # commit message -- a real account of the change -- rather than only
    # generic metadata.
    assert sent["body"].startswith(DEFAULT_COMMIT_MESSAGE)
    assert "Posted automatically by grain-agent" in sent["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("opened PR o/r#42" in o for o in outcomes)
    # bwsalmon/agents#54: the task issue is *not* closed the moment the PR
    # is opened any more -- only tagged as agent-completed. It gets closed
    # later, once the PR itself closes (see the dedicated tests for that).
    assert not any(c["method"] == "PATCH" for c in transport.calls)
    completed_call = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    )
    assert json.loads(completed_call["body"]) == {"labels": ["grain-agent-completed"]}
    # The completed label goes on and the in-progress label comes off; the
    # trigger label is never re-added for a genuinely finished run.
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    label_mutations = [c for c in mutating if "labels" in c["path"]]
    assert len(label_mutations) == 3  # completed on, in-progress off, agent label off
    # The open PR is recorded so a later run can close the issue once it does.
    assert orchestrator.state.open_pull_requests == {
        "5": OpenPullRequest(issue=5, target_owner="o", target_repo="r", pr_number=42),
    }


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
    assert len(mutating) == 3  # remove in-progress, add trigger back, remove agent label


def test_a_succeeded_auto_merge_task_records_that_on_its_open_pr():
    """The `/auto-merge` directive's whole path end to end, short of
    `_close_finished_prs` itself (covered separately below): `Assignment`
    carries it from dispatch, `sweep()`'s `Outcome` carries it through the
    slot freeing, and `_finish_succeeded_issue` writes it onto the
    `OpenPullRequest` record it creates -- bwsalmon/agents#83.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), auto_merge=True)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert orchestrator.state.open_pull_requests["5"].auto_merge is True


# --- closing on PR close (bwsalmon/agents#54) ------------------------------

def test_close_finished_prs_leaves_the_issue_alone_while_the_pr_is_still_open():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(open_pr_response(42, state="open"))

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert orchestrator.state.open_pull_requests == {
        "5": OpenPullRequest(issue=5, target_owner="o", target_repo="r", pr_number=42),
    }


def test_close_finished_prs_closes_the_task_issue_once_its_pr_closes():
    """The core behaviour bwsalmon/agents#54 asked for: a task issue closes
    once the PR opened for it does -- merged or closed without merging both
    read "closed" here and are treated the same.
    """
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, state="closed"),
        ApiResponse(200, {}, b"{}"),  # close_issue
    ])

    orchestrator.run_once(NOW)

    close_call = next(c for c in transport.calls if c["method"] == "PATCH")
    assert close_call["path"] == "/repos/o/r/issues/5"
    assert json.loads(close_call["body"]) == {"state": "closed"}
    assert orchestrator.state.open_pull_requests == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("closed" in o and "o/r#42" in o for o in outcomes)


def test_close_finished_prs_closes_the_task_issue_in_the_task_repo_not_the_target():
    state = AutomationState()
    state.record_open_pr(5, "other", "service", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state, allowed=("o/r", "other/service"))
    transport.responses.extend([
        open_pr_response(42, state="closed"),
        ApiResponse(200, {}, b"{}"),  # close_issue
    ])

    orchestrator.run_once(NOW)

    # Not transport.calls[0] any more (bwsalmon/agents#139):
    # `_restart_orphaned_in_progress`'s own unconditional listing now runs
    # ahead of `_close_finished_prs` in `run_once` and is call zero instead.
    pr_get_call = next(c for c in transport.calls if c["method"] == "GET" and "/pulls/" in c["path"])
    assert pr_get_call["path"] == "/repos/other/service/pulls/42"
    close_call = next(c for c in transport.calls if c["method"] == "PATCH")
    assert close_call["path"] == "/repos/o/r/issues/5"


def test_close_finished_prs_tolerates_a_404_from_get_pull_request():
    """A stale record (the target repo or PR named in it is gone -- an
    operator changed the allowlist, or the PR was deleted outright) must
    not crash the cycle, the same "stale assignment" tolerance `_requeue`
    and `_finish_question` already have.
    """
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(404, {}, b'{"message": "Not Found"}'))

    orchestrator.run_once(NOW)  # must not raise

    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert orchestrator.state.open_pull_requests == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "stale" in o for o in outcomes)


def test_close_finished_prs_tolerates_a_404_from_close_issue():
    # The PR really did close, but the task issue itself is gone from the
    # currently configured task repo -- there is nothing left to close.
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, state="closed"),
        ApiResponse(404, {}, b'{"message": "Not Found"}'),
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.open_pull_requests == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("not found" in o and "stale assignment" in o for o in outcomes)


def test_close_finished_prs_still_raises_a_non_404_error_from_get_pull_request():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(500, {}, b"internal error"))

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_close_finished_prs_still_raises_a_non_404_error_from_close_issue():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, state="closed"),
        ApiResponse(500, {}, b"internal error"),
    ])

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


# --- suggesting a fix for conflicts/failing checks (bwsalmon/agents#83) ----

def failing_check_runs_body(*names: str) -> bytes:
    return json.dumps({
        "total_count": len(names),
        "check_runs": [
            {"name": n, "status": "completed", "conclusion": "failure"} for n in names
        ],
    }).encode()


def pending_check_runs_body(name: str = "tests") -> bytes:
    return json.dumps({
        "total_count": 1,
        "check_runs": [{"name": name, "status": "in_progress", "conclusion": None}],
    }).encode()


def new_issue_response(number: int) -> ApiResponse:
    return ApiResponse(201, {}, json.dumps(issue_json(number)).encode())


def test_close_finished_prs_suggests_a_fix_when_the_pr_has_conflicts():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, mergeable=False),
        new_issue_response(100),  # create_issue
        ApiResponse(201, {}, json.dumps({"id": 1}).encode()),  # create_comment
    ])

    orchestrator.run_once(NOW)

    create_call = next(c for c in transport.calls if c["path"] == "/repos/o/r/issues")
    sent = json.loads(create_call["body"])
    assert sent["labels"] == ["grain-agent-needs-approval"]
    assert "conflicts with" in sent["body"]
    assert "/repo o/r" in sent["body"]
    assert "/base feature-x" in sent["body"]
    assert "/auto-merge true" in sent["body"]
    assert orchestrator.state.open_pull_requests["5"].fix_issue == 100
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("suggested fix o/r#100" in o for o in outcomes)


def test_close_finished_prs_suggests_a_fix_when_a_check_is_failing():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.check_runs_body = failing_check_runs_body("unit-tests")
    transport.responses.extend([
        open_pr_response(42, mergeable=True),
        new_issue_response(100),
        ApiResponse(201, {}, json.dumps({"id": 1}).encode()),
    ])

    orchestrator.run_once(NOW)

    create_call = next(c for c in transport.calls if c["path"] == "/repos/o/r/issues")
    sent = json.loads(create_call["body"])
    assert "`unit-tests`" in sent["body"]
    assert orchestrator.state.open_pull_requests["5"].fix_issue == 100


def test_close_finished_prs_does_not_suggest_a_fix_twice():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    state.mark_fix_suggested(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    # _promote_lgtm_comments runs first now that a fix is on record
    # (bwsalmon/agents#136) -- it lists issues carrying needs_approval_label
    # before _close_finished_prs gets to its own get_pull_request poll.
    transport.responses.append(ApiResponse(200, {}, b"[]"))
    transport.responses.append(open_pr_response(42, mergeable=False))

    orchestrator.run_once(NOW)

    assert not any(c["path"] == "/repos/o/r/issues" and c["method"] == "POST"
                   for c in transport.calls)
    assert orchestrator.state.open_pull_requests["5"].fix_issue == 100


def test_close_finished_prs_never_suggests_a_fix_for_an_auto_merge_pr():
    """A fix task's own PR going wrong must not chain into a second fix
    suggestion -- that risks an unbounded chain of them. It's left open,
    visibly, for a human to notice instead.
    """
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(open_pr_response(42, mergeable=False))

    orchestrator.run_once(NOW)

    assert not any(c["path"] == "/repos/o/r/issues" for c in transport.calls)
    assert orchestrator.state.open_pull_requests["5"].fix_issue is None


def test_close_finished_prs_tolerates_a_404_while_suggesting_a_fix():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, mergeable=False),
        ApiResponse(404, {}, b'{"message": "Not Found"}'),
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.open_pull_requests["5"].fix_issue is None


# --- lgtm approval on comment (bwsalmon/agents#136) -------------------------

def test_an_lgtm_comment_approves_a_suggested_fix_the_same_as_trigger_label_would():
    """A trusted `/lgtm` comment on a `needs_approval_label` issue
    (`_suggest_fix`, bwsalmon/agents#83) is a second way to approve it,
    alongside a human applying `trigger_label` by hand
    (`test_dispatch_strips_the_needs_approval_label_once_a_human_approves`).
    """
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    state.mark_fix_suggested(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(100, labels=("grain-agent-needs-approval",))]
        ).encode()),
        ApiResponse(200, {}, json.dumps([
            comment_json_for(200, user="maintainer", body="looks good\n/lgtm",
                              author_association="OWNER"),
        ]).encode()),
        ApiResponse(200, {}, b"{}"),  # remove_label(needs_approval_label)
        ApiResponse(200, {}, b"{}"),  # add_label(trigger_label)
        open_pr_response(42, mergeable=True),
    ])

    orchestrator.run_once(NOW)

    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/100/labels/grain-agent-needs-approval"
        for c in transport.calls
    )
    added = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/100/labels"
    )
    assert json.loads(added["body"]) == {"labels": ["grain-agent"]}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("maintainer" in o and "/lgtm" in o for o in outcomes)


def test_an_untrusted_lgtm_comment_does_not_approve_a_suggested_fix():
    """Same prompt-injection concern every other comment-triggered
    promotion in this module already guards against (see
    `test_an_untrusted_comment_does_not_restart_a_completed_issue`): a
    random public commenter must not be able to approve a suggested fix
    for the agent set to attempt just by leaving a comment.
    """
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    state.mark_fix_suggested(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(100, labels=("grain-agent-needs-approval",))]
        ).encode()),
        ApiResponse(200, {}, json.dumps([
            comment_json_for(200, user="rando", body="/lgtm",
                              author_association="NONE"),
        ]).encode()),
        open_pr_response(42, mergeable=True),
    ])

    orchestrator.run_once(NOW)

    assert not any(c["method"] in ("DELETE", "POST") and "/labels" in c["path"]
                   for c in transport.calls)


def test_a_comment_without_lgtm_does_not_approve_a_suggested_fix():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    state.mark_fix_suggested(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(100, labels=("grain-agent-needs-approval",))]
        ).encode()),
        ApiResponse(200, {}, json.dumps([
            comment_json_for(200, user="maintainer", body="not yet, this needs more work",
                              author_association="OWNER"),
        ]).encode()),
        open_pr_response(42, mergeable=True),
    ])

    orchestrator.run_once(NOW)

    assert not any(c["method"] in ("DELETE", "POST") and "/labels" in c["path"]
                   for c in transport.calls)


def test_no_suggested_fix_on_record_skips_the_lgtm_poll_entirely():
    """The overwhelming common case: no `_suggest_fix` has ever run, so
    `state.open_pull_requests` carries no `fix_issue` at all. This must
    add no GitHub call to a plain `run_once` -- see
    `_promote_lgtm_comments`'s own docstring for why that guard matters.
    """
    orchestrator, transport = make_orchestrator(issues=[])

    orchestrator.run_once(NOW)

    assert not any(
        c["method"] == "GET" and "labels=grain-agent-needs-approval" in c["path"]
        for c in transport.calls
    )


# --- proposing new tasks (bwsalmon/agents#175) ------------------------------

def test_a_succeeded_run_files_proposed_tasks_before_finishing(monkeypatch, tmp_path):
    """`propose_task` calls made this run are filed as fresh task-repo
    issues, each carrying `needs_approval_label` -- the same "grain
    suggests, a human decides" gate `_suggest_fix` already uses -- before
    the rest of this outcome's own finish handling runs. A `depends_on`
    entry is resolved against either an existing issue number (`55` here)
    or an earlier proposal's own `id` in the same batch (`infra`), in the
    order the agent proposed them.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    tasks_file = tmp_path / "proposed-tasks.json"
    tasks_file.write_text(json.dumps([
        {"id": "infra", "title": "Set up infra", "body": "do infra work", "depends_on": []},
        {"id": None, "title": "Build feature", "body": "do feature work",
         "depends_on": ["infra", "55"]},
    ]))
    monkeypatch.setattr(core_module, "proposed_tasks_path", lambda unit: str(tasks_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        [new_issue_response(101), new_issue_response(102),
         ApiResponse(201, {}, json.dumps({"id": 900}).encode())]  # create_comment (summary)
        + pr_flow_response(42)
        + [
            completion_priming_response(),
            # `_promote_lgtm_comments` now also polls this cycle, since the
            # two issues just filed above put something in
            # `state.proposed_task_issues` for the first time.
            ApiResponse(200, {}, b"[]"),
            open_pr_response(42),
        ]
    )

    orchestrator.run_once(NOW)

    created = [c for c in transport.calls
               if c["method"] == "POST" and c["path"] == "/repos/o/r/issues"]
    assert len(created) == 2
    first, second = (json.loads(c["body"]) for c in created)
    assert first["labels"] == ["grain-agent-needs-approval"]
    assert second["labels"] == ["grain-agent-needs-approval"]
    assert "Set up infra" in first["title"]
    assert "Proposed by o/r#5" in first["body"]
    assert "/depends" not in first["body"]
    assert "do feature work" in second["body"]
    assert "/depends 101,55" in second["body"]

    summary = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/comments"
    )
    sent = json.loads(summary["body"])
    assert "o/r#101" in sent["body"] and "o/r#102" in sent["body"]

    assert orchestrator.state.proposed_task_issues == {101, 102}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("filed 2 proposed task(s): o/r#101, o/r#102" in o for o in outcomes)


def test_a_succeeded_run_with_no_proposed_tasks_files_nothing():
    """The overwhelming common case: the agent never called `propose_task`
    this run, so `proposed_tasks_path` is absent (`dispatch.py` never
    writes it unless the tool is called) -- must not add any GitHub call
    or change `run_once`'s existing behaviour at all.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "POST" and c["path"] == "/repos/o/r/issues"
                   for c in transport.calls)
    assert orchestrator.state.proposed_task_issues == set()


def test_file_proposed_tasks_drops_an_unresolvable_dependency_with_a_note(
    monkeypatch, tmp_path,
):
    """A `depends_on` entry naming neither a digit nor a seen `id` (a typo,
    a forward reference, or the proposal's own `id`) must not lose the
    whole proposal -- it's dropped, with a note in the filed issue's own
    body, and everything else about the proposal still gets filed.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    tasks_file = tmp_path / "proposed-tasks.json"
    tasks_file.write_text(json.dumps([
        {"id": None, "title": "Orphaned", "body": "x", "depends_on": ["nonexistent"]},
    ]))
    monkeypatch.setattr(core_module, "proposed_tasks_path", lambda unit: str(tasks_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        [new_issue_response(101), ApiResponse(201, {}, json.dumps({"id": 900}).encode())]
        + pr_flow_response(42)
        + [
            completion_priming_response(),
            # `_promote_lgtm_comments` now also polls this cycle -- same
            # reason as the multi-proposal test above.
            ApiResponse(200, {}, b"[]"),
            open_pr_response(42),
        ]
    )

    orchestrator.run_once(NOW)

    created = next(c for c in transport.calls
                    if c["method"] == "POST" and c["path"] == "/repos/o/r/issues")
    sent = json.loads(created["body"])
    assert "/depends" not in sent["body"]
    assert "nonexistent" in sent["body"]
    assert "dropped" in sent["body"]


def test_file_proposed_tasks_tolerates_a_404_and_stops_filing_the_rest(
    monkeypatch, tmp_path,
):
    """Same "stale config, not a crash" tolerance `_suggest_fix` already
    holds a 404 to: the task repo itself is gone from underneath this
    deployment, so nothing further in this batch gets filed either -- but
    the rest of `run_once` still proceeds normally.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    tasks_file = tmp_path / "proposed-tasks.json"
    tasks_file.write_text(json.dumps([
        {"id": None, "title": "First", "body": "x", "depends_on": []},
        {"id": None, "title": "Second", "body": "y", "depends_on": []},
    ]))
    monkeypatch.setattr(core_module, "proposed_tasks_path", lambda unit: str(tasks_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend(
        [ApiResponse(404, {}, b'{"message": "Not Found"}')]
        + pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.proposed_task_issues == set()
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("First" in o and "not found" in o for o in outcomes)


def test_a_proposed_task_on_record_does_not_skip_the_lgtm_poll():
    """The mirror image of `test_no_suggested_fix_on_record_skips_the_lgtm_poll_entirely`:
    an outstanding proposed task (bwsalmon/agents#175) is exactly the other
    reason `_promote_lgtm_comments`'s guard now polls at all.
    """
    state = AutomationState()
    state.record_proposed_task(100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, b"[]"))  # list_issues(needs_approval)

    orchestrator.run_once(NOW)

    assert any(
        c["method"] == "GET" and "labels=grain-agent-needs-approval" in c["path"]
        for c in transport.calls
    )


def test_an_lgtm_comment_approves_a_proposed_task_and_clears_it_from_state():
    """A trusted `/lgtm` comment promotes a proposed task the same way it
    already promotes a `_suggest_fix` one (`test_an_lgtm_comment_approves_a_suggested_fix_the_same_as_trigger_label_would`),
    and -- unlike `fix_issue`, which is never cleared -- also drops it from
    `state.proposed_task_issues`, since there is no later "original PR
    closed" event to lean on for this feature the way `_suggest_fix` has.
    """
    state = AutomationState()
    state.record_proposed_task(100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(100, labels=("grain-agent-needs-approval",))]
        ).encode()),
        ApiResponse(200, {}, json.dumps([
            comment_json_for(200, user="maintainer", body="looks good\n/lgtm",
                              author_association="OWNER"),
        ]).encode()),
        ApiResponse(200, {}, b"{}"),  # remove_label(needs_approval_label)
        ApiResponse(200, {}, b"{}"),  # add_label(trigger_label)
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.proposed_task_issues == set()


# --- auto-merging a stacked fix PR (bwsalmon/agents#83) --------------------

def test_close_finished_prs_auto_merges_a_clean_auto_merge_pr():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, mergeable=True),
        ApiResponse(200, {}, b'{"merged": true}'),  # merge_pull_request
        ApiResponse(200, {}, b"{}"),  # close_issue
    ])

    orchestrator.run_once(NOW)

    merge_call = next(c for c in transport.calls if c["path"] == "/repos/o/r/pulls/42/merge")
    assert merge_call["method"] == "PUT"
    close_call = next(c for c in transport.calls if c["method"] == "PATCH")
    assert close_call["path"] == "/repos/o/r/issues/5"
    assert orchestrator.state.open_pull_requests == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("auto-merged o/r#42" in o for o in outcomes)


def test_close_finished_prs_does_not_auto_merge_while_mergeable_is_still_unknown():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(open_pr_response(42, mergeable=None))

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "PUT" for c in transport.calls)
    assert orchestrator.state.open_pull_requests["5"].pr_number == 42


def test_close_finished_prs_does_not_auto_merge_while_checks_are_pending():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.check_runs_body = pending_check_runs_body()
    transport.responses.append(open_pr_response(42, mergeable=True))

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "PUT" for c in transport.calls)


def test_close_finished_prs_does_not_auto_merge_a_pr_with_a_failing_check():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.check_runs_body = failing_check_runs_body("unit-tests")
    transport.responses.append(open_pr_response(42, mergeable=True))

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "PUT" for c in transport.calls)


def test_close_finished_prs_retries_a_failed_auto_merge_next_cycle():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42, auto_merge=True)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.extend([
        open_pr_response(42, mergeable=True),
        ApiResponse(405, {}, b"not mergeable"),
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert orchestrator.state.open_pull_requests["5"].pr_number == 42
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("auto-merge" in o and "failed" in o for o in outcomes)


# --- per-sandbox agent labelling (bwsalmon/agents#95) ----------------------

def test_dispatch_labels_the_issue_with_the_sandbox_that_took_it():
    orchestrator, transport = make_orchestrator(issues=[issue_json(1)])
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments["sandbox-0"].issue == 1
    add_calls = [
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/1/labels"
    ]
    assert any(json.loads(c["body"]) == {"labels": ["grain-agent-working-0"]} for c in add_calls)


def test_a_refresh_tolerates_a_404_for_a_stale_assignment():
    """The same "stale assignment, log and move on" tolerance every other
    GitHub-facing call in a cycle already has (bwsalmon/agents#51 and
    friends) -- a leftover assignment pointing at an issue number that no
    longer exists in the currently configured repo must not crash the
    refresh pass before `_dispatch` ever runs.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=201, unit="grain-task-sandbox-0", now=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=active\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(issue_json(201)).encode()),  # cancel-on-close poll
        ApiResponse(404, {}, b'{"message": "Not Found"}'),  # refresh's own add_label
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert "sandbox-0" in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(
        "could not refresh agent label" in o and "stale assignment" in o
        for o in outcomes
    )


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


def test_dispatch_points_the_mcp_server_at_grain_agents_own_ssh_key_copy():
    # Found live (docs/roadmap.md item 8's "Update"): OpenSSH refuses to
    # use a private key file it considers group-readable at all, so
    # grain-agent's MCP server can't share AutomationConfig.ssh_key_path
    # (root-owned, used by this same process for its own SSH calls) --
    # it needs CONTROLLER_AGENT_SSH_KEY_PATH, a separate, independently
    # owner-only copy. Locks down the split that bug was in, so it can't
    # silently regress back to the broken shared-file shape.
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)])
    orchestrator.run_once(NOW)
    runner = orchestrator.base_runner
    mcp_config_dd = next(
        stdin for argv, stdin in runner.calls
        if argv[:2] == ["sudo", "dd"] and "mcp-config.json" in argv[2]
    )
    mcp_config = json.loads(mcp_config_dd)
    server_args = mcp_config["mcpServers"]["grain-sandbox"]["args"]
    key_path = server_args[server_args.index("--key-path") + 1]
    assert key_path == CONTROLLER_AGENT_SSH_KEY_PATH
    # Not this process's own key (AutomationConfig's default) -- that one's
    # root-owned and group-unreadable, exactly the file OpenSSH rejected.
    assert key_path != "/data/secrets/controller-ssh"


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
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("health warning:") for o in outcomes)
    warning_entries = [e for e in orchestrator.audit.entries
                        if e["outcome"].startswith("health warning:")]
    assert warning_entries[0]["sandbox"] == "sandbox-0"
    # Still freed and still gets its PR — health doesn't block the sweep's
    # own success handling.
    assert "sandbox-0" not in orchestrator.state.assignments
    assert any("opened PR o/r#42" in o for o in outcomes)


def test_a_freed_sandboxs_failed_gemini_key_revocation_is_logged_but_still_freed():
    """Same visibility-only treatment as the health-warning case above
    (bwsalmon/agents#47): a Gemini key that fails to revoke when its task's
    sandbox is freed doesn't block the sweep -- it's surfaced in the audit
    log for an operator to revoke by hand.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1),
                 gemini_key_name="projects/proj/locations/global/keys/abc")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    runner.expect("true", returncode=0)
    runner.expect("systemctl is-system-running", stdout="running\n")
    runner.expect("docker info", stdout="Server Version: 27.0.0\n")
    runner.expect(
        "df -P /",
        stdout="Filesystem 1024-blocks Used Available Capacity Mounted on\n"
               "/dev/vda1 1 1 1 10% /\n",
    )
    runner.expect("gcloud services api-keys delete", returncode=1, stderr="API unreachable")
    orchestrator, transport = make_orchestrator(
        issues=[], state=state, runner=runner,
        gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments  # still freed
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(
        o.startswith("credential warning:") and "API unreachable" in o for o in outcomes
    )


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


# --- GCP metadata server (bwsalmon/agents#98) --------------------------

GCP_KEY_EMAIL = "narrow@p.iam.gserviceaccount.com"
GCP_KEY_JSON = '{"type": "service_account", "private_key": "fake"}'


def test_dispatch_does_nothing_gcp_key_related_when_none_is_configured():
    """The "not configured" default (`gcp_key_config = None`) must not
    change a single existing dispatch's behaviour -- every test above this
    one runs with no gcp_key_config at all."""
    runner = FakeRunner()
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], runner=runner)
    orchestrator.run_once(NOW)
    assert not runner.ran("gcloud iam service-accounts")


def test_dispatch_mints_a_gcp_key_unconditionally_when_configured():
    """Unlike the Gemini key, no task label is needed (bwsalmon/agents#126,
    mirroring the old metadata broker's "every sandbox, every dispatch"
    behaviour) -- a plain issue with no special label still gets one."""
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="abc123\n")
    runner.expect("cat", stdout=GCP_KEY_JSON)
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)
    create_call = next(c for c in runner.commands if "keys create" in c)
    assert f"--iam-account={GCP_KEY_EMAIL}" in create_call


def test_dispatch_places_the_minted_gcp_key_in_the_sandbox():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="abc123\n")
    runner.expect("cat", stdout=GCP_KEY_JSON)
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)
    dd_calls = [(argv, stdin) for argv, stdin in runner.calls if argv[0] == "dd"]
    key_call = next(c for c in dd_calls if c[0][1] == f"of={GCP_KEY_PATH}")
    assert key_call[1] == GCP_KEY_JSON


def test_gcp_key_is_recorded_on_the_assignment_for_later_revocation():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="abc123\n")
    runner.expect("cat", stdout=GCP_KEY_JSON)
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments["sandbox-0"].gcp_key_id == "abc123"


def test_gcp_key_mint_failure_degrades_to_dispatching_without_one():
    """bwsalmon/agents#138: unlike a dispatch failure (below), a broken
    minter must not become a standing veto on every dispatch for as long
    as it stays broken -- the sandbox still gets its task, just without a
    GCP key, and the failure is only surfaced for visibility."""
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", returncode=1,
                  stderr="PERMISSION_DENIED")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments["sandbox-0"].gcp_key_id is None
    assert not runner.ran("keys delete")
    outcomes = [entry["outcome"] for entry in orchestrator.audit.entries]
    assert any("dispatched to" in o and "PERMISSION_DENIED" in o for o in outcomes)


def test_a_dispatch_failure_after_minting_a_gcp_key_revokes_it():
    """The key was minted but dispatch() never got far enough to record an
    Assignment for sweeper.py to later revoke it through -- must not leak.
    """
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="abc123\n")
    runner.expect("cat", stdout=GCP_KEY_JSON)
    runner.expect("bash -c", returncode=128, stderr="fatal: Authentication failed")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)  # must not raise
    assert orchestrator.state.assignments == {}
    delete_call = next(c for c in runner.commands if "keys delete" in c)
    assert "abc123" in delete_call
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatch failed" in o for o in outcomes)


def test_a_dispatch_failure_whose_gcp_key_cleanup_also_fails_reports_both():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="abc123\n")
    runner.expect("cat", stdout=GCP_KEY_JSON)
    runner.expect("bash -c", returncode=128, stderr="fatal: Authentication failed")
    runner.expect("gcloud iam service-accounts keys delete", returncode=1,
                  stderr="cleanup also failed")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(1)], runner=runner,
        gcp_key_config=GcpKeyConfig(service_account_email=GCP_KEY_EMAIL, project_id="p"),
    )
    orchestrator.run_once(NOW)
    outcomes = [entry["outcome"] for entry in orchestrator.audit.entries]
    combined = next(o for o in outcomes if "dispatch failed" in o)
    assert "Authentication failed" in combined
    assert "cleanup also failed" in combined


# --- PR-continuation dispatch (docs/roadmap.md item 9, via a `/pr` task) ---

def test_a_pr_directive_dispatches_to_the_prs_own_existing_branch():
    """A task issue carrying `/pr N` continues that PR in the target repo,
    instead of starting a fresh `grain/issue-<n>` branch -- the shape item
    9's labelled-PR trigger became once PRs stopped living in the polled
    repo.
    """
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(3, body="fix the review comments\n/repo o/r\n/pr 7")]
    )
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(3, body="fix the review comments\n/repo o/r\n/pr 7")]
        ).encode()),                                                  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(200, {}, json.dumps(
            pr_detail_json(7, head_ref="feature-x")).encode()),       # get_pull_request
        ApiResponse(200, {}, b"[]"),                                  # list_review_comments
    ])

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    # The trigger's number is the *task issue's*, not the PR's: that is what
    # carries the labels and what a requeue or a question is filed against.
    assert assignment.issue == 3
    assert assignment.kind is TriggerKind.PR
    assert assignment.branch == "feature-x"
    runner = orchestrator.base_runner
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert any("checkout -f -B feature-x origin/feature-x" in c[2] for c in clone_calls)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatched to o/r (PR #7)" in o for o in outcomes)


def test_a_pr_task_is_skipped_when_the_pool_is_full_same_as_any_other():
    # The shared-budget decision: a PR-continuation task competes for the
    # same free-sandbox pool as a fresh one, no separate budget.
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit="grain-task-sandbox-0", now=NOW)
    state.assign("sandbox-1", issue=2, unit="grain-task-sandbox-1", now=NOW)
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n",
    )
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(9, body="/repo o/r\n/pr 4")], state=state, runner=runner,
    )
    # bwsalmon/agents#82: one cancel-on-close `get_issue` poll per
    # still-active assignment the sweep leaves alone -- both answered
    # open, so neither sandbox is touched.
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(1)).encode()))
    transport.responses.append(ApiResponse(200, {}, json.dumps(issue_json(2)).encode()))

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments  # still occupied, untouched
    assert "sandbox-1" in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: no free sandbox" in outcomes


def test_a_pr_triggered_success_pushes_commits_and_does_not_open_a_new_pr():
    state = AutomationState()
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x",
                 target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),   # branch_exists("feature-x"): true
        ApiResponse(200, {}, b"{}"),   # add_label: completed
        ApiResponse(200, {}, b"{}"),   # remove_label: in-progress off
        ApiResponse(200, {}, b"{}"),   # remove_label: agent label off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("pushed additional commits to o/r" in o for o in outcomes)
    # No PR-creation call at all -- the PR this dispatch worked already existed.
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 3  # completed label goes on, in-progress comes off, agent label off
    completed_call = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/7/labels"
    )
    assert json.loads(completed_call["body"]) == {"labels": ["grain-agent-completed"]}
    # bwsalmon/agents#23's close_issue is issue-triggered only -- a
    # PR-triggered task is continuing an existing PR, which has its own
    # lifecycle, so this path must never close anything. Unlike the
    # fresh-branch path (bwsalmon/agents#54) there is also no open-PR
    # record to poll later: the PR predates the task and isn't this
    # deployment's to close.
    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert orchestrator.state.open_pull_requests == {}


def test_a_pr_triggered_run_with_no_new_branch_is_requeued_not_dropped():
    state = AutomationState()
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x",
                 target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(404, {}, b"not found"))  # branch_exists: false

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("does not exist" in o for o in outcomes)
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 3  # remove in-progress, add trigger back, remove
                               # agent label — same requeue path a
                               # failed/stranded run takes


# --- review dispatch (bwsalmon/agents#154) ----------------------------------

def test_a_review_directive_dispatches_to_the_prs_own_existing_branch_read_only():
    """A task issue carrying `/pr N` plus `/review true` reads that PR's
    branch instead of continuing work on it -- the review-mode counterpart
    to `test_a_pr_directive_dispatches_to_the_prs_own_existing_branch`.
    """
    body = "please review this\n/repo o/r\n/pr 7\n/review true"
    orchestrator, transport = make_orchestrator(issues=[issue_json(3, body=body)])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(3, body=body)]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                            # list_comments
        ApiResponse(200, {}, json.dumps(
            pr_detail_json(7, head_ref="feature-x")).encode()),                 # get_pull_request
    ])

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert assignment.issue == 3
    assert assignment.kind is TriggerKind.REVIEW
    assert assignment.branch == "feature-x"
    assert assignment.pr_number == 7
    runner = orchestrator.base_runner
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert any("checkout -f -B feature-x origin/feature-x" in c[2] for c in clone_calls)
    prompt = next(
        stdin for argv, stdin in runner.calls
        if argv[:1] == ["sudo"] and any("prompt.md" in a for a in argv)
    )
    assert "add_review_comment" in prompt
    assert "git push" not in prompt
    # No inline-review-comment or top-level-conversation fetch: a review
    # dispatch reads the PR fresh, unlike a `/pr`-continuation dispatch.
    assert not any(c["path"] == "/repos/o/r/pulls/7/comments" for c in transport.calls)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatched to o/r (review of PR #7)" in o for o in outcomes)


def test_a_review_directive_without_a_pr_is_parked_not_dispatched():
    body = "/repo o/r\n/review true"
    orchestrator, transport = make_orchestrator(issues=[issue_json(4, body=body)])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(4, body=body)]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                            # list_comments
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),                 # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "/pr" in json.loads(comment["body"])["body"]


def test_a_review_triggered_success_posts_a_draft_review_with_inline_and_general_comments(
    monkeypatch, tmp_path,
):
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.REVIEW, branch="feature-x",
                 pr_number=7, target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    review_file = tmp_path / "review.json"
    review_file.write_text(json.dumps([
        {"body": "looks fine overall", "path": None, "line": None},
        {"body": "consider a docstring here", "path": "src/thing.py", "line": 12},
    ]))
    monkeypatch.setattr(core_module, "review_path", lambda unit: str(review_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),                              # branch_exists: true
        ApiResponse(200, {}, json.dumps({"id": 555}).encode()),   # create_review
        ApiResponse(200, {}, b"{}"),                              # add_label: completed
        ApiResponse(200, {}, b"{}"),                              # remove_label: in-progress off
        ApiResponse(200, {}, b"{}"),                              # remove_label: agent label off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    review_call = next(c for c in transport.calls if c["method"] == "POST"
                        and c["path"] == "/repos/o/r/pulls/7/reviews")
    sent = json.loads(review_call["body"])
    assert "event" not in sent  # never auto-submitted -- see github.py's create_review
    assert "looks fine overall" in sent["body"]
    assert "Posted automatically by grain-agent" in sent["body"]
    assert sent["comments"] == [
        {"path": "src/thing.py", "line": 12, "body": "consider a docstring here"}
    ]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("posted a draft review on o/r#7 (1 inline comment(s))" in o for o in outcomes)


def test_a_review_triggered_success_with_no_comments_posts_no_review(monkeypatch, tmp_path):
    """The agent looking and finding nothing worth flagging is a normal
    outcome, not a reason to post an empty draft review nobody asked for.
    """
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.REVIEW, branch="feature-x",
                 pr_number=7, target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    # Never written -- the agent never called add_review_comment.
    monkeypatch.setattr(core_module, "review_path",
                         lambda unit: str(tmp_path / "review.json"))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),   # branch_exists: true
        ApiResponse(200, {}, b"{}"),   # add_label: completed
        ApiResponse(200, {}, b"{}"),   # remove_label: in-progress off
        ApiResponse(200, {}, b"{}"),   # remove_label: agent label off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    assert not any(c["path"] == "/repos/o/r/pulls/7/reviews" for c in transport.calls)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("left no review comments" in o for o in outcomes)


def test_a_review_triggered_run_with_no_branch_is_requeued_not_dropped():
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.REVIEW, branch="feature-x",
                 pr_number=7, target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.append(ApiResponse(404, {}, b"not found"))  # branch_exists: false

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("does not exist" in o for o in outcomes)
    assert not any(c["path"] == "/repos/o/r/pulls/7/reviews" for c in transport.calls)


def test_a_review_triggered_success_that_asked_a_question_also_posts_a_comment(
    monkeypatch, tmp_path,
):
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.REVIEW, branch="feature-x",
                 pr_number=7, target_owner="o", target_repo="r")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    question_file = tmp_path / "question.txt"
    question_file.write_text("should I flag style nits too?")
    monkeypatch.setattr(core_module, "question_path", lambda unit: str(question_file))
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),  # create_comment
        ApiResponse(200, {}, b"{}"),  # remove_label (in-progress off)
        ApiResponse(200, {}, b"{}"),  # add_label (awaiting-reply on)
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    comment_call = next(
        c for c in transport.calls if c["method"] == "POST" and c["path"].endswith("/comments")
    )
    assert "should I flag style nits too?" in json.loads(comment_call["body"])["body"]
    assert not any(c["path"] == "/repos/o/r/pulls/7/reviews" for c in transport.calls)


# --- session history wiring (docs/roadmap.md item 10) -----------------------

def test_orchestrator_defaults_to_a_null_session_history():
    orchestrator, _ = make_orchestrator(issues=[])
    assert isinstance(orchestrator.history, NullSessionHistory)


def test_a_swept_success_is_recorded_into_the_injected_history(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    # claude -p now runs on the controller and its transcript is a plain
    # local file -- point capture_trajectory's transcript_path lookup at a
    # real tmp_path file rather than scripting a `cat` response.
    transcript = tmp_path / f"{unit_name('sandbox-0')}.transcript.jsonl"
    transcript.write_text('{"type": "result", "result": "done"}\n')
    monkeypatch.setattr(capture_module, "transcript_path", lambda unit: str(transcript))
    history = RecordingSessionHistory()
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner,
                                                 history=history)
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert len(history.calls) == 1
    assert history.calls[0]["issue"] == 5
    assert history.calls[0]["outcome"] == "succeeded"
    assert history.calls[0]["transcript_text"] == '{"type": "result", "result": "done"}\n'


# --- task repo vs. target repo -------------------------------------------

def test_a_repo_directive_sends_the_work_to_that_repo_not_the_task_repo():
    """The whole point of the split: the issue is filed in the task repo,
    but the clone, the push and the PR all belong to the repo the task's
    own `/repo` line names.
    """
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(4, body="ship it\n/repo other/service")],
        allowed=("o/r", "other/service"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert (assignment.target_owner, assignment.target_repo) == ("other", "service")
    # The sandbox clones through the proxy, on the *target* repo's path.
    clone_calls = [argv for argv, _ in orchestrator.base_runner.calls
                   if argv[:2] == ["bash", "-c"]]
    assert any("/other/service.git" in c[2] for c in clone_calls)
    # Labels still move on the task repo, which is the only repo this
    # deployment ever writes to besides opening a PR.
    label_calls = [c for c in transport.calls
                   if "labels" in c["path"] and c["method"] in ("POST", "DELETE")]
    assert label_calls
    assert all(c["path"].startswith("/repos/o/r/issues/4/labels") for c in label_calls)


def test_the_prompt_names_both_repos_and_carries_no_directive_lines():
    orchestrator, transport = make_orchestrator(
        issues=[], allowed=("o/r", "other/service"),
    )
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="ship it\n/repo other/service")]).encode()),  # list_issues
        ApiResponse(200, {}, json.dumps([
            {"id": 1, "user": {"login": "maintainer"},
             "body": "confirming:\n/repo other/service", "author_association": "MEMBER"},
        ]).encode()),                                                          # list_comments
    ])

    orchestrator.run_once(NOW)

    prompt = next(
        stdin for argv, stdin in orchestrator.base_runner.calls
        if argv[:1] == ["sudo"] and any("prompt.md" in a for a in argv)
    )
    assert "o/r#4" in prompt          # where the task lives
    assert "other/service" in prompt  # where the code lives
    # Stripped from the body *and* from the conversation section -- a
    # maintainer's correction is a reply, so it reaches the prompt that way
    # if nothing strips it there too.
    assert "/repo other/service" not in prompt
    assert "ship it" in prompt
    assert "confirming:" in prompt


def test_the_pr_is_opened_in_the_target_repo_and_closes_the_task_issue():
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1),
                 target_owner="other", target_repo="service", base="trunk")
    runner = FakeRunner()
    runner.expect("systemctl show",
                   stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(
        issues=[], state=state, runner=runner, allowed=("o/r", "other/service"),
    )
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    branch_call = transport.calls[0]
    assert branch_call["path"] == "/repos/other/service/branches/grain%2Fissue-5"
    pr_call = next(c for c in transport.calls if c["method"] == "POST" and c["path"] == "/repos/other/service/pulls")
    assert pr_call["path"] == "/repos/other/service/pulls"
    sent = json.loads(pr_call["body"])
    # The base recorded at dispatch, not re-read and not a global default.
    assert sent["base"] == "trunk"
    # A cross-repo closing reference: `Closes #5` would name an issue in the
    # target repo, which is a different issue entirely (or nobody's). It's
    # kept for the link/mention even though (bwsalmon/agents#23) it never
    # auto-closes across repos.
    assert "Closes o/r#5" in sent["body"]
    # The task issue is *not* closed here any more (bwsalmon/agents#54) --
    # only recorded, in the *task* repo's `open_pull_requests`, against the
    # *target* repo's PR, so a later run can close it once that PR does.
    assert not any(c["method"] == "PATCH" for c in transport.calls)
    assert orchestrator.state.open_pull_requests == {
        "5": OpenPullRequest(issue=5, target_owner="other", target_repo="service", pr_number=42),
    }
    # The in-progress label comes off the *task* repo.
    delete_calls = [c for c in transport.calls if c["method"] == "DELETE"]
    assert delete_calls[0]["path"].startswith("/repos/o/r/issues/5/labels")


def test_a_task_naming_a_non_allow_listed_repo_is_parked_not_dispatched():
    """Fail closed, and say why: the allowlist is the operator's control
    over which repos this agent set can touch, and an issue body is not.
    """
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(4, body="/repo somewhere/else")],
        allowed=("o/r",),
    )
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/repo somewhere/else")]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                   # list_comments
        ApiResponse(201, {}, json.dumps({"id": 555}).encode()),        # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "somewhere/else" in json.loads(comment["body"])["body"]
    assert "allowlist" in json.loads(comment["body"])["body"]
    # Parked exactly like an unanswered question: trigger label off,
    # awaiting-reply on, and the comment id recorded as the reply baseline.
    assert orchestrator.state.pending_questions["4"].question_comment_id == 555
    added = [json.loads(c["body"])["labels"] for c in transport.calls
             if c["method"] == "POST" and c["path"].endswith("/labels")]
    assert added == [["grain-agent-awaiting-reply"]]


def test_parking_tolerates_a_404_when_the_issue_vanished_mid_cycle():
    """Same 404 tolerance `_finish_question` gets: an issue that vanished
    between the listing and `_park`'s own comment call isn't a reason to
    crash the cycle -- there's nothing left to park either way.
    """
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(4, body="/repo somewhere/else")],
        allowed=("o/r",),
    )
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/repo somewhere/else")]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                   # list_comments
        ApiResponse(404, {}, b'{"message": "Not Found"}'),             # the park comment 404s
    ])

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("could not park issue #4" in o for o in outcomes)


def test_parking_raises_on_a_non_404_error_posting_the_comment():
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(4, body="/repo somewhere/else")],
        allowed=("o/r",),
    )
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/repo somewhere/else")]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                   # list_comments
        ApiResponse(500, {}, b"internal error"),                       # the park comment fails
    ])

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_a_task_with_no_repo_directive_and_no_default_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    orchestrator.config = AutomationConfig(task_owner="o", task_repo="r")
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(4)]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(201, {}, json.dumps({"id": 7}).encode()),         # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "/repo owner/name" in json.loads(comment["body"])["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)


def test_parking_one_task_does_not_stop_the_next_one_dispatching():
    """A parked task consumes no sandbox and no rate-limit slot, so the
    rest of the cycle's queue is unaffected.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([
            issue_json(1, body="/repo nope/nope"),
            issue_json(2, body="/repo o/r"),
        ]).encode()),                                            # list_issues
        ApiResponse(200, {}, b"[]"),                             # #1 list_comments
        ApiResponse(201, {}, json.dumps({"id": 11}).encode()),   # #1 park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments["sandbox-0"].issue == 2
    assert "1" in orchestrator.state.pending_questions


def test_a_trusted_reply_can_carry_the_corrected_repo_directive():
    """The repair loop in one action: reply with the right `/repo` line and
    the next cycle dispatches it, no issue-body edit needed. Untrusted
    comments are excluded from directive reading for the same reason they
    can't promote a question -- that would be an unlabelled stranger
    choosing which repo the agent writes to.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r", "fix/ed"))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(4, body="no directive here")]).encode()),
        ApiResponse(200, {}, json.dumps([
            {"id": 1, "user": {"login": "stranger"}, "body": "/repo evil/repo",
             "author_association": "NONE"},
            {"id": 2, "user": {"login": "maintainer"},
             "body": "sorry, wrong repo:\n/repo fix/ed",
             "author_association": "MEMBER"},
        ]).encode()),                                            # list_comments
    ])
    orchestrator.config = AutomationConfig(task_owner="o", task_repo="r")

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert (assignment.target_owner, assignment.target_repo) == ("fix", "ed")


# --- dependencies (bwsalmon/agents#164) -------------------------------------

def test_a_task_with_an_open_dependency_is_skipped_not_dispatched():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(1, body="/depends 2")]).encode()),      # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main): true
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "open"}).encode()),       # get_issue(2): still open
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: blocked on #2" in outcomes
    # Not parked: nothing needs a human reply for this to resume, so the
    # trigger label stays put and no comment is posted -- unlike every
    # other "can't dispatch this" case, which does both.
    assert "1" not in orchestrator.state.pending_questions
    assert not any(c["method"] == "POST" and c["path"].endswith("/comments")
                    for c in transport.calls)
    # bwsalmon/agents#194: the block is visible on the issue itself too,
    # not just that audit line.
    label_call = next(c for c in transport.calls if c["method"] == "POST"
                       and c["path"] == "/repos/o/r/issues/1/labels")
    assert json.loads(label_call["body"]) == {"labels": ["grain-waiting-on-dependency"]}


def test_an_already_labelled_blocked_dependency_is_not_relabelled():
    """`GitHubClient.add_label` is a no-op against a label an issue already
    carries, so this is free either way -- but a fresh POST every cycle
    would still be one wasted call each time nothing changed, the same
    reasoning `_refresh_agent_labels` docstring makes for `agent_label`.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(
            1, body="/depends 2",
            labels=("grain-agent", "grain-waiting-on-dependency"),
        )]).encode()),                                            # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main): true
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "open"}).encode()),        # get_issue(2): still open
    ])

    orchestrator.run_once(NOW)

    assert not any(c["method"] == "POST" and c["path"] == "/repos/o/r/issues/1/labels"
                    for c in transport.calls)


def test_a_task_with_a_closed_dependency_dispatches_normally():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(1, body="/depends 2")]).encode()),      # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main): true
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "closed"}).encode()),     # get_issue(2): closed
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments["sandbox-0"].issue == 1


def test_only_the_still_open_dependencies_are_named_in_the_skip_outcome():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(1, body="/depends 2,3")]).encode()),    # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main): true
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "closed"}).encode()),     # get_issue(2): closed
        ApiResponse(200, {}, json.dumps(
            {**issue_json(3), "state": "open"}).encode()),       # get_issue(3): still open
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: blocked on #3" in outcomes


def test_a_missing_dependency_issue_is_treated_as_still_blocking():
    """`_is_issue_closed`'s existing 404 tolerance (bwsalmon/agents#82) is
    reused here rather than refusing the task outright: a `/depends` line
    naming a typo'd or deleted issue number blocks instead of silently
    dispatching anyway, and the recurring "skipped: blocked on #N" audit
    line is what surfaces the mistake to a human, the same way any other
    standing block would.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(1, body="/depends 999")]).encode()),    # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main): true
        ApiResponse(404, {}, b'{"message": "Not Found"}'),        # get_issue(999): gone
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: blocked on #999" in outcomes


def test_a_task_depending_on_itself_is_parked_not_endlessly_blocked():
    """Unlike a dependency on a different issue, this can never resolve on
    its own -- refused outright the same as any other unusable directive,
    rather than left to block forever with only the audit log to explain
    why.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/depends 4")]).encode()),      # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),     # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "depend on its own completion" in json.loads(comment["body"])["body"]


def test_a_blocked_dependency_does_not_stop_the_next_candidate_dispatching():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([
            issue_json(1, body="/depends 2"),
            issue_json(2, body="do the other thing"),
        ]).encode()),                                            # list_issues
        ApiResponse(200, {}, b"[]"),                              # #1 list_comments
        ApiResponse(200, {}, b"{}"),                              # #1 branch_exists(main): true
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "open"}).encode()),       # get_issue(2): still open
    ])

    orchestrator.run_once(NOW)

    # #1 stayed queued (blocked), #2 -- which carries no /depends of its
    # own -- took the only sandbox instead. A `continue`, not the `break`
    # "no free sandbox"/"rate limit" use: a block is specific to the one
    # issue that named it, not a reason to stop looking at the rest of the
    # queue.
    assert orchestrator.state.assignments["sandbox-0"].issue == 2
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: blocked on #2" in outcomes


def test_a_dependency_closing_lets_the_task_dispatch_on_a_later_cycle():
    """No `_park` happened while blocked (previous tests) -- so nothing
    needs a human reply for this to resume; the very next cycle just
    checks the dependency issue's state again and finds it closed. The
    label cycle 1 put on the issue (bwsalmon/agents#194) is expected to
    still be there in cycle 2's fetched issue, the same way `trigger_label`
    would be -- these fixtures don't model GitHub state persisting between
    `run_once` calls, so it's set by hand on the cycle-2 `issue_json` here.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(1, body="/depends 2")]).encode()),      # list_issues, cycle 1
        ApiResponse(200, {}, b"[]"),                              # list_comments, cycle 1
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main), cycle 1
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "open"}).encode()),       # get_issue(2): still open
    ])
    orchestrator.run_once(NOW)
    assert orchestrator.state.assignments == {}
    label_call = next(c for c in transport.calls if c["method"] == "POST"
                       and c["path"] == "/repos/o/r/issues/1/labels")
    assert json.loads(label_call["body"]) == {"labels": ["grain-waiting-on-dependency"]}

    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(
            1, body="/depends 2",
            labels=("grain-agent", "grain-waiting-on-dependency"),
        )]).encode()),                                            # list_issues, cycle 2
        ApiResponse(200, {}, b"[]"),                              # list_comments, cycle 2
        ApiResponse(200, {}, b"{}"),                              # branch_exists(main), cycle 2
        ApiResponse(200, {}, json.dumps(
            {**issue_json(2), "state": "closed"}).encode()),     # get_issue(2): now closed
    ])
    orchestrator.run_once(NOW + timedelta(minutes=5))

    assert orchestrator.state.assignments["sandbox-0"].issue == 1
    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/1/labels/grain-waiting-on-dependency"
        for c in transport.calls
    )


def test_a_nonexistent_target_repo_is_parked_rather_than_crashing_the_cycle():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r", "gone/away"))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/repo gone/away")]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                              # list_comments
        ApiResponse(201, {}, json.dumps({"id": 3}).encode()),     # the park comment
    ])
    # The routing transport answers `GET /repos/{owner}/{repo}` itself, so
    # 404 that one route specifically for this test.
    transport.repo_status = 404

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "404" in json.loads(comment["body"])["body"]


def test_a_non_404_error_reading_the_target_repo_raises_rather_than_parking():
    """Only a 404 is treated as "this repo doesn't exist/isn't visible,
    park the task" -- a genuine API failure reading the target repo's
    default branch must still surface, not be silently parked alongside it.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r", "gone/away"))
    transport.responses.append(
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="/repo gone/away")]).encode())  # list_issues
    )
    transport.repo_status = 500

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_a_base_directive_overrides_the_target_repos_default_branch():
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="/repo o/r\n/base release-2")],
    )

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments["sandbox-0"].base == "release-2"


def test_a_base_directive_also_builds_the_workspace_from_that_base():
    # bwsalmon/agents#6: a /base directive that differs from the target
    # repo's real default branch (OrchestratorTransport always answers
    # "main") must change what the sandbox workspace -- and therefore the
    # agent's new branch -- is actually built on top of, not just where
    # create_pull_request later opens the PR. Before the fix, the workspace
    # always reset to origin/HEAD (the real default branch) regardless of
    # `/base`, so the agent's branch (and the eventual PR diff) carried
    # every commit "main" had that "release-2" didn't.
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="/repo o/r\n/base release-2")],
    )

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert clone_calls
    script = clone_calls[0][2]
    assert "checkout -f -B release-2 origin/release-2" in script
    # Not the repo's real default branch -- the whole point of the bug.
    assert "checkout -f --detach origin/HEAD" not in script


def test_a_target_repo_with_no_such_base_branch_is_parked_not_retried_forever():
    """bwsalmon/agents#224: an empty target repo (no commits, no branches
    at all) still reports a `default_branch` name from GitHub's own repos
    API -- that field is the repo's *configured* default, not proof a
    branch by that name actually exists. Before this, `_resolve_target`
    trusted it unchecked, so dispatch reached `ensure_workspace`'s `git
    checkout` on the sandbox, which failed as a bare `CommandError` that
    `_dispatch` only logs to the audit trail and retries next cycle --
    forever, with nothing ever posted to the issue. This confirms that
    failure is now caught up front, the same "clear comment, not a crash
    three layers down" treatment the 404-target-repo case already gets.
    """
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(4)]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(404, {}, b"not found"),                # branch_exists(main): false
        ApiResponse(201, {}, json.dumps({"id": 3}).encode()),         # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    body = json.loads(comment["body"])["body"]
    assert "main" in body
    assert "doesn't exist" in body


def test_a_non_404_error_confirming_the_base_branch_raises_rather_than_parking():
    orchestrator, transport = make_orchestrator(issues=[], allowed=("o/r",))
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(4)]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(500, {}, b"boom"),                     # branch_exists(main): error
    ])

    with pytest.raises(GitHubError):
        orchestrator.run_once(NOW)


def test_the_session_history_records_which_repo_the_work_was_in(tmp_path, monkeypatch):
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1),
                 target_owner="other", target_repo="service", base="main")
    runner = FakeRunner()
    runner.expect("systemctl show",
                   stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    monkeypatch.setattr(capture_module, "transcript_path",
                         lambda unit: str(tmp_path / "missing.jsonl"))
    history = RecordingSessionHistory()
    orchestrator, transport = make_orchestrator(
        issues=[], state=state, runner=runner, history=history,
        allowed=("o/r", "other/service"),
    )
    transport.responses.extend(
        pr_flow_response(42) + [completion_priming_response(), open_pr_response(42)]
    )

    orchestrator.run_once(NOW)

    assert history.calls[0]["target"] == "other/service"


# --- Gemini API key (bwsalmon/agents#47, label not directive: #49) ---------

GEMINI_KEY_NAME = "projects/1/locations/global/keys/abc"


def gemini_runner(**overrides) -> FakeRunner:
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{GEMINI_KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="AIzaSecretValue\n")
    # bwsalmon/agents#131: the per-cycle reap lists the project's keys
    # every run_once, the same way the agent-key reap does. Empty, so
    # nothing is ever old enough to delete in these tests.
    runner.expect("gcloud services api-keys list", stdout="[]")
    for prefix, kwargs in overrides.items():
        runner.expect(prefix, **kwargs)
    return runner


GEMINI_LABELS = ("grain-agent", "grain-gemini-key")


def test_gemini_key_label_without_config_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, labels=GEMINI_LABELS)]).encode()),         # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),         # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "grain-gemini-key" in json.loads(comment["body"])["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)
    # Refused before anything was ever minted.
    assert not any("gcloud" in c for c in orchestrator.base_runner.commands)


def test_a_task_with_no_gemini_key_label_never_mints_a_key():
    """The per-cycle reap (bwsalmon/agents#131) does call gcloud every
    run_once now, so "no gcloud at all" is no longer the property -- what
    must not happen is a *mint* for a task that never asked for one."""
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4)], runner=runner,
        gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assert not any("api-keys create" in c for c in runner.commands)
    assert not any("get-key-string" in c for c in runner.commands)


def test_gemini_key_label_with_config_places_the_key_in_the_sandbox():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    key_call = next(
        (argv, stdin) for argv, stdin in runner.calls
        if argv[:1] == ["dd"] and argv[1] == f"of={GEMINI_KEY_PATH}"
    )
    assert key_call[1] == "AIzaSecretValue"


def test_gemini_key_label_tells_the_agent_where_the_key_is():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    prompt = next(
        stdin for argv, stdin in runner.calls
        if argv[:1] == ["sudo"] and any("prompt.md" in a for a in argv)
    )
    assert GEMINI_KEY_PATH in prompt
    assert "AIzaSecretValue" not in prompt


def test_gemini_key_is_recorded_on_the_assignment_for_later_revocation():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert assignment.gemini_key_name == GEMINI_KEY_NAME


def test_gemini_key_display_name_folds_in_the_sandbox_and_issue():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    create_call = next(c for c in runner.commands if "api-keys create" in c)
    assert "sandbox-0" in create_call
    assert "issue-4" in create_call


def test_gemini_key_mint_failure_does_not_crash_the_cycle():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", returncode=1, stderr="PERMISSION_DENIED")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatch failed" in o and "PERMISSION_DENIED" in o for o in outcomes)


def test_a_dispatch_failure_after_minting_a_gemini_key_revokes_it():
    """The key was minted but dispatch() never got far enough to record an
    Assignment for sweeper.py to later revoke it through -- must not leak.
    """
    runner = gemini_runner(**{"bash -c": {"returncode": 128, "stderr": "fatal: Authentication failed"}})
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    delete_call = next(c for c in runner.commands if "api-keys delete" in c)
    assert GEMINI_KEY_NAME in delete_call
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatch failed" in o for o in outcomes)


def test_a_dispatch_failure_whose_gemini_key_cleanup_also_fails_reports_both():
    """Best-effort cleanup of an orphaned key can itself fail (Google's API
    unreachable, say) -- that must not mask the original dispatch failure
    it was trying to clean up after, so both land in the one audit outcome.
    """
    runner = gemini_runner(**{
        "bash -c": {"returncode": 128, "stderr": "fatal: Authentication failed"},
        "gcloud services api-keys delete": {"returncode": 1, "stderr": "API unreachable"},
    })
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=GEMINI_LABELS)],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(
        "dispatch failed" in o and "also failed to revoke" in o and "API unreachable" in o
        for o in outcomes
    )


# --- grain-scratch-repo label (bwsalmon/agents#159) -------------------------

SCRATCH_LABELS = ("grain-agent", "grain-scratch-repo")


def test_scratch_repo_label_without_config_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, labels=SCRATCH_LABELS)]).encode()),         # list_issues
        ApiResponse(200, {}, b"[]"),                                   # list_comments
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),          # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "grain-scratch-repo" in json.loads(comment["body"])["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)


def test_scratch_repo_label_dispatches_into_the_sandboxs_own_repo():
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=SCRATCH_LABELS)],
        allowed=("o/r", "acme/grain-scratch-sandbox-0"),
        scratch_repo_config=ScratchRepoConfig(owner="acme"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert (assignment.target_owner, assignment.target_repo) == (
        "acme", "grain-scratch-sandbox-0",
    )


def test_scratch_repo_label_overrides_a_repo_directive():
    """The label wins outright: which scratch repo applies can't be known
    until a sandbox is picked, so it can't be reconciled with a `/repo`
    line written in advance the normal way."""
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="/repo o/elsewhere", labels=SCRATCH_LABELS)],
        allowed=("o/r", "o/elsewhere", "acme/grain-scratch-sandbox-0"),
        scratch_repo_config=ScratchRepoConfig(owner="acme"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert (assignment.target_owner, assignment.target_repo) == (
        "acme", "grain-scratch-sandbox-0",
    )


def test_a_task_with_no_scratch_repo_label_is_unaffected():
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4)],
        scratch_repo_config=ScratchRepoConfig(owner="acme"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert (assignment.target_owner, assignment.target_repo) == ("o", "r")


def test_resolve_target_names_the_repo_for_whichever_sandbox_was_assigned():
    orchestrator, _ = make_orchestrator(
        allowed=("acme/grain-scratch-sandbox-1",),
        scratch_repo_config=ScratchRepoConfig(owner="acme"),
    )
    issue = Issue(number=5, title="t", body="", html_url="https://github.com/o/r/issues/5",
                   labels=SCRATCH_LABELS, state="open")

    task = orchestrator._resolve_target(issue, [], sandbox="sandbox-1")

    assert (task.repo.owner, task.repo.name) == ("acme", "grain-scratch-sandbox-1")


# --- grain-github-<name> label (bwsalmon/agents#52) -------------------------

def test_a_grain_github_label_records_the_override_for_the_dispatched_sandbox():
    credential_store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, labels=("grain-agent", "grain-github-workflow"))],
        credentials=credentials_with("workflow"),
        credential_store=credential_store,
    )

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments  # actually dispatched, not parked
    overrides = json.loads(credential_store._path.read_text())
    assert overrides["sandbox-0"] == "workflow"


def test_a_task_with_no_grain_github_label_never_touches_the_credential_store():
    credential_store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4)],
        credentials=credentials_with("workflow"),
        credential_store=credential_store,
    )

    orchestrator.run_once(NOW)

    assert not credential_store._path.exists()


def test_a_grain_github_label_with_no_credentials_wired_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, labels=("grain-agent", "grain-github-workflow"))]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),         # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "grain-github-workflow" in json.loads(comment["body"])["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)


def test_a_grain_github_label_naming_an_unconfigured_key_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, labels=("grain-agent", "grain-github-nonexistent"))]).encode()),
        ApiResponse(200, {}, b"[]"),
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),
    ])
    orchestrator.credentials = credentials_with("workflow")  # "nonexistent" isn't here

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "grain-github-nonexistent" in json.loads(comment["body"])["body"]


def test_two_grain_github_labels_on_one_issue_is_parked_as_ambiguous():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, labels=("grain-agent", "grain-github-workflow", "grain-github-release"))]
        ).encode()),
        ApiResponse(200, {}, b"[]"),
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),
    ])
    orchestrator.credentials = credentials_with("workflow", "release")

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "ambiguous" in json.loads(comment["body"])["body"]


def test_a_dispatch_failure_with_a_grain_github_label_does_not_leak_into_the_next_task():
    """The override is written before the dispatch attempt, which can then
    fail partway with no Assignment ever recorded -- sweeper.py's _release
    never runs for this sandbox, so a later, unrelated, unlabelled task
    picking up the same now-free sandbox must not silently inherit the
    elevated credential. Same "must not leak" bar bwsalmon/agents#47's
    gemini-key dispatch-failure test above already holds this feature to,
    but for the override file rather than a minted key.
    """
    credential_store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    runner = FakeRunner()
    runner.expect("bash -c", returncode=128, stderr="fatal: Authentication failed")
    orchestrator, transport = make_orchestrator(
        issues=[issue_json(4, labels=("grain-agent", "grain-github-workflow"))],
        runner=runner,
        credentials=credentials_with("workflow"),
        credential_store=credential_store,
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    overrides = json.loads(credential_store._path.read_text())
    assert overrides["sandbox-0"] == "workflow"

    # The sandbox is free again -- a later cycle dispatches an unrelated,
    # unlabelled task to it. Fix the runner (the same one, since it's still
    # what "sandbox-0" resolves to) and change what the queue returns.
    del runner.responses["bash -c"]
    transport.default = ApiResponse(200, {}, json.dumps([issue_json(5)]).encode())

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    overrides = json.loads(credential_store._path.read_text())
    assert "sandbox-0" not in overrides


# --- Self-debug (bwsalmon/agents#62) ----------------------------------------

SELF_DEBUG_LABELS = ("grain-agent", "grain-self-debug")


def _mcp_config_args_from(runner) -> list[str]:
    mcp_config_dd = next(
        stdin for argv, stdin in runner.calls
        if argv[:2] == ["sudo", "dd"] and "mcp-config.json" in argv[2]
    )
    return json.loads(mcp_config_dd)["mcpServers"]["grain-sandbox"]["args"]


def _prompt_stdin_from(runner) -> str:
    return next(
        stdin for argv, stdin in runner.calls
        if argv[:1] == ["sudo"] and any("prompt.md" in a for a in argv)
    )


def test_a_task_with_no_self_debug_label_never_enables_the_tool():
    orchestrator, _ = make_orchestrator(issues=[issue_json(4)])

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    assert "--self-debug" not in _mcp_config_args_from(runner)
    assert "grain-self-debug" not in _prompt_stdin_from(runner)


def test_self_debug_label_enables_the_tool_and_tells_the_agent():
    orchestrator, _ = make_orchestrator(issues=[issue_json(4, labels=SELF_DEBUG_LABELS)])

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    assert "--self-debug" in _mcp_config_args_from(runner)
    prompt = _prompt_stdin_from(runner)
    assert "grain-self-debug" in prompt
    assert "read_grain_logs" in prompt


def test_self_debug_label_needs_no_deployment_config_unlike_gemini_key():
    """Unlike `grain-gemini-key` (which parks a task when
    `gemini_key_config` is unset), `grain-self-debug` never needs to --
    there is no equivalent config gating it, so a plain deployment with
    nothing extra configured still dispatches normally.
    """
    orchestrator, _ = make_orchestrator(issues=[issue_json(4, labels=SELF_DEBUG_LABELS)])

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert not any(o.startswith("parked") for o in outcomes)


# --- Self-repair (bwsalmon/agents#99) ---------------------------------------

SELF_REPAIR_LABELS = ("grain-agent", "grain-self-repair")


def test_a_task_with_no_self_repair_label_never_enables_the_tools():
    orchestrator, _ = make_orchestrator(issues=[issue_json(4)])

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    assert "--self-repair" not in _mcp_config_args_from(runner)
    assert "grain-self-repair" not in _prompt_stdin_from(runner)


def test_self_repair_label_enables_the_tools_and_tells_the_agent():
    orchestrator, _ = make_orchestrator(issues=[issue_json(4, labels=SELF_REPAIR_LABELS)])

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    assert "--self-repair" in _mcp_config_args_from(runner)
    prompt = _prompt_stdin_from(runner)
    assert "grain-self-repair" in prompt
    assert "restart_grain_service" in prompt
    assert "reboot_controller" in prompt


def test_self_repair_label_needs_no_deployment_config_unlike_gemini_key():
    """Same shape as self_debug_label's own version of this test -- there
    is no config gating grain-self-repair either, so a plain deployment
    still dispatches normally."""
    orchestrator, _ = make_orchestrator(issues=[issue_json(4, labels=SELF_REPAIR_LABELS)])

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert not any(o.startswith("parked") for o in outcomes)


def test_self_debug_and_self_repair_labels_are_independent():
    """Carrying grain-self-debug alone must not also turn on the
    self-repair roster, and vice versa -- the two are separate labels
    gating separate tool sets."""
    orchestrator, _ = make_orchestrator(issues=[issue_json(4, labels=SELF_DEBUG_LABELS)])

    orchestrator.run_once(NOW)

    runner = orchestrator.base_runner
    assert "--self-debug" in _mcp_config_args_from(runner)
    assert "--self-repair" not in _mcp_config_args_from(runner)


# --- surviving a controller VM restart (bwsalmon/agents#51) ---------------
#
# `claude -p` runs on the controller now (docs/roadmap.md item 8's
# "Update"), so a controller VM restart is exactly the "host is stopped, or
# a run dies mid-flight" case docs/design.md's stranded-work sweeper exists
# for. The sweeper itself already handles a unit that has vanished
# (`UnitState.ABSENT`, tested in test_automation_sweeper.py) -- what these
# tests are about is the other half: `AutomationState` must actually be on
# disk, at the right moment, for that recovery to have anything to work
# with. Before `Orchestrator.state_path`/`_save_state` existed, the only
# place `AutomationState` was ever written to disk was one `state.save()`
# call in `cli.py`'s `cmd_automation_run_once`, *after* `run_once()`
# returned in full -- a crash anywhere inside `run_once` (a restarted
# controller VM being the most literal version of "crash") lost every
# state mutation made so far, even ones whose real-world GitHub side effect
# (a label move, a PR) had already landed and could never be undone.

def test_dispatch_persists_the_assignment_before_the_trigger_label_comes_off(
    tmp_path, monkeypatch,
):
    """Removing the trigger label is the step that makes a dispatch
    irreversible from `_dispatch`'s own polling's point of view -- once
    it's off, `list_issues(trigger_label=...)` will never surface this
    issue again. So the new assignment must already be durably on disk
    *before* that call, or a crash landing right after it (a controller VM
    restarting being the realistic version of "crash") strands the task:
    not labelled for redispatch, and not recorded anywhere the sweeper
    could find it stranded either. Simulated here as a raise from
    `remove_label` itself -- standing in for the process dying mid-call --
    and checked by reading the state file back independently of the
    `Orchestrator` that (mid-crash) never got to return.
    """
    state_path = tmp_path / "state.json"
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], state_path=state_path)
    monkeypatch.setattr(
        orchestrator.github, "remove_label",
        lambda *a, **k: (_ for _ in ()).throw(RuntimeError("controller restarted here")),
    )

    with pytest.raises(RuntimeError):
        orchestrator.run_once(NOW)

    persisted = AutomationState.load(state_path)
    assert persisted.assignments["sandbox-0"].issue == 1


def test_a_task_stranded_by_a_controller_crash_mid_dispatch_is_recovered_on_restart(
    tmp_path, monkeypatch,
):
    """End-to-end version of the previous test. After the same simulated
    crash, a *fresh* `Orchestrator` -- reloading `AutomationState` from the
    same file, exactly what a restarted controller process does on its
    next `run_once` (`cli.py`'s `build_orchestrator`) -- must still find
    the dispatched issue via the assignment that survived, notice its unit
    is gone (nothing real was ever started against this `FakeRunner` on the
    restarted side either), requeue it, and redispatch it in that same
    cycle (sweep runs before dispatch). The alternative -- what happened
    before this fix -- is the issue simply vanishing: no longer trigger-
    labelled, and no assignment on disk to notice that either.
    """
    state_path = tmp_path / "state.json"
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], state_path=state_path)
    monkeypatch.setattr(
        orchestrator.github, "remove_label",
        lambda *a, **k: (_ for _ in ()).throw(RuntimeError("controller restarted here")),
    )
    with pytest.raises(RuntimeError):
        orchestrator.run_once(NOW)

    restarted_state = AutomationState.load(state_path)
    restarted, _ = make_orchestrator(
        issues=[issue_json(1)], state=restarted_state, state_path=state_path,
    )
    restarted.run_once(NOW + timedelta(minutes=5))

    outcomes = [e["outcome"] for e in restarted.audit.entries]
    assert "stranded" in outcomes
    assert restarted.state.assignments["sandbox-0"].issue == 1
    # And the recovery is itself durable -- not just in this process's
    # memory, in case *this* run_once is the one that gets interrupted too.
    assert AutomationState.load(state_path).assignments["sandbox-0"].issue == 1


def test_a_sweep_release_is_persisted_before_the_pr_is_opened(tmp_path, monkeypatch):
    """The mirror image of the dispatch-side test above: `sweep()` already
    releases a finished sandbox's slot in memory before `core.py` gets to
    act on the outcome, so that release must be durable *before*
    `create_pull_request` runs -- otherwise a crash right after a real PR
    is opened leaves the state file still claiming the sandbox is busy with
    a now-finished, already-reaped unit, which the next sweep would
    misread as freshly stranded and try to process all over again.
    """
    state_path = tmp_path / "state.json"
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit=unit_name("sandbox-0"), now=NOW)
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=exited\nResult=success\n",
    )
    orchestrator, transport = make_orchestrator(
        issues=[], state=state, runner=runner, state_path=state_path,
    )
    transport.responses.extend(pr_flow_response(1))
    monkeypatch.setattr(
        orchestrator.github, "create_pull_request",
        lambda *a, **k: (_ for _ in ()).throw(RuntimeError("controller restarted here")),
    )

    with pytest.raises(RuntimeError):
        orchestrator.run_once(NOW)

    persisted = AutomationState.load(state_path)
    assert persisted.assignments == {}


# --- GCP janitor (bwsalmon/agents#113) --------------------------------------

def test_janitor_is_a_noop_without_config():
    runner = FakeRunner()
    orchestrator, _ = make_orchestrator(issues=[], runner=runner)

    orchestrator.run_once(NOW)

    assert not any("gcloud" in c for c in runner.commands)


def test_janitor_deletes_an_old_instance_and_logs_it():
    runner = FakeRunner()
    runner.expect("gcloud compute instances list", stdout=json.dumps([{
        "name": "agent-thing",
        "zone": "https://www.googleapis.com/compute/v1/projects/proj/zones/us-central1-a",
        "creationTimestamp": "2020-01-01T00:00:00Z", "labels": {},
    }]))
    orchestrator, _ = make_orchestrator(
        issues=[], runner=runner,
        janitor_config=JanitorConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assert any("instances delete" in c and "agent-thing" in c for c in runner.commands)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("janitor deleted instance agent-thing" in o for o in outcomes)


def test_janitor_never_touches_the_grain_host_instance():
    runner = FakeRunner()
    runner.expect("gcloud compute instances list", stdout=json.dumps([{
        "name": "grain-host",
        "zone": "https://www.googleapis.com/compute/v1/projects/proj/zones/us-central1-a",
        "creationTimestamp": "2020-01-01T00:00:00Z", "labels": {},
    }]))
    orchestrator, _ = make_orchestrator(
        issues=[], runner=runner,
        janitor_config=JanitorConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assert not any("instances delete" in c for c in runner.commands)


def test_janitor_never_deletes_a_gemini_key_still_referenced_by_a_live_assignment():
    """Defensive extra check alongside the age cutoff -- task runtimes are
    always well under a sane TTL in practice, but a key `state.assignments`
    still names is never a candidate no matter what the listing says.
    Exercises `_janitor` directly (as `_ssh_runner_for` is elsewhere in this
    file) so this stays about the janitor's own wiring, not sweep's.
    """
    live_key = "projects/1/locations/global/keys/abc"
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit=unit_name("sandbox-0"), now=NOW,
                 gemini_key_name=live_key)
    runner = FakeRunner()
    runner.expect("gcloud services api-keys list", stdout=json.dumps([{
        "name": live_key, "displayName": "grain-sandbox-0-issue-1",
        "createTime": "2020-01-01T00:00:00Z",
    }]))
    orchestrator, _ = make_orchestrator(
        issues=[], runner=runner, state=state,
        janitor_config=JanitorConfig(project_id="proj"),
    )

    orchestrator._janitor(NOW)

    assert not any("api-keys delete" in c for c in runner.commands)


# --- scheduled jobs (bwsalmon/agents#163) -----------------------------

def scheduled_job(name="weekly-audit", interval_hours=168, needs_approval=False) -> ScheduledJob:
    return ScheduledJob(
        name=name, interval_hours=interval_hours,
        title_template="Weekly audit", body_template="Please audit things.",
        needs_approval=needs_approval,
    )


def test_scheduled_jobs_is_a_noop_without_config():
    orchestrator, transport = make_orchestrator(issues=[])

    orchestrator._scheduled_jobs(NOW)

    assert transport.calls == []


def test_scheduled_jobs_files_an_issue_the_first_time_it_is_due():
    orchestrator, transport = make_orchestrator(
        issues=[], scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(),)),
    )
    transport.responses.append(ApiResponse(200, {}, b"[]"))  # list_issues(marker_label)
    transport.responses.append(ApiResponse(201, {}, json.dumps(
        issue_json(99, labels=("grain-agent", "grain-scheduled-weekly-audit"))
    ).encode()))

    orchestrator._scheduled_jobs(NOW)

    create_call = transport.calls[-1]
    assert create_call["method"] == "POST"
    assert create_call["path"] == "/repos/o/r/issues"
    payload = json.loads(create_call["body"])
    assert payload["title"] == "Weekly audit"
    assert payload["body"] == "Please audit things."
    assert payload["labels"] == ["grain-agent", "grain-scheduled-weekly-audit"]
    assert orchestrator.state.scheduled_job_last_fired["weekly-audit"] == NOW


def test_scheduled_jobs_uses_needs_approval_label_when_the_job_opts_in():
    orchestrator, transport = make_orchestrator(
        issues=[],
        scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(needs_approval=True),)),
    )
    transport.responses.append(ApiResponse(200, {}, b"[]"))
    transport.responses.append(ApiResponse(201, {}, json.dumps(issue_json(99)).encode()))

    orchestrator._scheduled_jobs(NOW)

    payload = json.loads(transport.calls[-1]["body"])
    assert payload["labels"] == ["grain-agent-needs-approval", "grain-scheduled-weekly-audit"]


def test_scheduled_jobs_records_an_audit_entry_when_it_fires():
    orchestrator, transport = make_orchestrator(
        issues=[], scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(),)),
    )
    transport.responses.append(ApiResponse(200, {}, b"[]"))
    transport.responses.append(ApiResponse(201, {}, json.dumps(issue_json(99)).encode()))

    orchestrator._scheduled_jobs(NOW)

    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("weekly-audit" in o and "99" in o for o in outcomes)


def test_scheduled_jobs_does_not_refire_before_the_interval_elapses():
    state = AutomationState()
    state.record_scheduled_job_fired("weekly-audit", NOW - timedelta(hours=1))
    orchestrator, transport = make_orchestrator(
        issues=[], state=state,
        scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(interval_hours=168),)),
    )

    orchestrator._scheduled_jobs(NOW)

    assert transport.calls == []
    assert orchestrator.state.scheduled_job_last_fired["weekly-audit"] == NOW - timedelta(hours=1)


def test_scheduled_jobs_fires_again_once_the_interval_has_elapsed():
    state = AutomationState()
    state.record_scheduled_job_fired("weekly-audit", NOW - timedelta(hours=200))
    orchestrator, transport = make_orchestrator(
        issues=[], state=state,
        scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(interval_hours=168),)),
    )
    transport.responses.append(ApiResponse(200, {}, b"[]"))
    transport.responses.append(ApiResponse(201, {}, json.dumps(issue_json(99)).encode()))

    orchestrator._scheduled_jobs(NOW)

    assert orchestrator.state.scheduled_job_last_fired["weekly-audit"] == NOW


def test_scheduled_jobs_skips_firing_while_a_previous_issue_is_still_uncompleted():
    """bwsalmon/agents#163: the operator's own steer -- a job whose last
    issue hasn't finished must not get a duplicate just because
    `interval_hours` has since elapsed.
    """
    orchestrator, transport = make_orchestrator(
        issues=[], scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(),)),
    )
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        issue_json(50, labels=("grain-agent-in-progress", "grain-scheduled-weekly-audit")),
    ]).encode()))

    orchestrator._scheduled_jobs(NOW)

    assert not any(c["method"] == "POST" for c in transport.calls)
    assert "weekly-audit" not in orchestrator.state.scheduled_job_last_fired


def test_scheduled_jobs_fires_again_once_the_previous_issue_is_completed():
    orchestrator, transport = make_orchestrator(
        issues=[], scheduled_jobs_config=ScheduledJobsConfig(jobs=(scheduled_job(),)),
    )
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        issue_json(50, labels=("grain-agent-completed", "grain-scheduled-weekly-audit")),
    ]).encode()))
    transport.responses.append(ApiResponse(201, {}, json.dumps(issue_json(99)).encode()))

    orchestrator._scheduled_jobs(NOW)

    assert any(c["method"] == "POST" for c in transport.calls)
    assert orchestrator.state.scheduled_job_last_fired["weekly-audit"] == NOW


def test_scheduled_jobs_runs_before_dispatch_in_run_once(monkeypatch):
    """bwsalmon/agents#163: run last of the pre-dispatch phases so an
    issue it files is picked up the same cycle, not left idle for one
    extra `run_once` tick -- see `_scheduled_jobs`'s own docstring.
    """
    orchestrator, _ = make_orchestrator(issues=[])
    order = []
    monkeypatch.setattr(orchestrator, "_scheduled_jobs", lambda now: order.append("scheduled_jobs"))
    monkeypatch.setattr(orchestrator, "_dispatch", lambda now: order.append("dispatch"))

    orchestrator.run_once(NOW)

    assert order == ["scheduled_jobs", "dispatch"]


# --- grain's own comments never count as a human speaking ----------------

SIGNATURE = core_module._AUTOMATION_SIGNATURE


def signed(body: str) -> str:
    """A comment body in the exact shape every automation comment in
    `core.py` is built in -- the signature on a line of its own, then the
    message. Written through `_AUTOMATION_SIGNATURE` rather than a copy of
    its text, so a reworded marker can never leave these tests passing
    against a string production stopped using.
    """
    return f"{SIGNATURE}\n\n{body}"


def test_a_comment_grain_posted_itself_does_not_restart_a_completed_issue():
    """The live bug: `_suggest_fix` comments on a *completed* issue to
    announce the follow-up task it just filed for that issue's stale PR.
    grain posts as a maintainer's own credential, so GitHub reports that
    comment as "OWNER" -- inside `_TRUSTED_REPLY_ASSOCIATIONS` -- and
    `_restart_commented_completions` read it as a human follow-up and put
    `trigger_label` back on. The task reopened itself, with nobody having
    asked.
    """
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="grain-agent-bot", author_association="OWNER",
                          body=signed("o/r#7 has conflicts with `main` -- filed "
                                      "o/r#8 to fix it.")),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert "5" in orchestrator.state.completed_issues
    assert orchestrator.state.assignments == {}
    assert not any(c["method"] == "PATCH" for c in transport.calls)


def test_a_maintainers_quote_reply_still_restarts_a_completed_issue():
    """The false negative the filter has to avoid: GitHub's own "Quote
    reply" button copies the comment being replied to -- signature and all
    -- into the new comment's body as `>`-prefixed lines. That is a human
    replying, which is precisely what this feature exists to notice, so
    only an *unquoted* signature line means grain wrote it.
    """
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    orchestrator, transport = make_orchestrator(issues=[issue_json(5)], state=state)
    quoted = "\n".join(f"> {line}" for line in signed("filed o/r#8 to fix it.").splitlines())
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="maintainer", author_association="OWNER",
                          body=f"{quoted}\n\nDon't bother, handle it here instead."),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert orchestrator.state.completed_issues == {}
    assert orchestrator.state.assignments["sandbox-0"].issue == 5


def test_a_comment_grain_posted_itself_does_not_answer_its_own_question():
    """Same confusion, the other comment-triggered redispatch: an issue
    parked on `awaiting_reply_label` must not be picked back up because
    the automation said something on the thread afterwards.
    """
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    orchestrator, transport = make_orchestrator(issues=[], state=state)
    transport.responses.append(ApiResponse(200, {}, json.dumps([
        comment_json_for(101, user="grain-agent-bot", author_association="OWNER",
                          body=signed("I can't start this task yet: no /repo.")),
    ]).encode()))

    orchestrator.run_once(NOW)

    assert "5" in orchestrator.state.pending_questions
    assert orchestrator.state.assignments == {}


def test_a_directive_in_grains_own_comment_is_not_read_as_an_instruction():
    """A directive is an instruction *from* a human. grain quotes a task's
    own text back at it, so without this filter its own comment could
    redirect the task it is describing.

    `_resolve_target` is called directly rather than through `run_once`:
    the shared transport serves one `default` body to every call, so a
    comment thread scripted through it cannot be aimed at the dispatch's
    own `list_comments` read specifically -- and a test that only *looked*
    like it was exercising the filter would be worse than none.
    """
    orchestrator, _ = make_orchestrator()
    issue = Issue(number=5, title="t", body="do the thing",
                   html_url="https://github.com/o/r/issues/5", labels=[], state="open")
    grain_said = Comment(id=101, user="grain-agent-bot", author_association="OWNER",
                          body=signed("/repo o/elsewhere"))

    task = orchestrator._resolve_target(issue, [grain_said], sandbox="sandbox-0")

    assert (task.repo.owner, task.repo.name) == ("o", "r"), (
        "grain's own comment redirected the task it was describing"
    )


def test_a_directive_in_a_maintainers_comment_is_still_read():
    """The control for the test above: the filter must subtract only
    grain's own voice, not every trusted comment -- repairing a task with
    a reply is the whole point of reading comments for directives.
    """
    orchestrator, _ = make_orchestrator(allowed=("o/r", "o/elsewhere"))
    issue = Issue(number=5, title="t", body="do the thing",
                   html_url="https://github.com/o/r/issues/5", labels=[], state="open")
    human_said = Comment(id=101, user="maintainer", author_association="OWNER",
                          body="/repo o/elsewhere")

    task = orchestrator._resolve_target(issue, [human_said], sandbox="sandbox-0")

    assert (task.repo.owner, task.repo.name) == ("o", "elsewhere")


def test_the_marker_is_only_recognised_on_a_line_of_its_own():
    assert core_module._is_automation_comment(signed("filed a fix"))
    assert core_module._is_automation_comment(f"Closes o/r#5.\n\n{SIGNATURE}")
    assert not core_module._is_automation_comment(f"> {SIGNATURE}")
    assert not core_module._is_automation_comment(None)
    assert not core_module._is_automation_comment("")
    assert not core_module._is_automation_comment("please redo this")
    # Mentioned inside a sentence, not posted as one: still a human talking.
    assert not core_module._is_automation_comment(f"your footer ({SIGNATURE}) is wrong")


# --- a second run of a task whose PR is already open ---------------------

def _succeeded_run_hitting_an_existing_pr(*, lookup: ApiResponse):
    """A sweep with one succeeded issue outcome whose `create_pull_request`
    is answered with GitHub's real 422 for a head branch that already has
    an open PR. `lookup` answers the `find_open_pull_request_for_branch`
    call that 422 sends us to.
    """
    already_exists = json.dumps({
        "message": "Validation Failed",
        "errors": [{"resource": "PullRequest", "field": "base", "code": "invalid",
                     "message": "A pull request already exists for o:grain/issue-5."}],
    }).encode()
    state = AutomationState()
    state.assign("sandbox-0", issue=5, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1))
    runner = FakeRunner()
    runner.expect("systemctl show",
                   stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(branch_json(DEFAULT_COMMIT_MESSAGE)).encode()),
        ApiResponse(200, {}, json.dumps(issue_json(42)).encode()),
        ApiResponse(422, {}, already_exists),
        lookup,
    ])
    return orchestrator, transport


def test_a_second_run_of_a_task_reuses_the_pr_its_first_run_opened():
    """`branch_name` is a pure function of the issue number, so a second
    run of the same task -- a human re-applying `trigger_label` for another
    round, or `_restart_commented_completions` doing it for them -- pushes
    to the branch the first run already opened a PR from. GitHub allows one
    open PR per head branch and answers with a 422.

    That is not a failure: a PR tracks its branch, so the commits this run
    just pushed are already in the existing PR. Before this, the 422
    propagated -- the issue stayed stuck carrying `in_progress_label`, with
    its work sitting in a PR nobody was told about, and no `_requeue` to
    get it out again.
    """
    orchestrator, transport = _succeeded_run_hitting_an_existing_pr(
        lookup=ApiResponse(200, {}, json.dumps([
            {"number": 42, "html_url": "https://github.com/o/r/pull/42"},
        ]).encode()),
    )
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),  # add_label (completed)
        ApiResponse(200, {}, b"{}"),  # remove_label (in-progress)
        ApiResponse(200, {}, b"{}"),  # remove_label (agent label)
        completion_priming_response(),
        open_pr_response(42),
    ])

    orchestrator.run_once(NOW)  # must not raise

    # The finish completed exactly as if this run had opened the PR itself.
    assert "sandbox-0" not in orchestrator.state.assignments
    assert orchestrator.state.open_pull_requests["5"].pr_number == 42
    assert "5" in orchestrator.state.completed_issues
    completed_call = next(
        c for c in transport.calls
        if c["method"] == "POST" and c["path"] == "/repos/o/r/issues/5/labels"
    )
    assert json.loads(completed_call["body"]) == {"labels": ["grain-agent-completed"]}
    assert any(
        c["method"] == "DELETE"
        and c["path"] == "/repos/o/r/issues/5/labels/grain-agent-in-progress"
        for c in transport.calls
    )
    # ...but the audit says what actually happened, rather than claiming a
    # PR was opened that in fact already existed.
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("existing PR o/r#42" in o for o in outcomes)
    assert not any("opened PR" in o for o in outcomes)


def test_a_422_with_no_existing_pr_behind_it_still_raises():
    """422 is also GitHub's answer to "No commits between `main` and
    `grain/issue-5`" -- a real problem whose message is worth keeping. Only
    an open PR actually found for this head explains the 422 away.
    """
    orchestrator, _ = _succeeded_run_hitting_an_existing_pr(
        lookup=ApiResponse(200, {}, b"[]"),
    )

    with pytest.raises(GitHubError) as caught:
        orchestrator.run_once(NOW)

    # The original 422, not whatever the lookup said -- the useful message
    # is the one GitHub gave for the failed creation.
    assert caught.value.status == 422


def test_the_existing_pr_lookup_asks_about_this_tasks_own_branch():
    orchestrator, transport = _succeeded_run_hitting_an_existing_pr(
        lookup=ApiResponse(200, {}, json.dumps([
            {"number": 42, "html_url": "https://github.com/o/r/pull/42"},
        ]).encode()),
    )
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"), ApiResponse(200, {}, b"{}"),
        ApiResponse(200, {}, b"{}"),
        completion_priming_response(), open_pr_response(42),
    ])

    orchestrator.run_once(NOW)

    lookup_call = next(
        c for c in transport.calls
        if c["method"] == "GET" and c["path"].startswith("/repos/o/r/pulls?")
    )
    assert lookup_call["path"] == "/repos/o/r/pulls?state=open&head=o%3Agrain%2Fissue-5"
