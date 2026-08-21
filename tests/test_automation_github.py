import json

import pytest

from grain.automation.github import (
    ApiResponse, DryRunGitHubClient, FakeTransport, GitHubClient, GitHubError,
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


def test_dry_run_client_passes_reads_through_but_prints_mutations(capsys):
    transport = FakeTransport(responses=[ApiResponse(200, {}, json.dumps([issue_json(1)]).encode())])
    real = GitHubClient(transport, token="t")
    dry = DryRunGitHubClient(real)

    issues = dry.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1]

    dry.add_label("o", "r", 1, "grain-agent-in-progress")
    dry.remove_label("o", "r", 1, "grain-agent")
    out = capsys.readouterr().out
    assert "add label" in out
    assert "remove label" in out
    # Only the read actually reached the transport.
    assert len(transport.calls) == 1
