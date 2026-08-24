"""Pool assignments and rate-limit history, persisted across cron runs.

This is the one file in the package that can't just copy an existing
pattern verbatim: every JSON file elsewhere in the repo
(`repo-allowlist.json`, `credentials.json`, `sandbox-tokens.json`) is
read-only from the consumer's side, written by an operator or a
provisioning step. This one is read-modify-written by `grain automation
run-once` itself, and a cron job can be killed mid-write — so saving goes
through a temp file and an atomic rename rather than a direct
`Path.write_text`, the same guarantee a crashed write must not corrupt the
one record of which sandbox is doing what.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from pathlib import Path


class TriggerKind(Enum):
    """What started this assignment — which decides what a *successful*
    finish means (docs/roadmap.md item 9): an issue-triggered run needs a
    new PR opened once its branch shows up; a PR-triggered run already has
    its PR, so a successful finish just means new commits landed on the
    branch it was pointed at. Stored alongside the assignment rather than
    inferred, since `core.py`'s sweep-time handling has no other way to tell
    the two apart once `AutomationState` is reloaded from disk on a fresh
    `run-once` invocation.
    """
    ISSUE = "issue"
    PR = "pr"


@dataclass(frozen=True)
class Assignment:
    # The trigger's number — an issue number for an ISSUE assignment, a PR
    # number for a PR one. Never ambiguous: GitHub gives issues and PRs one
    # shared number sequence per repo (a PR *is* a special kind of issue in
    # its own data model), so one repo can never have both an issue #5 and a
    # PR #5 to confuse this field's meaning.
    issue: int
    unit: str
    started_at: datetime
    kind: TriggerKind = TriggerKind.ISSUE
    # The branch this assignment is working on. For an ISSUE assignment this
    # is left unset — `dispatch.branch_name(issue)` recomputes it as a pure
    # function of the number whenever it's needed, per docs/roadmap.md item
    # 2's "deterministic, not self-reported" precedent. A PR assignment has
    # no such deterministic name to recompute (the branch is whatever the PR
    # author already called it), so it's recorded once here, at dispatch
    # time, when `GitHubClient.get_pull_request` already had to read it
    # anyway.
    branch: str | None = None
    # The *target* repo this assignment's work is happening in — the one
    # cloned, pushed to, and opened a PR against — as opposed to the task
    # repo the trigger issue itself lives in (`AutomationConfig`'s
    # `task_owner`/`task_repo`, which is what `issue` above is a number in).
    # Recorded at dispatch from the task's `/repo` directive rather than
    # re-parsed at sweep time: an issue body is editable, and an edit landing
    # mid-run must not be able to redirect where the finished work's PR gets
    # opened. Same "decide once, verify don't trust" discipline
    # `branch_name()` already applies to the branch.
    #
    # `None` means an assignment written before the task/target split, when
    # a deployment had exactly one repo; `core.py` reads that as the task
    # repo, which is precisely what it meant then.
    target_owner: str | None = None
    target_repo: str | None = None
    # The base branch a PR from this assignment targets — the task's `/base`
    # directive, else the target repo's own default branch, read once at
    # dispatch (`GitHubClient.default_branch`) and pinned here for the same
    # reason `target_owner`/`target_repo` are.
    base: str | None = None


@dataclass(frozen=True)
class PendingQuestion:
    """Tracks one issue/PR sitting idle after an `ask_question` call
    (docs/roadmap.md item 13), waiting for a trusted reply to auto-redispatch
    it without an operator re-applying the trigger label by hand.

    `question_comment_id` is the id of the question comment `core.py`'s
    `_finish_question` itself posted — the baseline a later check compares
    the issue's current comment thread against: any comment with a *higher*
    id, from a trusted author, means someone replied after the question was
    asked. A comment id, not a timestamp or a count, because it can't be
    spoofed by editing an older comment's body and is stable even if
    comments in between get deleted.
    """
    issue: int
    question_comment_id: int
    kind: TriggerKind = TriggerKind.ISSUE
    branch: str | None = None


def pr_key(owner: str, repo: str, number: int) -> str:
    """The stable identity of one tracked PR (bwsalmon/agents#24) --
    `AutomationState.tracked_prs`'s dict key. An owner/repo/number triple
    has no single field that can key it alone, since two different target
    repos can each have their own PR #1. Computed here, not inlined at each
    call site, so `core.py` and `AutomationState` can never disagree on it.
    """
    return f"{owner}/{repo}#{number}"


@dataclass(frozen=True)
class TrackedPullRequest:
    """One PR grain itself opened, watched for new feedback to turn into
    candidate tasks (bwsalmon/agents#24). Only a fresh-branch dispatch's own
    PR is tracked (`core.py`'s `_finish_succeeded_issue`, right after
    `create_pull_request` succeeds) -- a `/pr`-continuation task can point
    at a PR grain never opened, whose pre-existing comment history isn't
    feedback grain caused, and would flood the task repo with backlog on
    the very first triage pass if it were tracked from zero.

    `last_review_comment_id`/`last_comment_id` are the highest id already
    considered on each of a PR's two comment surfaces (inline review
    comments, top-level conversation) -- the same "an id, not a count or a
    timestamp" baseline `PendingQuestion.question_comment_id` already
    relies on, for the same reason: unspoofable by editing an older
    comment, and stable even if something in between gets deleted. Both
    start at zero for a freshly tracked PR, which by construction has no
    comments yet.
    """
    owner: str
    repo: str
    number: int
    origin_task_issue: int
    last_review_comment_id: int = 0
    last_comment_id: int = 0


@dataclass
class AutomationState:
    assignments: dict[str, Assignment] = field(default_factory=dict)
    run_timestamps: list[datetime] = field(default_factory=list)
    # Keyed by str(issue number) -- JSON object keys must be strings, same
    # reason `assignments` is keyed by sandbox name rather than an int.
    pending_questions: dict[str, PendingQuestion] = field(default_factory=dict)
    # Keyed by `pr_key(owner, repo, number)` -- see `TrackedPullRequest`.
    tracked_prs: dict[str, TrackedPullRequest] = field(default_factory=dict)

    @classmethod
    def load(cls, path: Path) -> "AutomationState":
        if not path.exists():
            return cls()
        raw = json.loads(path.read_text())
        assignments = {
            name: Assignment(
                issue=a["issue"], unit=a["unit"],
                started_at=datetime.fromisoformat(a["started_at"]),
                # .get() with a default, not a required key: an
                # already-on-disk state file written before item 9 has
                # neither field, and every assignment it could hold was
                # necessarily an issue dispatch with no recorded branch.
                kind=TriggerKind(a.get("kind", TriggerKind.ISSUE.value)),
                branch=a.get("branch"),
                target_owner=a.get("target_owner"),
                target_repo=a.get("target_repo"),
                base=a.get("base"),
            )
            for name, a in raw.get("assignments", {}).items()
        }
        run_timestamps = [
            datetime.fromisoformat(t) for t in raw.get("run_timestamps", [])
        ]
        pending_questions = {
            key: PendingQuestion(
                issue=q["issue"], question_comment_id=q["question_comment_id"],
                kind=TriggerKind(q.get("kind", TriggerKind.ISSUE.value)),
                branch=q.get("branch"),
            )
            for key, q in raw.get("pending_questions", {}).items()
        }
        tracked_prs = {
            key: TrackedPullRequest(
                owner=t["owner"], repo=t["repo"], number=t["number"],
                origin_task_issue=t["origin_task_issue"],
                last_review_comment_id=t.get("last_review_comment_id", 0),
                last_comment_id=t.get("last_comment_id", 0),
            )
            for key, t in raw.get("tracked_prs", {}).items()
        }
        return cls(assignments=assignments, run_timestamps=run_timestamps,
                    pending_questions=pending_questions, tracked_prs=tracked_prs)

    def save(self, path: Path) -> None:
        data = {
            "assignments": {
                name: {
                    "issue": a.issue, "unit": a.unit,
                    "started_at": a.started_at.isoformat(),
                    "kind": a.kind.value, "branch": a.branch,
                    "target_owner": a.target_owner, "target_repo": a.target_repo,
                    "base": a.base,
                }
                for name, a in self.assignments.items()
            },
            "run_timestamps": [t.isoformat() for t in self.run_timestamps],
            "pending_questions": {
                key: {
                    "issue": q.issue, "question_comment_id": q.question_comment_id,
                    "kind": q.kind.value, "branch": q.branch,
                }
                for key, q in self.pending_questions.items()
            },
            "tracked_prs": {
                key: {
                    "owner": t.owner, "repo": t.repo, "number": t.number,
                    "origin_task_issue": t.origin_task_issue,
                    "last_review_comment_id": t.last_review_comment_id,
                    "last_comment_id": t.last_comment_id,
                }
                for key, t in self.tracked_prs.items()
            },
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(json.dumps(data, indent=2))
        tmp.replace(path)

    # --- pool ---------------------------------------------------------
    def free_sandbox(self, sandbox_names: list[str]) -> str | None:
        for name in sandbox_names:
            if name not in self.assignments:
                return name
        return None

    def assign(self, sandbox: str, issue: int, unit: str, now: datetime, *,
               kind: TriggerKind = TriggerKind.ISSUE, branch: str | None = None,
               target_owner: str | None = None, target_repo: str | None = None,
               base: str | None = None) -> None:
        self.assignments[sandbox] = Assignment(
            issue=issue, unit=unit, started_at=now, kind=kind, branch=branch,
            target_owner=target_owner, target_repo=target_repo, base=base,
        )

    def release(self, sandbox: str) -> None:
        self.assignments.pop(sandbox, None)

    def in_progress_issues(self) -> set[int]:
        return {a.issue for a in self.assignments.values()}

    # --- pending questions (docs/roadmap.md item 13) ---------------------
    def record_pending_question(self, issue: int, question_comment_id: int, *,
                                 kind: TriggerKind = TriggerKind.ISSUE,
                                 branch: str | None = None) -> None:
        self.pending_questions[str(issue)] = PendingQuestion(
            issue=issue, question_comment_id=question_comment_id,
            kind=kind, branch=branch,
        )

    def clear_pending_question(self, issue: int) -> None:
        self.pending_questions.pop(str(issue), None)

    # --- feedback triage (bwsalmon/agents#24) ----------------------------
    def track_pull_request(self, owner: str, repo: str, number: int, *,
                            origin_task_issue: int) -> None:
        """Starts watching a freshly opened PR for feedback. A no-op if
        already tracked (idempotent against a redispatch that somehow saw
        this PR before -- not expected in practice, since each fresh-branch
        PR is opened exactly once, but re-tracking would reset the baseline
        back to zero and replay the PR's whole comment history as "new").
        """
        key = pr_key(owner, repo, number)
        if key in self.tracked_prs:
            return
        self.tracked_prs[key] = TrackedPullRequest(
            owner=owner, repo=repo, number=number, origin_task_issue=origin_task_issue,
        )

    def update_tracked_pull_request(self, key: str, *, last_review_comment_id: int,
                                     last_comment_id: int) -> None:
        tracked = self.tracked_prs.get(key)
        if tracked is None:
            return
        self.tracked_prs[key] = TrackedPullRequest(
            owner=tracked.owner, repo=tracked.repo, number=tracked.number,
            origin_task_issue=tracked.origin_task_issue,
            last_review_comment_id=last_review_comment_id,
            last_comment_id=last_comment_id,
        )

    def untrack_pull_request(self, key: str) -> None:
        self.tracked_prs.pop(key, None)

    # --- rate limit -----------------------------------------------------
    def record_run(self, now: datetime) -> None:
        self.run_timestamps.append(now)
        # Nothing downstream needs more than the last hour; trim on write
        # so the file does not grow without bound over a long deployment.
        cutoff = now.timestamp() - 24 * 3600
        self.run_timestamps = [
            t for t in self.run_timestamps if t.timestamp() >= cutoff
        ]


def utcnow() -> datetime:
    return datetime.now(timezone.utc)
