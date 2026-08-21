import json

import pytest

from grain.automation.github import (
    ApiResponse, DryRunGitHubClient, FakeTransport, GitHubClient, GitHubError,
    PullRequest,
)


def issue_json(number: int, *, is_pr: bool = False, labels=("grain-agent",)) -> dict:
    item = {
        "number": number,
        "title": f"issue {number}",
        "body": "do the thing",
        "html_url": f"https://github.com/o/r/issues/{number}",
        "labels": [{"name": l} for l in labels],
    }
    if is_pr:
        item["pull_request"] = {"url": "..."}
    return item


def test_list_issues_returns_open_issues():
    transport = FakeTransport(
        responses=[ApiResponse(200, {}, json.dumps([issue_json(1)]).encode())]
    )
    client = GitHubClient(transport, token="t")
    issues = client.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1]
    assert issues[0].labels == frozenset({"grain-agent"})


def test_list_issues_filters_out_pull_requests():
    transport = FakeTransport(
        responses=[ApiResponse(
            200, {}, json.dumps([issue_json(1), issue_json(2, is_pr=True)]).encode()
        )]
    )
    client = GitHubClient(transport, token="t")
    issues = client.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1]


def test_list_issues_follows_link_header_pagination():
    transport = FakeTransport(responses=[
        ApiResponse(
            200,
            {"Link": '<https://api.github.com/repos/o/r/issues?page=2>; rel="next"'},
            json.dumps([issue_json(1)]).encode(),
        ),
        ApiResponse(200, {}, json.dumps([issue_json(2)]).encode()),
    ])
    client = GitHubClient(transport, token="t")
    issues = client.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1, 2]
    assert transport.calls[1]["path"] == "/repos/o/r/issues?page=2"


def test_list_issues_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(403, {}, b"nope")])
    client = GitHubClient(transport, token="t")
    with pytest.raises(GitHubError):
        client.list_issues("o", "r", "grain-agent")


def test_add_label_posts_the_label_body():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"[]")])
    GitHubClient(transport, token="t").add_label("o", "r", 1, "grain-agent-in-progress")
    call = transport.calls[0]
    assert call["method"] == "POST"
    assert json.loads(call["body"]) == {"labels": ["grain-agent-in-progress"]}


def test_remove_label_tolerates_a_404_the_label_is_already_gone():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    # Should not raise.
    GitHubClient(transport, token="t").remove_label("o", "r", 1, "grain-agent")


def test_remove_label_raises_on_other_errors():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").remove_label("o", "r", 1, "grain-agent")


def test_anonymous_client_sends_no_authorization_header():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"[]")])
    GitHubClient(transport, token=None).list_issues("o", "r", "grain-agent")
    assert "Authorization" not in transport.calls[0]["headers"]


def test_branch_exists_true_on_200():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"{}")])
    assert GitHubClient(transport, token="t").branch_exists("o", "r", "grain/issue-1") is True


def test_branch_exists_false_on_404():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    assert GitHubClient(transport, token="t").branch_exists("o", "r", "grain/issue-1") is False


def test_branch_exists_percent_encodes_the_slash_in_the_branch_name():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"{}")])
    GitHubClient(transport, token="t").branch_exists("o", "r", "grain/issue-1")
    assert transport.calls[0]["path"] == "/repos/o/r/branches/grain%2Fissue-1"


def test_branch_exists_raises_on_other_errors():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").branch_exists("o", "r", "grain/issue-1")


def test_create_pull_request_posts_head_base_and_title():
    body = json.dumps({"number": 42, "html_url": "https://github.com/o/r/pull/42"}).encode()
    transport = FakeTransport(responses=[ApiResponse(201, {}, body)])
    pr = GitHubClient(transport, token="t").create_pull_request(
        "o", "r", head="grain/issue-1", base="main", title="grain: fix #1",
    )
    assert pr == PullRequest(number=42, html_url="https://github.com/o/r/pull/42")
    call = transport.calls[0]
    assert call["method"] == "POST"
    assert call["path"] == "/repos/o/r/pulls"
    sent = json.loads(call["body"])
    assert sent["head"] == "grain/issue-1"
    assert sent["base"] == "main"
    assert sent["title"] == "grain: fix #1"


def test_create_pull_request_raises_on_a_non_201():
    transport = FakeTransport(responses=[ApiResponse(422, {}, b"already exists")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").create_pull_request(
            "o", "r", head="grain/issue-1", base="main", title="x",
        )


def test_dry_run_client_passes_reads_through_but_prints_mutations(capsys):
    transport = FakeTransport(responses=[
        ApiResponse(200, {}, json.dumps([issue_json(1)]).encode()),
        ApiResponse(200, {}, b"{}"),
    ])
    real = GitHubClient(transport, token="t")
    dry = DryRunGitHubClient(real)

    issues = dry.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1]
    assert dry.branch_exists("o", "r", "grain/issue-1") is True

    dry.add_label("o", "r", 1, "grain-agent-in-progress")
    dry.remove_label("o", "r", 1, "grain-agent")
    pr = dry.create_pull_request("o", "r", head="grain/issue-1", base="main", title="x")
    out = capsys.readouterr().out
    assert "add label" in out
    assert "remove label" in out
    assert "open PR" in out
    assert isinstance(pr, PullRequest)
    # Only the two reads (list_issues, branch_exists) actually reached the
    # transport — every mutation, including PR creation, only printed.
    assert len(transport.calls) == 2
