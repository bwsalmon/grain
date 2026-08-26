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
from dataclasses import dataclass, field, replace
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
    # The full resource name of a Gemini API key minted for this task
    # (bwsalmon/agents#47, `gemini_keys.create_key`), or `None` for the
    # common case of no `gemini_key_label` on the task issue. Recorded
    # here, not re-derived at sweep time, for the same reason
    # `target_owner`/`target_repo` are: `sweeper.py`'s `_release` needs to
    # know which key to revoke once this assignment is freed, and by then
    # the task's own labels are no longer read.
    gemini_key_name: str | None = None
    # bwsalmon/agents#126: the bare id of a GCP service-account key minted
    # for this task (`gcp_keys.create_key`), or `None` for a deployment
    # that never configured `Orchestrator.gcp_key_config`. Recorded here
    # for the same reason `gemini_key_name` above is: `sweeper.py`'s
    # `_release` needs to know which key to revoke once this assignment is
    # freed, and by then `core.py` no longer has it in hand.
    gcp_key_id: str | None = None
    # bwsalmon/agents#83: whether the task's own `/auto-merge` directive
    # asked for its resulting PR to be merged automatically rather than
    # left for a human to review -- see `directives.py`'s own docstring.
    # Recorded here, not re-derived at finish time, for the same "an issue
    # body is editable mid-run" reason every other directive-derived field
    # on this dataclass already is: `core.py`'s `_finish_succeeded_issue`
    # needs it to decide how to record the PR it just opened
    # (`OpenPullRequest.auto_merge`), and by finish time the task's own
    # directives are no longer read.
    auto_merge: bool = False


@dataclass(frozen=True)
class OpenPullRequest:
    """Tracks one task issue whose PR (opened by `core.py`'s
    `_finish_succeeded_issue`) is still open, so a later `run_once` can
    close the task issue once that PR itself closes (bwsalmon/agents#54)
    rather than the moment the PR is opened -- the trigger for that lives
    in the *target* repo, not the task repo the issue itself is in
    (`AutomationConfig`'s task/target split), so there is no label move or
    webhook to react to; this is what a poll on a later `run_once` checks
    against.

    `issue` is the task issue's own number, in the task repo -- what
    `close_issue` needs; `target_owner`/`target_repo`/`pr_number` are where
    to look for that PR's current state, recorded once at PR-creation time
    the same "decide once, don't re-derive" way `Assignment.target_owner`
    already is.
    """
    issue: int
    target_owner: str
    target_repo: str
    pr_number: int
    # bwsalmon/agents#83: whether this PR came from a task carrying
    # `/auto-merge` (`Assignment.auto_merge`, `core.py`'s
    # `_finish_succeeded_issue`) -- `_close_finished_prs` merges it itself
    # once it reads clean, rather than only ever waiting on a human to
    # close it the way an ordinary task's PR does.
    auto_merge: bool = False
    # bwsalmon/agents#83: the task issue number of the fix `core.py`'s
    # `_suggest_fix` filed for this PR, once it has (so a later cycle
    # doesn't file a second one for the same conflict or failing check --
    # `_close_finished_prs` only calls `_suggest_fix` while this is still
    # `None`). Unset for the overwhelming common case of a PR that never
    # needed one.
    fix_issue: int | None = None


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


@dataclass(frozen=True)
class CompletedIssue:
    """Tracks one task issue currently carrying `completed_label`, so a
    later poll (`core.py`'s `_restart_commented_completions`,
    bwsalmon/agents#135) can restart it -- reopen it if GitHub had already
    closed it, and put `trigger_label` back on -- once a human comments on
    it instead of relabelling it by hand.

    `baseline_comment_id` starts `None` rather than being filled in at
    completion time the way `PendingQuestion.question_comment_id` is: two
    of the three finish paths that apply `completed_label`
    (`_finish_succeeded_issue`, `_finish_succeeded_pr`) never post a
    comment of their own, so there is no id finish time can hand back as
    "the highest comment on this issue right now." The first poll after
    completion primes this field from a fresh `list_comments` read instead
    of comparing against anything -- comparing on that very first read
    would risk treating either a comment already on the issue before this
    run even started, or (the third finish path's own
    `comment_on_issue` reply) the automation comment `_finish_no_changes`/
    `_finish_question` just posted, as a "new" one and restarting a task
    nobody actually asked to restart.
    """
    issue: int
    baseline_comment_id: int | None = None


@dataclass
class AutomationState:
    assignments: dict[str, Assignment] = field(default_factory=dict)
    run_timestamps: list[datetime] = field(default_factory=list)
    # Keyed by str(issue number) -- JSON object keys must be strings, same
    # reason `assignments` is keyed by sandbox name rather than an int.
    pending_questions: dict[str, PendingQuestion] = field(default_factory=dict)
    # Same string-keying reason as `pending_questions` -- one task issue can
    # only ever have one PR open for it at a time (a fresh dispatch never
    # redispatches an issue already `in_progress_issues()`), so the issue
    # number is a safe, simple key here too.
    open_pull_requests: dict[str, OpenPullRequest] = field(default_factory=dict)
    # Same string-keying reason again -- bwsalmon/agents#135, one task issue
    # is never completed twice at once.
    completed_issues: dict[str, CompletedIssue] = field(default_factory=dict)

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
                gemini_key_name=a.get("gemini_key_name"),
                gcp_key_id=a.get("gcp_key_id"),
                auto_merge=a.get("auto_merge", False),
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
        open_pull_requests = {
            key: OpenPullRequest(
                issue=o["issue"], target_owner=o["target_owner"],
                target_repo=o["target_repo"], pr_number=o["pr_number"],
                auto_merge=o.get("auto_merge", False),
                fix_issue=o.get("fix_issue"),
            )
            for key, o in raw.get("open_pull_requests", {}).items()
        }
        completed_issues = {
            key: CompletedIssue(
                issue=c["issue"], baseline_comment_id=c.get("baseline_comment_id"),
            )
            for key, c in raw.get("completed_issues", {}).items()
        }
        return cls(assignments=assignments, run_timestamps=run_timestamps,
                    pending_questions=pending_questions,
                    open_pull_requests=open_pull_requests,
                    completed_issues=completed_issues)

    def save(self, path: Path) -> None:
        data = {
            "assignments": {
                name: {
                    "issue": a.issue, "unit": a.unit,
                    "started_at": a.started_at.isoformat(),
                    "kind": a.kind.value, "branch": a.branch,
                    "target_owner": a.target_owner, "target_repo": a.target_repo,
                    "base": a.base, "gemini_key_name": a.gemini_key_name,
                    "gcp_key_id": a.gcp_key_id,
                    "auto_merge": a.auto_merge,
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
            "open_pull_requests": {
                key: {
                    "issue": o.issue, "target_owner": o.target_owner,
                    "target_repo": o.target_repo, "pr_number": o.pr_number,
                    "auto_merge": o.auto_merge, "fix_issue": o.fix_issue,
                }
                for key, o in self.open_pull_requests.items()
            },
            "completed_issues": {
                key: {"issue": c.issue, "baseline_comment_id": c.baseline_comment_id}
                for key, c in self.completed_issues.items()
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
               base: str | None = None, gemini_key_name: str | None = None,
               gcp_key_id: str | None = None, auto_merge: bool = False) -> None:
        self.assignments[sandbox] = Assignment(
            issue=issue, unit=unit, started_at=now, kind=kind, branch=branch,
            target_owner=target_owner, target_repo=target_repo, base=base,
            gemini_key_name=gemini_key_name, gcp_key_id=gcp_key_id,
            auto_merge=auto_merge,
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

    # --- open PRs awaiting a close (bwsalmon/agents#54) ------------------
    def record_open_pr(self, issue: int, target_owner: str, target_repo: str,
                        pr_number: int, *, auto_merge: bool = False) -> None:
        self.open_pull_requests[str(issue)] = OpenPullRequest(
            issue=issue, target_owner=target_owner, target_repo=target_repo,
            pr_number=pr_number, auto_merge=auto_merge,
        )

    def clear_open_pr(self, issue: int) -> None:
        self.open_pull_requests.pop(str(issue), None)

    # --- suggested fixes (bwsalmon/agents#83) -----------------------------
    def mark_fix_suggested(self, issue: int, fix_issue: int) -> None:
        """Records that `core.py`'s `_suggest_fix` already filed
        `fix_issue` for the open PR tracked against `issue`, so a later
        `_close_finished_prs` cycle doesn't file a second one for the same
        conflict or failing check.
        """
        pending = self.open_pull_requests[str(issue)]
        self.open_pull_requests[str(issue)] = replace(pending, fix_issue=fix_issue)

    # --- restart on comment after completion (bwsalmon/agents#135) -------
    def record_completed_issue(self, issue: int) -> None:
        self.completed_issues[str(issue)] = CompletedIssue(issue=issue)

    def prime_completed_baseline(self, issue: int, baseline_comment_id: int) -> None:
        """Fills in the "highest comment id seen so far" baseline the
        first time `core.py`'s `_restart_commented_completions` polls a
        freshly completed issue -- see `CompletedIssue`'s own docstring
        for why this can't just be passed to `record_completed_issue` up
        front.
        """
        existing = self.completed_issues[str(issue)]
        self.completed_issues[str(issue)] = replace(
            existing, baseline_comment_id=baseline_comment_id,
        )

    def clear_completed_issue(self, issue: int) -> None:
        self.completed_issues.pop(str(issue), None)

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
