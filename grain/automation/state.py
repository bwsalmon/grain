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
    # time, when `GitHubClient.get_pull_request`/`list_pull_requests` already
    # had to read it anyway.
    branch: str | None = None


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


@dataclass
class AutomationState:
    assignments: dict[str, Assignment] = field(default_factory=dict)
    run_timestamps: list[datetime] = field(default_factory=list)
    # Keyed by str(issue number) -- JSON object keys must be strings, same
    # reason `assignments` is keyed by sandbox name rather than an int.
    pending_questions: dict[str, PendingQuestion] = field(default_factory=dict)

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
        return cls(assignments=assignments, run_timestamps=run_timestamps,
                    pending_questions=pending_questions)

    def save(self, path: Path) -> None:
        data = {
            "assignments": {
                name: {
                    "issue": a.issue, "unit": a.unit,
                    "started_at": a.started_at.isoformat(),
                    "kind": a.kind.value, "branch": a.branch,
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
               kind: TriggerKind = TriggerKind.ISSUE, branch: str | None = None) -> None:
        self.assignments[sandbox] = Assignment(
            issue=issue, unit=unit, started_at=now, kind=kind, branch=branch
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
