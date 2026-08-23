import json
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

import grain.automation.capture as capture_module
import grain.automation.core as core_module
from grain.automation.audit import RecordingAuditLog
from grain.automation.config import AutomationConfig
from grain.automation.core import Orchestrator
from grain.automation.dispatch import CONTROLLER_AGENT_SSH_KEY_PATH, unit_name
from grain.automation.github import ApiResponse, FakeTransport, GitHubClient, GitHubError
from grain.automation.history import NullSessionHistory, RecordingSessionHistory
from grain.automation.state import AutomationState, TriggerKind
from grain.inventory import Cluster
from grain.proxy.tokens import SandboxTokenStore
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def issue_json(number: int) -> dict:
    return {
        # "id" isn't read by list_issues itself, but the same fixture also
        # serves as FakeTransport's shared default response for whichever
        # GET call falls through to it -- including _dispatch's new
        # list_comments call (docs/roadmap.md item 12), which does read
        # "id". Present so that fallback doesn't KeyError in tests that
        # never queue a dedicated comments response.
        "id": number, "number": number, "title": f"issue {number}", "body": "do it",
        "html_url": f"https://github.com/o/r/issues/{number}",
        "labels": [{"name": "grain-agent"}],
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


def pr_detail_json(number: int, head_ref: str = "feature-x", base_ref: str = "main") -> dict:
    return {
        "number": number, "title": f"pr {number}", "body": "please review",
        "html_url": f"https://github.com/o/r/pull/{number}",
        "head": {"ref": head_ref, "sha": "abc123", "label": f"o:{head_ref}"},
        "base": {"ref": base_ref, "label": f"o:{base_ref}"},
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


def make_orchestrator(*, issues=(), state=None, runner=None, token_store=None, history=None):
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
        audit=RecordingAuditLog(), history=history,
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


def test_a_pr_triggered_success_that_asked_a_question_also_posts_a_comment(monkeypatch, tmp_path):
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x")
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
    # docs/roadmap.md item 14: every PR grain-agent opens carries a
    # consistent, visible marker distinguishing it from a human-authored one.
    assert "🤖" in sent["title"]
    assert "Posted automatically by grain-agent" in sent["body"]
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


# --- PR-triggered dispatch (docs/roadmap.md item 9) ------------------------

def test_a_labelled_pr_is_dispatched_to_its_own_existing_branch():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([]).encode()),                       # list_issues
        ApiResponse(200, {}, json.dumps([pr_trigger_json(7)]).encode()),     # list_pull_requests: candidate
        ApiResponse(200, {}, json.dumps(pr_detail_json(7, head_ref="feature-x")).encode()),  # hydration
        ApiResponse(200, {}, b"[]"),                                          # list_review_comments
    ])

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert assignment.issue == 7
    assert assignment.kind is TriggerKind.PR
    assert assignment.branch == "feature-x"
    runner = orchestrator.base_runner
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert any("checkout -f -B feature-x origin/feature-x" in c[2] for c in clone_calls)
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "dispatched" in outcomes


def test_issues_and_prs_are_merged_and_sorted_by_number_together():
    # Two candidates for two sandboxes: PR #2 and issue #5. Since GitHub
    # gives issues and PRs one shared number sequence per repo, sorting the
    # merged candidate list by number alone is already "oldest trigger
    # first" across both kinds -- #2 gets dispatched (and thus a sandbox)
    # before #5.
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([issue_json(5)]).encode()),          # list_issues
        ApiResponse(200, {}, json.dumps([pr_trigger_json(2)]).encode()),     # list_pull_requests: candidate
        ApiResponse(200, {}, json.dumps(pr_detail_json(2)).encode()),        # hydration
        ApiResponse(200, {}, b"[]"),                                          # list_review_comments
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments["sandbox-0"].issue == 2
    assert orchestrator.state.assignments["sandbox-0"].kind is TriggerKind.PR
    assert orchestrator.state.assignments["sandbox-1"].issue == 5
    assert orchestrator.state.assignments["sandbox-1"].kind is TriggerKind.ISSUE


def test_a_pr_candidate_is_skipped_when_the_pool_is_full_same_as_an_issue():
    # The shared-budget decision: a PR-triggered candidate competes for the
    # same free-sandbox pool as an issue-triggered one, no separate budget.
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit="grain-task-sandbox-0", now=NOW)
    state.assign("sandbox-1", issue=2, unit="grain-task-sandbox-1", now=NOW)
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n",
    )
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps([]).encode()),                       # list_issues
        ApiResponse(200, {}, json.dumps([pr_trigger_json(9)]).encode()),     # list_pull_requests: candidate
        ApiResponse(200, {}, json.dumps(pr_detail_json(9)).encode()),        # hydration (candidates are
                                                                               # always hydrated up front,
                                                                               # same as list_issues already
                                                                               # fully reads every issue)
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" in orchestrator.state.assignments  # still occupied, untouched
    assert "sandbox-1" in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert "skipped: no free sandbox" in outcomes


def test_a_pr_triggered_success_pushes_commits_and_does_not_open_a_new_pr():
    state = AutomationState()
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x")
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    orchestrator, transport = make_orchestrator(issues=[], state=state, runner=runner)
    transport.responses.extend([
        ApiResponse(200, {}, b"{}"),   # branch_exists("feature-x"): true
        ApiResponse(200, {}, b"{}"),   # remove_label: in-progress off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("pushed additional commits to PR #7" in o for o in outcomes)
    # No PR-creation call at all -- the PR this dispatch worked already existed.
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 1  # only the in-progress label comes off


def test_a_pr_triggered_run_with_no_new_branch_is_requeued_not_dropped():
    state = AutomationState()
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0",
                 now=NOW - timedelta(hours=1), kind=TriggerKind.PR, branch="feature-x")
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
    assert len(mutating) == 2  # remove in-progress, add trigger back — same
                               # requeue path a failed/stranded run takes


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
    transport.responses.extend(pr_flow_response(42))

    orchestrator.run_once(NOW)

    assert len(history.calls) == 1
    assert history.calls[0]["issue"] == 5
    assert history.calls[0]["outcome"] == "succeeded"
    assert history.calls[0]["transcript_text"] == '{"type": "result", "result": "done"}\n'
