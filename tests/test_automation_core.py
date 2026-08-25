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
from grain.automation.dispatch import CONTROLLER_AGENT_SSH_KEY_PATH, GEMINI_KEY_PATH, unit_name
from grain.automation.gemini_keys import GeminiKeyConfig
from grain.automation.github import ApiResponse, FakeTransport, GitHubClient, GitHubError
from grain.automation.history import NullSessionHistory, RecordingSessionHistory
from grain.automation.state import AutomationState, TriggerKind
from grain.inventory import Cluster
from grain.proxy.allowlist import Allowlist
from grain.proxy.tokens import SandboxTokenStore
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def issue_json(number: int, body: str = "do it") -> dict:
    return {
        # "id" isn't read by list_issues itself, but the same fixture also
        # serves as FakeTransport's shared default response for whichever
        # GET call falls through to it -- including _dispatch's new
        # list_comments call (docs/roadmap.md item 12), which does read
        # "id". Present so that fallback doesn't KeyError in tests that
        # never queue a dedicated comments response.
        "id": number, "number": number, "title": f"issue {number}", "body": body,
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
    """The five responses one `_finish_succeeded` call consumes, in exact
    call order: `branch_exists` (200 -> the branch is really there),
    `get_issue` (the title `create_pull_request`'s own title folds in),
    `create_pull_request` (201, with the fields `GitHubClient` reads back),
    `close_issue` (bwsalmon/agents#23 -- the task issue closed explicitly,
    since a cross-repo `Closes` reference never auto-closes), then
    `remove_label` (in-progress comes off). `FakeTransport.responses`
    is a strict FIFO queue regardless of which call consumes each entry, so
    a test with more than one succeeded outcome needs this whole handful
    per outcome, in order, or a later call silently eats an earlier
    outcome's response.
    """
    return [
        ApiResponse(200, {}, b"{}"),
        ApiResponse(200, {}, json.dumps(issue_json(pr_number)).encode()),
        ApiResponse(201, {}, json.dumps(
            {"number": pr_number, "html_url": f"https://github.com/o/r/pull/{pr_number}"}
        ).encode()),
        ApiResponse(200, {}, b"{}"),
        ApiResponse(200, {}, b"{}"),
    ]


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

    def request(self, *, method: str, path: str, headers: dict, body):
        if method == "GET" and re.fullmatch(r"/repos/[^/]+/[^/]+", path):
            self.calls.append(
                {"method": method, "path": path, "headers": dict(headers), "body": body}
            )
            if self.repo_status != 200:
                return ApiResponse(self.repo_status, {}, b"not found")
            return ApiResponse(200, {}, b'{"default_branch": "main"}')
        return super().request(method=method, path=path, headers=headers, body=body)


def allowlist_of(repos) -> Allowlist:
    """A real `Allowlist` over a real file — it re-reads on every check, so
    a test that needs to widen or narrow it can just rewrite the file.
    """
    path = Path(tempfile.mkdtemp()) / "repo-allowlist.json"
    path.write_text(json.dumps(list(repos)))
    return Allowlist(path)


def make_orchestrator(*, issues=(), state=None, runner=None, token_store=None,
                       history=None, allowed=("o/r",), gemini_key_config=None):
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
    orchestrator, _ = make_orchestrator(issues=[issue_json(1)], state=state, runner=runner)
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
    pr_call = next(c for c in transport.calls if c["method"] == "POST")
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
    assert "Posted automatically by grain-agent" in sent["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("opened PR o/r#42" in o for o in outcomes)
    # bwsalmon/agents#23: the task issue is closed explicitly, since a
    # cross-repo `Closes` reference in the PR body never auto-closes it.
    close_call = next(c for c in transport.calls if c["method"] == "PATCH")
    assert close_call["path"] == "/repos/o/r/issues/5"
    assert json.loads(close_call["body"]) == {"state": "closed"}
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
    assert any("opened PR o/r#42" in o for o in outcomes)


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
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(9, body="/repo o/r\n/pr 4")], state=state, runner=runner,
    )

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
        ApiResponse(200, {}, b"{}"),   # remove_label: in-progress off
    ])

    orchestrator.run_once(NOW)

    assert "sandbox-0" not in orchestrator.state.assignments
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("pushed additional commits to o/r" in o for o in outcomes)
    # No PR-creation call at all -- the PR this dispatch worked already existed.
    assert not any(c["path"] == "/repos/o/r/pulls" for c in transport.calls)
    mutating = [c for c in transport.calls if c["method"] in ("POST", "DELETE")]
    assert len(mutating) == 1  # only the in-progress label comes off
    # bwsalmon/agents#23's close_issue is issue-triggered only -- a
    # PR-triggered task is continuing an existing PR, which has its own
    # lifecycle, so this path must never close anything.
    assert not any(c["method"] == "PATCH" for c in transport.calls)


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
    transport.responses.extend(pr_flow_response(42))

    orchestrator.run_once(NOW)

    branch_call = transport.calls[0]
    assert branch_call["path"] == "/repos/other/service/branches/grain%2Fissue-5"
    pr_call = next(c for c in transport.calls if c["method"] == "POST")
    assert pr_call["path"] == "/repos/other/service/pulls"
    sent = json.loads(pr_call["body"])
    # The base recorded at dispatch, not re-read and not a global default.
    assert sent["base"] == "trunk"
    # A cross-repo closing reference: `Closes #5` would name an issue in the
    # target repo, which is a different issue entirely (or nobody's). It's
    # kept for the link/mention even though (bwsalmon/agents#23) it never
    # auto-closes across repos.
    assert "Closes o/r#5" in sent["body"]
    # The task issue is closed explicitly, in the *task* repo -- not the
    # target repo the PR itself was opened in.
    close_call = next(c for c in transport.calls if c["method"] == "PATCH")
    assert close_call["path"] == "/repos/o/r/issues/5"
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
    transport.responses.extend(pr_flow_response(42))

    orchestrator.run_once(NOW)

    assert history.calls[0]["target"] == "other/service"


# --- Gemini API key (bwsalmon/agents#47) ------------------------------------

GEMINI_KEY_NAME = "projects/1/locations/global/keys/abc"


def gemini_runner(**overrides) -> FakeRunner:
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{GEMINI_KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="AIzaSecretValue\n")
    for prefix, kwargs in overrides.items():
        runner.expect(prefix, **kwargs)
    return runner


def test_gemini_key_directive_without_config_is_parked():
    orchestrator, transport = make_orchestrator(issues=[])
    transport.responses.extend([
        ApiResponse(200, {}, json.dumps(
            [issue_json(4, body="do it\n/gemini-key")]).encode()),  # list_issues
        ApiResponse(200, {}, b"[]"),                                  # list_comments
        ApiResponse(201, {}, json.dumps({"id": 9}).encode()),         # the park comment
    ])

    orchestrator.run_once(NOW)

    assert orchestrator.state.assignments == {}
    comment = next(c for c in transport.calls if c["method"] == "POST"
                    and c["path"].endswith("/comments"))
    assert "gemini-key" in json.loads(comment["body"])["body"]
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any(o.startswith("parked, awaiting reply") for o in outcomes)
    # Refused before anything was ever minted.
    assert not any("gcloud" in c for c in orchestrator.base_runner.commands)


def test_a_task_with_no_gemini_key_directive_never_calls_gcloud():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4)], runner=runner,
        gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assert not any("gcloud" in c for c in runner.commands)


def test_gemini_key_directive_with_config_places_the_key_in_the_sandbox():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="do it\n/gemini-key")],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    key_call = next(
        (argv, stdin) for argv, stdin in runner.calls
        if argv[:1] == ["dd"] and argv[1] == f"of={GEMINI_KEY_PATH}"
    )
    assert key_call[1] == "AIzaSecretValue"


def test_gemini_key_directive_tells_the_agent_where_the_key_is():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="do it\n/gemini-key")],
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
        issues=[issue_json(4, body="do it\n/gemini-key")],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)

    assignment = orchestrator.state.assignments["sandbox-0"]
    assert assignment.gemini_key_name == GEMINI_KEY_NAME


def test_gemini_key_display_name_folds_in_the_sandbox_and_issue():
    runner = gemini_runner()
    orchestrator, _ = make_orchestrator(
        issues=[issue_json(4, body="do it\n/gemini-key")],
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
        issues=[issue_json(4, body="do it\n/gemini-key")],
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
        issues=[issue_json(4, body="do it\n/gemini-key")],
        runner=runner, gemini_key_config=GeminiKeyConfig(project_id="proj"),
    )

    orchestrator.run_once(NOW)  # must not raise

    assert orchestrator.state.assignments == {}
    delete_call = next(c for c in runner.commands if "api-keys delete" in c)
    assert GEMINI_KEY_NAME in delete_call
    outcomes = [e["outcome"] for e in orchestrator.audit.entries]
    assert any("dispatch failed" in o for o in outcomes)
