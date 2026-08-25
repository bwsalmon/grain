import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from grain.automation.github import (
    ApiResponse, BranchHead, Comment, DryRunGitHubClient, FakeTransport, GitHubClient,
    GitHubError, PullRequest, PullRequestDetail, ReviewComment, RealTransport,
)


# --- RealTransport (the one piece of `github.py` that makes a real
# network call) -- exercised here against a real local HTTP server rather
# than mocked, since the whole point is to check that `http.client` is
# driven correctly (method/path/headers/body out, status/headers/body
# back). `use_tls=False` is a real, documented configuration -- the
# mocked-GitHub live-test seam (docs/roadmap.md item 8) -- so this is not
# testing a code path production never takes.

class _EchoHandler(BaseHTTPRequestHandler):
    def _handle(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        _EchoHandler.last_request = {
            "method": self.command, "path": self.path,
            "headers": dict(self.headers), "body": body,
        }
        self.send_response(200)
        self.send_header("X-Reply", "yes")
        self.send_header("Content-Length", "5")
        self.end_headers()
        self.wfile.write(b"hello")

    def do_GET(self):
        self._handle()

    def do_POST(self):
        self._handle()

    def log_message(self, format, *args):
        pass


def test_real_transport_sends_the_request_and_parses_the_response():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _EchoHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        port = server.server_address[1]
        transport = RealTransport(f"127.0.0.1:{port}", use_tls=False)
        resp = transport.request(
            method="POST", path="/repos/acme/widgets/issues",
            headers={"Accept": "application/vnd.github+json"}, body=b"payload",
        )
        assert resp.status == 200
        assert resp.body == b"hello"
        assert resp.headers["X-Reply"] == "yes"
    finally:
        server.shutdown()
        server.server_close()
    assert _EchoHandler.last_request["method"] == "POST"
    assert _EchoHandler.last_request["path"] == "/repos/acme/widgets/issues"
    assert _EchoHandler.last_request["body"] == b"payload"
    assert _EchoHandler.last_request["headers"]["Accept"] == "application/vnd.github+json"


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


def test_next_page_path_returns_none_when_the_link_header_has_no_next_rel():
    from grain.automation.github import _next_page_path

    header = '<https://api.github.com/repos/o/r/issues?page=1>; rel="prev"'
    assert _next_page_path(header) is None


def test_next_page_path_skips_earlier_segments_to_find_next():
    from grain.automation.github import _next_page_path

    header = (
        '<https://api.github.com/repos/o/r/issues?page=1>; rel="prev", '
        '<https://api.github.com/repos/o/r/issues?page=3>; rel="next"'
    )
    assert _next_page_path(header) == "/repos/o/r/issues?page=3"


def test_list_issues_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(403, {}, b"nope")])
    client = GitHubClient(transport, token="t")
    with pytest.raises(GitHubError):
        client.list_issues("o", "r", "grain-agent")


def test_get_issue_returns_the_issue():
    transport = FakeTransport(responses=[ApiResponse(200, {}, json.dumps(issue_json(7)).encode())])
    issue = GitHubClient(transport, token="t").get_issue("o", "r", 7)
    assert issue.number == 7
    assert issue.title == "issue 7"
    assert issue.labels == frozenset({"grain-agent"})
    assert transport.calls[0]["path"] == "/repos/o/r/issues/7"


def test_get_issue_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").get_issue("o", "r", 7)


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


def test_close_issue_patches_the_issue_closed():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"{}")])
    GitHubClient(transport, token="t").close_issue("o", "r", 1)
    call = transport.calls[0]
    assert call["method"] == "PATCH"
    assert call["path"] == "/repos/o/r/issues/1"
    assert json.loads(call["body"]) == {"state": "closed"}


def test_close_issue_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").close_issue("o", "r", 1)


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


def test_get_branch_head_returns_the_sha_and_message_on_200():
    body = json.dumps(
        {"commit": {"sha": "abc123", "commit": {"message": "Fix the thing\n\nDetails."}}}
    ).encode()
    transport = FakeTransport(responses=[ApiResponse(200, {}, body)])
    head = GitHubClient(transport, token="t").get_branch_head("o", "r", "grain/issue-1")
    assert head == BranchHead(sha="abc123", message="Fix the thing\n\nDetails.")


def test_get_branch_head_returns_none_on_404():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    assert GitHubClient(transport, token="t").get_branch_head("o", "r", "grain/issue-1") is None


def test_get_branch_head_raises_on_other_errors():
    transport = FakeTransport(responses=[ApiResponse(500, {}, b"boom")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").get_branch_head("o", "r", "grain/issue-1")


def test_dry_run_client_passes_get_branch_head_through():
    body = json.dumps(
        {"commit": {"sha": "abc123", "commit": {"message": "Fix the thing"}}}
    ).encode()
    transport = FakeTransport(responses=[ApiResponse(200, {}, body)])
    dry = DryRunGitHubClient(GitHubClient(transport, token="t"))
    assert dry.get_branch_head("o", "r", "grain/issue-1") == BranchHead(
        sha="abc123", message="Fix the thing"
    )


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
        state="open",
    )
    assert transport.calls[0]["path"] == "/repos/o/r/pulls/5"


def test_get_pull_request_reads_a_closed_state():
    """`state` (bwsalmon/agents#54) is what `core.py`'s `_close_finished_prs`
    polls to decide whether a task issue's PR is done -- "closed" covers
    both merged and closed-without-merging, both read this way by GitHub.
    """
    body = pr_json(5)
    body["state"] = "closed"
    transport = FakeTransport(responses=[ApiResponse(200, {}, json.dumps(body).encode())])
    pr = GitHubClient(transport, token="t").get_pull_request("o", "r", 5)
    assert pr.state == "closed"


def test_get_pull_request_defaults_state_to_open_when_absent():
    # A fixture (or, in principle, a very old cached response) missing the
    # "state" key entirely must not KeyError -- every real GitHub response
    # for this endpoint has always included it, but there's no reason to
    # make that a hard requirement when a safe default exists.
    transport = FakeTransport(responses=[ApiResponse(200, {}, json.dumps(pr_json(5)).encode())])
    pr = GitHubClient(transport, token="t").get_pull_request("o", "r", 5)
    assert pr.state == "open"


def test_get_pull_request_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").get_pull_request("o", "r", 5)


def test_default_branch_reads_the_repos_own_default():
    """What a PR opened in a target repo bases off, when the task's own
    `/base` directive doesn't say -- read from the repo rather than
    configured, since a task repo dispatches into many target repos and
    each one knows its own answer.
    """
    transport = FakeTransport(responses=[
        ApiResponse(200, {}, json.dumps({"default_branch": "trunk"}).encode()),
    ])
    assert GitHubClient(transport, token="t").default_branch("o", "r") == "trunk"
    assert transport.calls[0]["path"] == "/repos/o/r"


def test_default_branch_raises_on_a_non_200():
    transport = FakeTransport(responses=[ApiResponse(404, {}, b"not found")])
    with pytest.raises(GitHubError):
        GitHubClient(transport, token="t").default_branch("o", "r")


def test_a_token_source_resolves_a_credential_per_repo():
    """One client, many repos: the task repo and each target repo may need
    different credentials, so the token is resolved per call rather than
    fixed at construction.
    """
    class PerRepo:
        def token_for(self, owner: str, repo: str) -> str | None:
            return {"o/tasks": "task-token", "o/code": "code-token"}.get(f"{owner}/{repo}")

    transport = FakeTransport()
    client = GitHubClient(transport, PerRepo())
    client.list_issues("o", "tasks", "grain-agent")
    client.list_issues("o", "code", "grain-agent")
    client.list_issues("o", "unmapped", "grain-agent")
    assert transport.calls[0]["headers"]["Authorization"] == "token task-token"
    assert transport.calls[1]["headers"]["Authorization"] == "token code-token"
    # No credential covers the third -- an anonymous request, not a crash:
    # that is a real shape (a public repo), and fail-closed for a *private*
    # one is GitHub's own 404, not something to guess at here.
    assert "Authorization" not in transport.calls[2]["headers"]


def test_a_bare_token_still_applies_to_every_repo():
    transport = FakeTransport()
    client = GitHubClient(transport, token="t")
    client.list_issues("o", "one", "grain-agent")
    client.list_issues("other", "two", "grain-agent")
    assert all(c["headers"]["Authorization"] == "token t" for c in transport.calls)


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


def test_create_comment_posts_the_body_and_returns_the_new_id():
    transport = FakeTransport(responses=[ApiResponse(201, {}, json.dumps({"id": 999}).encode())])
    comment_id = GitHubClient(transport, token="t").create_comment("o", "r", 5, "a question for you")
    assert comment_id == 999
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
        ApiResponse(200, {}, json.dumps(pr_json(1)).encode()),
        ApiResponse(200, {}, json.dumps([review_comment_json(1)]).encode()),
        ApiResponse(200, {}, json.dumps({"default_branch": "main"}).encode()),
    ])
    dry = DryRunGitHubClient(GitHubClient(transport, token="t"))
    pr = dry.get_pull_request("o", "r", 1)
    assert isinstance(pr, PullRequestDetail)
    comments = dry.list_review_comments("o", "r", 1)
    assert [c.id for c in comments] == [1]
    assert dry.default_branch("o", "r") == "main"
    # All three calls were reads, so all three actually reached the transport
    # — nothing here is a mutation DryRunGitHubClient would intercept.
    assert len(transport.calls) == 3


def test_dry_run_client_passes_reads_through_but_prints_mutations(capsys):
    transport = FakeTransport(responses=[
        ApiResponse(200, {}, json.dumps([issue_json(1)]).encode()),
        ApiResponse(200, {}, json.dumps(issue_json(1)).encode()),
        ApiResponse(200, {}, b"{}"),
    ])
    real = GitHubClient(transport, token="t")
    dry = DryRunGitHubClient(real)

    issues = dry.list_issues("o", "r", "grain-agent")
    assert [i.number for i in issues] == [1]
    assert dry.get_issue("o", "r", 1).title == "issue 1"
    assert dry.branch_exists("o", "r", "grain/issue-1") is True

    dry.add_label("o", "r", 1, "grain-agent-in-progress")
    dry.remove_label("o", "r", 1, "grain-agent")
    dry.close_issue("o", "r", 1)
    pr = dry.create_pull_request("o", "r", head="grain/issue-1", base="main", title="x")
    out = capsys.readouterr().out
    assert "add label" in out
    assert "remove label" in out
    assert "close issue" in out
    assert "open PR" in out
    assert isinstance(pr, PullRequest)
    # Only the three reads (list_issues, get_issue, branch_exists) actually
    # reached the transport — every mutation, including PR creation and
    # closing the issue, only printed.
    assert len(transport.calls) == 3


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
