"""The GitHub REST calls the orchestrator needs: list labelled issues, move
labels, confirm a branch exists, open a PR. Still no sandbox-side access —
see docs/design.md's split surface ("Orchestrator: API operations...
sandboxes: git transport only"). PR creation is what docs/roadmap.md item 2
added; reading a PR's own data and its review comments (never writing a
comment) is what item 9 adds, to support dispatching to an existing PR
rather than only a labelled issue. That PR path is now reached through a
`/pr` directive on a task issue (`directives.py`) rather than by polling a
second repo for labelled PRs, so `get_pull_request` stayed and the
label-listing counterpart it once had is gone.

Comment *creation* (`create_comment`) arrives with docs/roadmap.md item 12,
for exactly one purpose: relaying a dispatched agent's `ask_question` MCP
call to a human. It is not a general capability the agent can reach
directly — the split surface above is unchanged, since `core.py` remains
the only caller and the agent never gets API access of its own; it only
ever writes a question to a local file `core.py` later reads (docs/roadmap.md
item 12, `mcp_server.py`'s `ask_question` tool). `list_comments` (the
top-level conversation thread, distinct from `list_review_comments`'s
inline diff comments) exists for the other half of that same item: showing
a redispatched issue/PR the human's reply to a prior question.

`create_review` (bwsalmon/agents#154) is the identical "the agent only ever
writes to a local file, `core.py` is the one that calls this" shape, for a
`/review`-directed dispatch's own `add_review_comment` MCP tool
(`mcp_server.py`): a review, with whatever inline comments the agent
attached, posted once the run finishes rather than one API call per
comment. Deliberately left as a **draft**: the request body carries no
`event` key, which is what keeps GitHub from submitting it (GitHub's own
"Create a review for a pull request" reference — omitting `event` leaves
the review `PENDING`, visible only to the credential that created it,
until a human opens it on github.com and submits it themselves). An agent
posting a *submitted* review of its own code would be marking its own
homework; a draft is a human's opinion of what an agent found, waiting on
that same human's sign-off before anyone else sees it.

**One client, many repos.** Every method already took `owner, repo` as
its first two arguments, but the token was fixed at construction — fine
while a deployment had exactly one repo, wrong now that the task repo (API:
issues, labels, comments) and each target repo (API: branches, PRs) are
different repos that may well need different credentials. `GitHubClient`
now resolves its token *per call* through a `TokenSource`, which is what
`grain/proxy/credentials.py`'s `CredentialSet` — narrowest-pattern-first,
`owner/repo` then `owner/*` then `*` — was always shaped for and never
wired to. A plain `str | None` still works and simply applies everywhere.

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


class TokenSource(Protocol):
    """Resolves the API token to use for one repo. `CredentialSet`
    (`grain/proxy/credentials.py`) satisfies this structurally — it is the
    real implementation in production; `StaticToken` covers "one token for
    everything," which is what a test and a single-credential deployment
    both want.
    """

    def token_for(self, owner: str, repo: str) -> str | None: ...


@dataclass(frozen=True)
class StaticToken:
    """One token regardless of repo — what `GitHubClient` wraps a bare
    `str | None` in, so the per-repo path below is the only path.
    """

    token: str | None

    def token_for(self, owner: str, repo: str) -> str | None:
        return self.token


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
    # GitHub's own field, "open" or "closed" (bwsalmon/agents#82) -- read by
    # `core.py`'s cancel-on-close poll to tell a task issue a human closed
    # early from one still open. Defaulted to "open" for the same
    # backward-compatible reason `PullRequestDetail.state` is: existing
    # fixtures and call sites that predate this field never set it, and
    # `list_issues`'s own query already filters to `state=open` anyway, so
    # "open" is what every issue it returns actually is.
    state: str = "open"


@dataclass(frozen=True)
class PullRequest:
    number: int
    html_url: str


@dataclass(frozen=True)
class BranchHead:
    """The tip of a branch: its sha and that commit's own message.
    `get_branch_head` returns this instead of a bare bool (bwsalmon/agents#79)
    so `core.py` can seed a freshly opened PR's body with the agent's own
    account of its change -- the commit message it already wrote to explain
    the diff -- rather than the generic, content-free line the body used to
    be built from alone.
    """
    sha: str
    message: str


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
    # GitHub's own field, "open" or "closed" -- "closed" covers both merged
    # and closed-without-merging, which `core.py`'s `_close_finished_prs`
    # (bwsalmon/agents#54) treats the same way: either one means nobody is
    # going to push more commits to this PR, so the task issue it was opened
    # for is done. Defaulted to "open" rather than required, since most
    # existing callers (dispatching to continue a PR, mid-flight by
    # definition) never cared before this field existed and a stale fixture
    # omitting it shouldn't suddenly need updating.
    state: str = "open"
    # GitHub's own field: `true`/`false` once it has finished computing
    # whether this PR can merge cleanly against its base, `null` (read here
    # as `None`) while that computation is still in flight -- GitHub does
    # this asynchronously, so a request right after a push can legitimately
    # see `None` for a cycle or two. `core.py`'s `_close_finished_prs`
    # (bwsalmon/agents#83) reads `False` as "has a conflict, suggest a fix"
    # and treats `None` the same as "don't know yet, check again next
    # cycle" -- never as either a conflict or a clean merge, since guessing
    # either way could file a needless fix or auto-merge something broken.
    # Defaulted to `None` for the same "existing callers never cared"
    # reason `state` is.
    mergeable: bool | None = None


@dataclass(frozen=True)
class Comment:
    """A plain top-level comment on an issue or PR — GitHub's own
    `/issues/{number}/comments` endpoint serves both, since a PR is a
    special kind of issue in its data model (same unification
    `list_pull_requests` already relies on). Distinct from `ReviewComment`:
    those are inline, diff-attached; this is the ordinary conversation
    thread — where a human's reply to an agent's `ask_question` call
    (docs/roadmap.md item 12) actually lands.

    `author_association` (GitHub's own field: "OWNER", "MEMBER",
    "COLLABORATOR", "CONTRIBUTOR", "NONE", ...) is what lets `core.py` tell
    a trusted reply from an arbitrary public comment when deciding whether
    to auto-redispatch after a question (docs/roadmap.md item 13) — the
    same trust tier as "can apply a label," since a random commenter on a
    public repo must not be able to redispatch the agent with content of
    their choosing. That would reopen the exact prompt-injection gate the
    trigger label exists to close (docs/design.md's split surface).
    """
    id: int
    user: str
    body: str
    author_association: str = "NONE"


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


@dataclass(frozen=True)
class NewReviewComment:
    """One inline comment to attach to a draft review (`create_review`) --
    the input shape for what `ReviewComment` above is the output shape of.
    A separate type rather than reusing `ReviewComment` itself: `id` and
    `user` are GitHub's to assign once the review is created, never a
    caller's to supply, and reusing it would mean every call site invents
    placeholder values for fields that mean nothing until the request
    round-trips.

    `line` is the line number in the file's *new* version (the right-hand
    side of the diff) that GitHub's own reviews API expects a `comments[]`
    entry to name -- the same "new" side `ReviewComment.line` already
    reads back once a comment exists.
    """
    path: str
    line: int
    body: str


@dataclass(frozen=True)
class CheckRun:
    """One check run against a commit -- GitHub's own shape for CI results
    (a GitHub Actions job, or any third-party check that posts through the
    Checks API), read by `core.py`'s `_close_finished_prs` (bwsalmon/agents#83)
    to decide whether an open PR's tests are failing.

    `status` is GitHub's own lifecycle field: `"queued"`, `"in_progress"`,
    or `"completed"` -- only `"completed"` has a meaningful `conclusion` at
    all, which is why callers check `status` before ever looking at it.
    `conclusion` is `None` until then, and one of `"success"`, `"failure"`,
    `"neutral"`, `"cancelled"`, `"skipped"`, `"timed_out"`, or
    `"action_required"` once it is -- GitHub's own enum, not narrowed here,
    since which of those count as "broken" is a policy decision for the
    caller, not this client.
    """
    name: str
    status: str
    conclusion: str | None


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
    def __init__(self, transport: Transport, token: str | None | TokenSource) -> None:
        self.transport = transport
        # A bare token is the degenerate `TokenSource`, wrapped here rather
        # than special-cased at every call site — so `_headers` has exactly
        # one way to find a token, and a caller that only has one keeps
        # passing it unchanged.
        self.tokens: TokenSource = (
            token if hasattr(token, "token_for") else StaticToken(token)
        )

    def _headers(self, owner: str, repo: str, *, json_body: bool = False) -> dict[str, str]:
        headers = {
            "Accept": "application/vnd.github+json",
            "User-Agent": "grain-automation",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        token = self.tokens.token_for(owner, repo)
        if token:
            headers["Authorization"] = f"token {token}"
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
                method="GET", path=path, headers=self._headers(owner, repo), body=None
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
                    state=item.get("state", "open"),
                ))
            path = _next_page_path(resp.headers.get("Link"))
        return issues

    def get_issue(self, owner: str, repo: str, number: int) -> Issue:
        """A single issue, fetched fresh — used to read an issue's current
        title when it isn't otherwise on hand (e.g. PR creation, which only
        carries the issue number through `Outcome`, not its title).
        """
        resp = self.transport.request(
            method="GET", path=f"/repos/{owner}/{repo}/issues/{number}",
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return Issue(
            number=data["number"], title=data["title"], body=data.get("body") or "",
            html_url=data["html_url"],
            labels=frozenset(l["name"] for l in data["labels"]),
            state=data.get("state", "open"),
        )

    def add_label(self, owner: str, repo: str, number: int, label: str) -> None:
        resp = self.transport.request(
            method="POST",
            path=f"/repos/{owner}/{repo}/issues/{number}/labels",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({"labels": [label]}).encode(),
        )
        if resp.status not in (200, 201):
            raise GitHubError(resp.status, resp.body)

    def remove_label(self, owner: str, repo: str, number: int, label: str) -> None:
        resp = self.transport.request(
            method="DELETE",
            path=f"/repos/{owner}/{repo}/issues/{number}/labels/{quote(label)}",
            headers=self._headers(owner, repo), body=None,
        )
        # 404 means the label is already off the issue — a fine outcome,
        # not an error, since the caller's intent ("this label should not
        # be on there") is already satisfied.
        if resp.status not in (200, 404):
            raise GitHubError(resp.status, resp.body)

    def close_issue(self, owner: str, repo: str, number: int) -> None:
        """Closes a task issue directly. bwsalmon/agents#23: a fully
        qualified `Closes owner/repo#N` in a PR body only auto-closes
        within the *same* repo it's opened in — across repos (the task/
        target split's normal case) it just links, never closes — so
        `core.py` closes the task issue itself rather than relying on
        the PR body text to do it.
        """
        resp = self.transport.request(
            method="PATCH",
            path=f"/repos/{owner}/{repo}/issues/{number}",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({"state": "closed"}).encode(),
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)

    def reopen_issue(self, owner: str, repo: str, number: int) -> None:
        """Reopens a task issue GitHub had closed. bwsalmon/agents#135:
        `_close_finished_prs` closes a task issue once its PR itself
        closes, but a human reviewing that "done" work may still come back
        with a follow-up comment instead of relabelling it -- `core.py`'s
        `_restart_commented_completions` is what reacts to that, and this
        is the other half of `close_issue`'s own PATCH, just the other
        state value.
        """
        resp = self.transport.request(
            method="PATCH",
            path=f"/repos/{owner}/{repo}/issues/{number}",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({"state": "open"}).encode(),
        )
        if resp.status != 200:
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
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status == 200:
            return True
        if resp.status == 404:
            return False
        raise GitHubError(resp.status, resp.body)

    def get_branch_head(self, owner: str, repo: str, branch: str) -> BranchHead | None:
        """`branch`'s tip commit, or None if the branch doesn't exist -- the
        same GET `branch_exists` makes, kept as its own method rather than
        folded into it since most callers only ever need the boolean and
        this one's response also carries the head commit's own message
        (bwsalmon/agents#79), which only `core.py`'s fresh-PR path needs, to
        seed a real description instead of a generic one.
        """
        resp = self.transport.request(
            method="GET",
            path=f"/repos/{owner}/{repo}/branches/{quote(branch, safe='')}",
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status == 404:
            return None
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        commit = data["commit"]
        return BranchHead(sha=commit["sha"], message=commit["commit"]["message"])

    def create_pull_request(self, owner: str, repo: str, *, head: str, base: str,
                             title: str, body: str = "") -> PullRequest:
        resp = self.transport.request(
            method="POST",
            path=f"/repos/{owner}/{repo}/pulls",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps(
                {"title": title, "head": head, "base": base, "body": body}
            ).encode(),
        )
        if resp.status != 201:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return PullRequest(number=data["number"], html_url=data["html_url"])

    def find_open_pull_request_for_branch(
        self, owner: str, repo: str, branch: str,
    ) -> PullRequest | None:
        """The open PR whose head is `branch`, if there is one.

        GitHub allows at most one open PR per head branch, so this is
        either exactly one or none -- `core.py`'s `_finish_succeeded_issue`
        uses it to recognise "the PR for this task already exists" after a
        `create_pull_request` 422, rather than failing a finish that has in
        fact already succeeded.

        `head` is qualified `owner:branch` the way GitHub's own filter
        requires; grain only ever pushes to branches in the target repo
        itself (never a fork), so the head owner is always `owner`.
        """
        resp = self.transport.request(
            method="GET",
            path=(f"/repos/{owner}/{repo}/pulls"
                  f"?state=open&head={quote(f'{owner}:{branch}', safe='')}"),
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        for data in json.loads(resp.body):
            return PullRequest(number=data["number"], html_url=data["html_url"])
        return None

    def create_issue(self, owner: str, repo: str, *, title: str, body: str = "",
                      labels: list[str] | None = None) -> Issue:
        """Files a new issue on the task repo -- bwsalmon/agents#83's
        `core.py`'s `_suggest_fix` is the only caller today, filing the
        follow-up task that fixes an open PR's conflicts or failing checks.
        `labels` lets it land already carrying `needs_approval_label`
        (`AutomationConfig`) in the same request, rather than a second
        `add_label` call that could fail partway and leave the issue
        unlabelled.
        """
        payload: dict = {"title": title, "body": body}
        if labels:
            payload["labels"] = list(labels)
        resp = self.transport.request(
            method="POST", path=f"/repos/{owner}/{repo}/issues",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps(payload).encode(),
        )
        if resp.status != 201:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return Issue(
            number=data["number"], title=data["title"], body=data.get("body") or "",
            html_url=data["html_url"],
            labels=frozenset(l["name"] for l in data.get("labels", [])),
        )

    def merge_pull_request(self, owner: str, repo: str, number: int) -> None:
        """Merges a PR directly, rather than leaving it for a human to
        click -- bwsalmon/agents#83's `core.py`'s `_close_finished_prs`
        calls this only for a PR whose task carried `/auto-merge`, and only
        once it reads clean (no conflict, no failing/pending check). A 405
        (not mergeable -- GitHub's own answer if `mergeable` went stale
        between the read and this call) or 409 (base branch moved
        underneath it) is left for the caller to decide whether to retry
        next cycle; only a genuine `GitHubError` a caller doesn't expect is
        the difference between those and, say, a 404.
        """
        resp = self.transport.request(
            method="PUT", path=f"/repos/{owner}/{repo}/pulls/{number}/merge",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({}).encode(),
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)

    def get_pull_request(self, owner: str, repo: str, number: int) -> PullRequestDetail:
        resp = self.transport.request(
            method="GET", path=f"/repos/{owner}/{repo}/pulls/{number}",
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        data = json.loads(resp.body)
        return PullRequestDetail(
            number=data["number"], title=data["title"], body=data.get("body") or "",
            html_url=data["html_url"],
            head_ref=data["head"]["ref"], base_ref=data["base"]["ref"],
            state=data.get("state", "open"), mergeable=data.get("mergeable"),
        )

    def default_branch(self, owner: str, repo: str) -> str:
        """The target repo's own default branch — the base a PR opened
        there should target when a task's `/base` directive doesn't say
        otherwise.

        Read from GitHub rather than configured: with one repo per
        deployment a single `base_branch` setting was a fair guess, but a
        task repo dispatching into many target repos would need an operator
        to keep a per-repo table of "main" vs "master" vs "trunk" correct,
        and GitHub already knows. `core.py` reads this once at dispatch and
        records it on the assignment, so the base can't change out from
        under a run between dispatch and PR creation.
        """
        resp = self.transport.request(
            method="GET", path=f"/repos/{owner}/{repo}",
            headers=self._headers(owner, repo), body=None,
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        return json.loads(resp.body)["default_branch"]

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
                method="GET", path=path, headers=self._headers(owner, repo), body=None
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

    def list_check_runs(self, owner: str, repo: str, ref: str) -> list[CheckRun]:
        """Check runs against `ref` -- a branch name works fine here, GitHub
        resolves it to that branch's current tip itself, so callers never
        need a commit sha in hand (`core.py`'s `_close_finished_prs`, which
        only ever has `PullRequestDetail.head_ref`, bwsalmon/agents#83).
        Paginates the same `Link`-header way every other list endpoint
        here does, but the response body itself is shaped differently --
        `{"total_count": N, "check_runs": [...]}`, not a bare array, which
        is GitHub's own shape for this one endpoint.
        """
        runs: list[CheckRun] = []
        path = f"/repos/{owner}/{repo}/commits/{quote(ref, safe='')}/check-runs?per_page=100"
        while path:
            resp = self.transport.request(
                method="GET", path=path, headers=self._headers(owner, repo), body=None
            )
            if resp.status != 200:
                raise GitHubError(resp.status, resp.body)
            for item in json.loads(resp.body)["check_runs"]:
                runs.append(CheckRun(
                    name=item["name"], status=item["status"],
                    conclusion=item.get("conclusion"),
                ))
            path = _next_page_path(resp.headers.get("Link"))
        return runs

    def list_comments(self, owner: str, repo: str, number: int) -> list[Comment]:
        """The plain top-level conversation on an issue or PR (docs/roadmap.md
        item 12) — where a human's reply to an agent's `ask_question` call
        lands. Same shared issues-comments endpoint and pagination shape
        `list_review_comments` uses for its own (inline) endpoint.
        """
        comments: list[Comment] = []
        path = f"/repos/{owner}/{repo}/issues/{number}/comments?per_page=100"
        while path:
            resp = self.transport.request(
                method="GET", path=path, headers=self._headers(owner, repo), body=None
            )
            if resp.status != 200:
                raise GitHubError(resp.status, resp.body)
            for item in json.loads(resp.body):
                comments.append(Comment(
                    id=item["id"], user=item.get("user", {}).get("login", ""),
                    body=item.get("body") or "",
                    author_association=item.get("author_association", "NONE"),
                ))
            path = _next_page_path(resp.headers.get("Link"))
        return comments

    def create_comment(self, owner: str, repo: str, number: int, body: str) -> int:
        """Posts a top-level comment — the operation docs/design.md's split
        surface originally noted as absent. Still not something the agent
        can reach directly (that boundary is unchanged): `core.py` is the
        only caller, and only to relay an `ask_question` call
        (docs/roadmap.md item 12) to a human.

        Returns the new comment's id (docs/roadmap.md item 13) — `core.py`
        records it as the baseline for "has a trusted reply arrived after
        this," since a comment's own id is the one thing that can't be
        spoofed by editing an earlier comment's body.
        """
        resp = self.transport.request(
            method="POST", path=f"/repos/{owner}/{repo}/issues/{number}/comments",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({"body": body}).encode(),
        )
        if resp.status != 201:
            raise GitHubError(resp.status, resp.body)
        return json.loads(resp.body)["id"]

    def create_review(self, owner: str, repo: str, number: int, *, body: str = "",
                       comments: list[NewReviewComment] = ()) -> int:
        """Creates a **draft** review on a pull request -- `core.py`'s
        `_finish_succeeded_review` (bwsalmon/agents#154) is the only
        caller, once a `/review`-directed dispatch finishes, the same
        "only `core.py` ever calls the mutating half" boundary
        `create_comment` documents above. No `event` key in the request
        body at all: GitHub's reviews API treats an omitted `event` as
        "leave this PENDING" (its own reference for "Create a review for a
        pull request"), which is what keeps this from ever auto-submitting
        an approval, a request for changes, or even a plain comment review
        on the agent's own behalf -- see this module's own docstring for
        why that has to wait on a human.

        Returns the new review's id -- unused by `core.py` today, kept for
        the same reason `create_comment` returns one: cheap, and the
        pattern every other creation call here already follows.
        """
        resp = self.transport.request(
            method="POST", path=f"/repos/{owner}/{repo}/pulls/{number}/reviews",
            headers=self._headers(owner, repo, json_body=True),
            body=json.dumps({
                "body": body,
                "comments": [
                    {"path": c.path, "line": c.line, "body": c.body}
                    for c in comments
                ],
            }).encode(),
        )
        if resp.status != 200:
            raise GitHubError(resp.status, resp.body)
        return json.loads(resp.body)["id"]


@dataclass
class DryRunGitHubClient:
    """Wraps a real client: reads pass through, label mutations print
    instead of firing. Same split `run.py`'s `DryRunRunner` makes for local
    commands — "read-only commands still execute."
    """

    inner: GitHubClient

    def list_issues(self, owner: str, repo: str, label: str) -> list[Issue]:
        return self.inner.list_issues(owner, repo, label)

    def get_issue(self, owner: str, repo: str, number: int) -> Issue:
        return self.inner.get_issue(owner, repo, number)

    def add_label(self, owner: str, repo: str, number: int, label: str) -> None:
        print(f"+ add label {label!r} to {owner}/{repo}#{number}")

    def remove_label(self, owner: str, repo: str, number: int, label: str) -> None:
        print(f"+ remove label {label!r} from {owner}/{repo}#{number}")

    def close_issue(self, owner: str, repo: str, number: int) -> None:
        print(f"+ close issue {owner}/{repo}#{number}")

    def reopen_issue(self, owner: str, repo: str, number: int) -> None:
        print(f"+ reopen issue {owner}/{repo}#{number}")

    def branch_exists(self, owner: str, repo: str, branch: str) -> bool:
        return self.inner.branch_exists(owner, repo, branch)

    def get_branch_head(self, owner: str, repo: str, branch: str) -> BranchHead | None:
        return self.inner.get_branch_head(owner, repo, branch)

    def find_open_pull_request_for_branch(
        self, owner: str, repo: str, branch: str,
    ) -> PullRequest | None:
        return self.inner.find_open_pull_request_for_branch(owner, repo, branch)

    def create_pull_request(self, owner: str, repo: str, *, head: str, base: str,
                             title: str, body: str = "") -> PullRequest:
        print(f"+ open PR {owner}/{repo}: {head!r} -> {base!r} ({title!r})")
        return PullRequest(number=0, html_url=f"(dry run) {owner}/{repo}: {head} -> {base}")

    def create_issue(self, owner: str, repo: str, *, title: str, body: str = "",
                      labels: list[str] | None = None) -> Issue:
        print(f"+ file issue {owner}/{repo}: {title!r} {list(labels or [])}")
        return Issue(number=0, title=title, body=body,
                      html_url=f"(dry run) {owner}/{repo}", labels=frozenset(labels or []))

    def merge_pull_request(self, owner: str, repo: str, number: int) -> None:
        print(f"+ merge PR {owner}/{repo}#{number}")

    def get_pull_request(self, owner: str, repo: str, number: int) -> PullRequestDetail:
        return self.inner.get_pull_request(owner, repo, number)

    def default_branch(self, owner: str, repo: str) -> str:
        return self.inner.default_branch(owner, repo)

    def list_review_comments(self, owner: str, repo: str, number: int) -> list[ReviewComment]:
        return self.inner.list_review_comments(owner, repo, number)

    def list_check_runs(self, owner: str, repo: str, ref: str) -> list[CheckRun]:
        return self.inner.list_check_runs(owner, repo, ref)

    def list_comments(self, owner: str, repo: str, number: int) -> list[Comment]:
        return self.inner.list_comments(owner, repo, number)

    def create_comment(self, owner: str, repo: str, number: int, body: str) -> int:
        print(f"+ comment on {owner}/{repo}#{number}: {body!r}")
        return 0

    def create_review(self, owner: str, repo: str, number: int, *, body: str = "",
                       comments: list[NewReviewComment] = ()) -> int:
        print(f"+ draft review on {owner}/{repo}#{number}: {body!r} "
              f"({len(comments)} inline comment(s))")
        return 0
