import json

import pytest

from grain.automation.github import (
    ApiResponse, Comment, DryRunGitHubClient, FakeTransport, GitHubClient, GitHubError,
    PullRequest, PullRequestDetail, ReviewComment,
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


# --- PR read path (docs/roadmap.md item 9) --------------------------------

def pr_json(number: int, *, head_ref: str = "feature-branch", base_ref: str = "main") -> dict:
    return {
        "number": number, "title": f"pr {number}", "body": "please review",
        "html_url": f"https://github.com/o/r/pull/{number}",
        "head": {"ref": head_ref, "sha": "abc123", "label": f"o:{head_ref}"},
        "base": {"ref": base_ref, "label": f"o:{base_ref}"},
    }


def test_get_pull_request_reads_head_and_base_ref():
    transport = FakeTransport(responses=[ApiResponse(200, {}, json.dumps(pr_json(5)).encode())])
    pr = GitHubClient(transport, token="t").get_pull_request("o", "r", 5)
    assert pr == PullRequestDetail(
        number=5, title="pr 5", body="please review",
        html_url="https://github.com/o/r/pull/5",
        head_ref="feature-branch", base_ref="main",
    )
    assert transport.calls[0]["path"] == "/repos/o/r/pulls/5"


def test_get_pull_request_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").get_pull_request("o", "r", 5)


def test_list_pull_requests_keeps_only_items_carrying_a_pull_request_key():
    # The opposite filter from list_issues: the issues endpoint returns both
    # issues and PRs, distinguished by a "pull_request" key on each item.
    transport = FakeTransport(responses=[
        ApiResponse(200, {}, json.dumps(
            [issue_json(1), issue_json(2, is_pr=True)]
        ).encode()),
        # The one PR hit gets hydrated with a get_pull_request call.
        ApiResponse(200, {}, json.dumps(pr_json(2)).encode()),
    ])
    prs = GitHubClient(transport, token="t").list_pull_requests("o", "r", "grain-agent")
    assert [p.number for p in prs] == [2]
    assert prs[0].head_ref == "feature-branch"
    # First call is the issues-style listing; second is the per-PR hydration.
    assert transport.calls[0]["path"] == "/repos/o/r/issues?labels=grain-agent&state=open&per_page=100"
    assert transport.calls[1]["path"] == "/repos/o/r/pulls/2"


def test_list_pull_requests_follows_link_header_pagination():
    transport = FakeTransport(responses=[
        ApiResponse(
            200,
            {"Link": '<https://api.github.com/repos/o/r/issues?page=2>; rel="next"'},
            json.dumps([issue_json(1, is_pr=True)]).encode(),
        ),
        ApiResponse(200, {}, json.dumps([issue_json(2, is_pr=True)]).encode()),
        ApiResponse(200, {}, json.dumps(pr_json(1)).encode()),
        ApiResponse(200, {}, json.dumps(pr_json(2)).encode()),
    ])
    prs = GitHubClient(transport, token="t").list_pull_requests("o", "r", "grain-agent")
    assert [p.number for p in prs] == [1, 2]


def test_list_pull_requests_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").list_pull_requests("o", "r", "grain-agent")


def review_comment_json(id_: int, *, line: int | None = 12) -> dict:
    return {
        "id": id_, "user": {"login": "reviewer"}, "body": "please fix this",
        "path": "src/thing.py", "line": line, "diff_hunk": "@@ -1,3 +1,3 @@",
    }


def test_list_review_comments_reads_the_review_comment_shape():
    transport = FakeTransport(
        responses=[ApiResponse(200, {}, json.dumps([review_comment_json(9)]).encode())]
    )
    comments = GitHubClient(transport, token="t").list_review_comments("o", "r", 5)
    assert comments == [ReviewComment(
        id=9, user="reviewer", body="please fix this", path="src/thing.py", line=12,
    )]
    assert transport.calls[0]["path"] == "/repos/o/r/pulls/5/comments?per_page=100"


def test_list_review_comments_tolerates_a_null_line_on_an_outdated_comment():
    transport = FakeTransport(
        responses=[ApiResponse(200, {}, json.dumps([review_comment_json(9, line=None)]).encode())]
    )
    comments = GitHubClient(transport, token="t").list_review_comments("o", "r", 5)
    assert comments[0].line is None


def test_list_review_comments_follows_link_header_pagination():
    transport = FakeTransport(responses=[
        ApiResponse(
            200,
            {"Link": '<https://api.github.com/repos/o/r/pulls/5/comments?page=2>; rel="next"'},
            json.dumps([review_comment_json(1)]).encode(),
        ),
        ApiResponse(200, {}, json.dumps([review_comment_json(2)]).encode()),
    ])
    comments = GitHubClient(transport, token="t").list_review_comments("o", "r", 5)
    assert [c.id for c in comments] == [1, 2]


def test_list_review_comments_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").list_review_comments("o", "r", 5)


def comment_json(id_: int, *, user: str = "human", body: str = "here's my answer") -> dict:
    return {"id": id_, "user": {"login": user}, "body": body}


def test_list_comments_reads_the_plain_comment_shape():
    transport = FakeTransport(
        responses=[ApiResponse(200, {}, json.dumps([comment_json(9)]).encode())]
    )
    comments = GitHubClient(transport, token="t").list_comments("o", "r", 5)
    assert comments == [Comment(id=9, user="human", body="here's my answer")]
    assert transport.calls[0]["path"] == "/repos/o/r/issues/5/comments?per_page=100"


def test_list_comments_follows_link_header_pagination():
    transport = FakeTransport(responses=[
        ApiResponse(
            200,
            {"Link": '<https://api.github.com/repos/o/r/issues/5/comments?page=2>; rel="next"'},
            json.dumps([comment_json(1)]).encode(),
        ),
        ApiResponse(200, {}, json.dumps([comment_json(2)]).encode()),
    ])
    comments = GitHubClient(transport, token="t").list_comments("o", "r", 5)
    assert [c.id for c in comments] == [1, 2]


def test_list_comments_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").list_comments("o", "r", 5)


def test_create_comment_posts_the_body():
    transport = FakeTransport(responses=[ApiResponse(201, {}, b"{}")])
    GitHubClient(transport, token="t").create_comment("o", "r", 5, "a question for you")
    call = transport.calls[0]
    assert call["method"] == "POST"
    assert call["path"] == "/repos/o/r/issues/5/comments"
    assert json.loads(call["body"]) == {"body": "a question for you"}


def test_create_comment_raises_on_a_non_201():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").create_comment("o", "r", 5, "a question")


def test_dry_run_client_passes_pr_reads_through(capsys):
    transport = FakeTransport(responses=[
        ApiResponse(200, {}, json.dumps([issue_json(1, is_pr=True)]).encode()),
        ApiResponse(200, {}, json.dumps(pr_json(1)).encode()),
        ApiResponse(200, {}, json.dumps(pr_json(1)).encode()),
        ApiResponse(200, {}, json.dumps([review_comment_json(1)]).encode()),
    ])
    dry = DryRunGitHubClient(GitHubClient(transport, token="t"))
    prs = dry.list_pull_requests("o", "r", "grain-agent")
    assert [p.number for p in prs] == [1]
    pr = dry.get_pull_request("o", "r", 1)
    assert isinstance(pr, PullRequestDetail)
    comments = dry.list_review_comments("o", "r", 1)
    assert [c.id for c in comments] == [1]
    # All four calls were reads, so all four actually reached the transport
    # — nothing here is a mutation DryRunGitHubClient would intercept.
    assert len(transport.calls) == 4


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


def test_dry_run_client_passes_list_comments_through_but_prints_create_comment(capsys):
    transport = FakeTransport(
        responses=[ApiResponse(200, {}, json.dumps([comment_json(1)]).encode())]
    )
    dry = DryRunGitHubClient(GitHubClient(transport, token="t"))

    comments = dry.list_comments("o", "r", 1)
    assert [c.id for c in comments] == [1]

    dry.create_comment("o", "r", 1, "a question for you")
    out = capsys.readouterr().out
    assert "comment on" in out
    assert "a question for you" in out
    # The read reached the transport; the mutation only printed.
    assert len(transport.calls) == 1
