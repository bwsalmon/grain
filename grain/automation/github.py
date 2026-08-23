"""The GitHub REST calls the orchestrator needs: list labelled issues, move
labels, confirm a branch exists, open a PR. Still no comment *creation* and
still no sandbox-side access — see docs/design.md's split surface
("Orchestrator: API operations... sandboxes: git transport only"). PR
creation is what docs/roadmap.md item 2 added; reading a PR's own data and
its review comments (never writing a comment) is what item 9 adds, to
support dispatching to an existing PR rather than only a labelled issue.

Same shape as `grain/proxy/forward.py`: a `Transport` protocol wrapping
`http.client` so `GitHubClient`'s logic is testable without a real call to
api.github.com, and a queue-based `FakeTransport` for tests that need more
than one scripted response (pagination).

Field shapes below (`head.ref`/`head.sha`, `base.ref`, and the review-comment
object's `id`/`user.login`/`body`/`path`/`line`) are pinned against GitHub's
own REST reference (`GET .../pulls/{number}`, `GET .../pulls/{number}/comments`
— both under `docs/rest/pulls/`), not guessed.
"""

from __future__ import annotations

import http.client
import json
from dataclasses import dataclass, field
from typing import Protocol
from urllib.parse import quote, urlsplit


@dataclass(frozen=True)
class ApiResponse:
    status: int
    headers: dict[str, str]
    body: bytes


class Transport(Protocol):
    def request(self, *, method: str, path: str, headers: dict[str, str],
                body: bytes | None) -> ApiResponse: ...


class RealTransport:
    """Talks to the GitHub API over HTTPS by default. `use_tls=False` exists
    for `AutomationConfig.github_host` pointed at a local mock server for a
    live end-to-end test (docs/roadmap.md item 8's mocked-GitHub run) — a
    mock has no clean answer for a self-signed cert, and this project
    already treats "point it at a mock" as a first-class test seam
    (`Transport` itself, `FakeTransport`) rather than something to fake with
    disabled certificate verification.
    """

    def __init__(self, host: str = "api.github.com", *, use_tls: bool = True) -> None:
        self.host = host
        self.use_tls = use_tls

    def request(self, *, method: str, path: str, headers: dict[str, str],
                body: bytes | None) -> ApiResponse:
        conn_cls = http.client.HTTPSConnection if self.use_tls else http.client.HTTPConnection
        conn = conn_cls(self.host, timeout=30)
        try:
            conn.request(method, path, body=body, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
            return ApiResponse(resp.status, dict(resp.getheaders()), data)
        finally:
            conn.close()


@dataclass
class FakeTransport:
    """Replays scripted responses in order. For tests, including pagination
    — call sites that need more than one page queue one response per page.
    """

    responses: list[ApiResponse] = field(default_factory=list)
    default: ApiResponse = field(
        default_factory=lambda: ApiResponse(200, {}, b"[]")
    )
    calls: list[dict] = field(default_factory=list)

    def request(self, *, method: str, path: str, headers: dict[str, str],
                body: bytes | None) -> ApiResponse:
        self.calls.append(
            {"method": method, "path": path, "headers": dict(headers), "body": body}
        )
        return self.responses.pop(0) if self.responses else self.default


class GitHubError(RuntimeError):
    def __init__(self, status: int, body: bytes) -> None:
        self.status = status
        self.body = body
        super().__init__(f"GitHub API error {status}: {body[:200]!r}")


@dataclass(frozen=True)
class Issue:
    number: int
    title: str
    body: str
    html_url: str
    labels: frozenset[str]


@dataclass(frozen=True)
class PullRequest:
    number: int
    html_url: str


@dataclass(frozen=True)
class PullRequestDetail:
    """Enough of a PR object to dispatch against it (docs/roadmap.md item 9)
    — title/body for the prompt, `head_ref` for the branch to check out and
    push back to. Deliberately a separate type from `PullRequest`
    (`create_pull_request`'s minimal return value): widening that one would
    ripple into every existing call site and test for no benefit, since
    nothing there needs the extra fields.
    """
    number: int
    title: str
    body: str
    html_url: str
    head_ref: str
    base_ref: str


@dataclass(frozen=True)
class ReviewComment:
    """One inline (diff-attached) review comment — GitHub's own term for
    what `GET .../pulls/{number}/comments` returns, distinct from a plain
    top-level issue/PR comment. `line` is `None` for a comment GitHub
    considers outdated (the API returns `original_line` instead in that
    case) — left as `None` here rather than falling back to
    `original_line`, since a prompt telling the agent "line N" about a line
    the diff has since moved past would be actively misleading.
    """
    id: int
    user: str
    body: str
    path: str
    line: int | None


def _next_page_path(link_header: str | None) -> str | None:
    """The path+query of the `rel="next"` link, or None on the last page.

    GitHub paginates via a full `Link` header URL rather than an opaque
    cursor; `Transport.request` takes a path against a fixed host, so only
    the path and query survive the hop.
    """
    if not link_header:
        return None
    for part in link_header.split(","):
        segment = part.strip()
        if 'rel="next"' not in segment:
            continue
        url = segment[segment.index("<") + 1 : segment.index(">")]
        split = urlsplit(url)
        return f"{split.path}?{split.query}" if split.query else split.path
    return None


class GitHubClient:
    def __init__(self, transport: Transport, token: str | None) -> None:
        self.transport = transport
        self.token = token

    def _headers(self, *, json_body: bool = False) -> dict[str, str]:
        headers = {
            "Accept": "application/vnd.github+json",
            "User-Agent": "grain-automation",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        if self.token:
            headers["Authorization"] = f"token {self.token}"
        if json_body:
            headers["Content-Type"] = "application/json"
        return headers

    def list_issues(self, owner: str, repo: str, label: str) -> list[Issue]:
        """Open issues carrying `label`. Filters out pull requests — the
        issues endpoint returns both, distinguished only by the presence of
        a `pull_request` key on the item.
        """
        issues: list[Issue] = []
        path = (
            f"/repos/{owner}/{repo}/issues"
            f"?labels={quote(label)}&state=open&per_page=100"
        )
        while path:
            resp = self.transport.request(
                method="GET", path=path, headers=self._headers(), body=None
            )
            if resp.status != 200:
                raise GitHubError(resp.status, resp.body)
            for item in json.loads(resp.body):
                if "pull_request" in item:
                    continue
                issues.append(Issue(
                    number=item["number"],
                    title=item["title"],
                    body=item.get("body") or "",
                    html_url=item["html_url"],
                    labels=frozenset(l["name"] for l in item["labels"]),
                ))
            path = _next_page_path(resp.headers.get("Link"))
        return issues

    def add_label(self, owner: str, repo: str, number: int, label: str) -> None:
        resp = self.transport.request(
            method="POST",
            path=f"/repos/{owner}/{repo}/issues/{number}/labels",
            headers=self._headers(json_body=True),
            body=json.dumps({"labels": [label]}).encode(),
        )
        if resp.status not in (200, 201):
            raise GitHubError(resp.status, resp.body)

    def remove_label(self, owner: str, repo: str, number: int, label: str) -> None:
        resp = self.transport.request(
            method="DELETE",
            path=f"/repos/{owner}/{repo}/issues/{number}/labels/{quote(label)}",
            headers=self._headers(), body=None,
        )
        # 404 means the label is already off the issue — a fine outcome,
        # not an error, since the caller's intent ("this label should not
        # be on there") is already satisfied.
        if resp.status not in (200, 404):
            raise GitHubError(resp.status, resp.body)

    def branch_exists(self, owner: str, repo: str, branch: str) -> bool:
        """Whether `branch` is really on the remote.

        The design decision behind why this exists at all (docs/roadmap.md
        item 2, docs/design.md's split surface): `dispatch()` tells the
        agent exactly what branch to push to, but the prompt it received
        came from untrusted issue content, so the controller confirms the
        branch is real via the API before opening a PR against it rather
        than trusting the agent's own report of what it did.
        """
        resp = self.transport.request(
            method="GET",
            # GitHub's branch-get endpoint requires `/` within the branch
            # name itself percent-encoded (`%2F`), not left as a path
            # separator — quote(..., safe="") is what does that.
            path=f"/repos/{owner}/{repo}/branches/{quote(branch, safe='')}",
            headers=self._headers(), body=None,
        )
        if resp.status == 200:
            return True
        if resp.status == 404:
            return False
        raise GitHubError(resp.status, resp.body)

    def create_pull_request(self, owner: str, repo: str, *, head: str, base: str,
                             title: str, body: str = "") -> PullRequest:
        resp = self.transport.request(
            method="POST",
            path=f"/repos/{owner}/{repo}/pulls",
            headers=self._headers(json_body=True),
            body=json.dumps(
                {"title": title, "head": head, "base": base, "body": body}
            ).encode(),
        )
        if resp.status != 201:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return PullRequest(number=data["number"], html_url=data["html_url"])

    def get_pull_request(self, owner: str, repo: str, number: int) -> PullRequestDetail:
        resp = self.transport.request(
            method="GET", path=f"/repos/{owner}/{repo}/pulls/{number}",
            headers=self._headers(), body=None,
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return PullRequestDetail(
            number=data["number"], title=data["title"], body=data.get("body") or "",
            html_url=data["html_url"],
            head_ref=data["head"]["ref"], base_ref=data["base"]["ref"],
        )

    def list_pull_requests(self, owner: str, repo: str, label: str) -> list[PullRequestDetail]:
        """Open PRs carrying `label` — the PR-trigger equivalent of
        `list_issues`, for docs/roadmap.md item 9.

        The pulls-list endpoint (`GET .../pulls`) takes no `labels` filter of
        its own — `state`, `head`, `base`, `sort`, `direction` only, per
        GitHub's own reference. Labels are exposed through the *issues* API,
        and a PR is a special kind of issue in GitHub's data model (same
        number sequence, same labels endpoint), so this walks the identical
        `/issues?labels=...` listing `list_issues` already uses, keeps only
        the items carrying a `pull_request` key (the opposite filter from
        `list_issues`), and hydrates each surviving number into a full
        `PullRequestDetail` with one `get_pull_request` call — the issues
        listing has title/body/html_url but never `head`/`base`, so a real PR
        read is still needed to learn the branch to dispatch against.
        """
        numbers: list[int] = []
        path = (
            f"/repos/{owner}/{repo}/issues"
            f"?labels={quote(label)}&state=open&per_page=100"
        )
        while path:
            resp = self.transport.request(
                method="GET", path=path, headers=self._headers(), body=None
            )
            if resp.status != 200:
                raise GitHubError(resp.status, resp.body)
            for item in json.loads(resp.body):
                if "pull_request" in item:
                    numbers.append(item["number"])
            path = _next_page_path(resp.headers.get("Link"))
        return [self.get_pull_request(owner, repo, n) for n in numbers]

    def list_review_comments(self, owner: str, repo: str, number: int) -> list[ReviewComment]:
        """Inline review comments on a PR — the context a dispatch needs to
        tell the agent what feedback it's addressing (docs/roadmap.md item
        9). Paginates the same way `list_issues` does; GitHub's REST API
        paginates every list endpoint via the same `Link` header convention,
        not something specific to the issues endpoint.
        """
        comments: list[ReviewComment] = []
        path = f"/repos/{owner}/{repo}/pulls/{number}/comments?per_page=100"
        while path:
            resp = self.transport.request(
                method="GET", path=path, headers=self._headers(), body=None
            )
            if resp.status != 200:
                raise GitHubError(resp.status, resp.body)
            for item in json.loads(resp.body):
                comments.append(ReviewComment(
                    id=item["id"], user=item.get("user", {}).get("login", ""),
                    body=item.get("body") or "", path=item.get("path", ""),
                    line=item.get("line"),
                ))
            path = _next_page_path(resp.headers.get("Link"))
        return comments


@dataclass
class DryRunGitHubClient:
    """Wraps a real client: reads pass through, label mutations print
    instead of firing. Same split `run.py`'s `DryRunRunner` makes for local
    commands — "read-only commands still execute."
    """

    inner: GitHubClient

    def list_issues(self, owner: str, repo: str, label: str) -> list[Issue]:
        return self.inner.list_issues(owner, repo, label)

    def add_label(self, owner: str, repo: str, number: int, label: str) -> None:
        print(f"+ add label {label!r} to {owner}/{repo}#{number}")

    def remove_label(self, owner: str, repo: str, number: int, label: str) -> None:
        print(f"+ remove label {label!r} from {owner}/{repo}#{number}")

    def branch_exists(self, owner: str, repo: str, branch: str) -> bool:
        return self.inner.branch_exists(owner, repo, branch)

    def create_pull_request(self, owner: str, repo: str, *, head: str, base: str,
                             title: str, body: str = "") -> PullRequest:
        print(f"+ open PR {owner}/{repo}: {head!r} -> {base!r} ({title!r})")
        return PullRequest(number=0, html_url=f"(dry run) {owner}/{repo}: {head} -> {base}")

    def get_pull_request(self, owner: str, repo: str, number: int) -> PullRequestDetail:
        return self.inner.get_pull_request(owner, repo, number)

    def list_pull_requests(self, owner: str, repo: str, label: str) -> list[PullRequestDetail]:
        return self.inner.list_pull_requests(owner, repo, label)

    def list_review_comments(self, owner: str, repo: str, number: int) -> list[ReviewComment]:
        return self.inner.list_review_comments(owner, repo, number)
