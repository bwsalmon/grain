"""The task model: the entities, independent of where they are stored.

`docs/data-model.md` is the reasoning; this is that document's decided
shape in code. Nothing here imports the store, and nothing here knows
about SQL -- which is the point of `docs/data-model.md`'s
"representation is not the model": Dolt is one representation of these
types, GitHub labels are another, and neither belongs in the types
themselves.

Four decisions from that document are load-bearing here, and each one is
why something is *absent* rather than present:

- **`TaskState` is not a field.** It is derived from approval plus
  grain's own observations, so `Task` has no `state` and no
  `state_since`. `state_of()` below computes it, and `schema.py` computes
  the same thing as a SQL view -- deliberately twice, so the test suite
  can hold them to agreeing.
- **Approval is an `Attribution`, not a bool**, so "who approved this, and
  did they do it directly?" is answerable from the task rather than only
  from the audit log.
- **Landing state is decided by the creating actor, never the reason** --
  see `lands_queued()`. A new `OriginReason` therefore cannot queue
  itself.
- **Blocked is not a state.** It is derived from links, and a blocked task
  is still `QUEUED`.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from enum import Enum


# --- principals (docs/data-model.md, "Principals") ---------------------

class PrincipalKind(Enum):
    AUTOMATION = "automation"   # the controller loop. One per deployment.
    AGENT = "agent"             # one dispatched run, in one sandbox.
    HUMAN = "human"             # a person.


@dataclass(frozen=True)
class PrincipalRef:
    kind: PrincipalKind
    # A GitHub login for HUMAN, a `Run.id` for AGENT, the deployment name
    # for AUTOMATION. Opaque here on purpose: this model never resolves a
    # principal to an account, it only records which one acted.
    id: str


@dataclass(frozen=True)
class Attribution:
    """Who performed an action, and whose output they were relaying.

    `on_behalf_of` is what `_AUTOMATION_SIGNATURE` gestures at with one
    bit. Grain relaying an agent's question is
    `(AUTOMATION, on_behalf_of=AGENT)`; grain restarting a task because a
    human commented is `(AUTOMATION, on_behalf_of=HUMAN)`; a human
    applying a label directly is `(HUMAN, None)`.
    """
    actor: PrincipalRef
    on_behalf_of: PrincipalRef | None = None


# --- origin (who asked, and why) ---------------------------------------

class OriginReason(Enum):
    DIRECT = "direct"      # somebody just filed it
    SCHEDULE = "schedule"  # a scheduled job fired from a template
    FIX = "fix"            # filed for a broken PR
    REVIEW = "review"      # review threads on a PR asked for it
    PROPOSAL = "proposal"  # an agent proposed it, or a parent decomposed


@dataclass(frozen=True)
class Origin:
    attribution: Attribution
    reason: OriginReason


def lands_queued(origin: Origin) -> bool:
    """Whether a task with this origin starts approved.

    **The actor decides; the reason does not.** A sixth `OriginReason`
    added later cannot queue itself, because reasons are not consulted --
    which is the trust gate stated as code rather than as a convention
    each filing path remembers.
    """
    return origin.attribution.actor.kind is PrincipalKind.HUMAN


# --- repos and folders --------------------------------------------------

@dataclass(frozen=True)
class RepoRef:
    owner: str
    name: str

    def __str__(self) -> str:
        return f"{self.owner}/{self.name}"

    @classmethod
    def parse(cls, text: str) -> "RepoRef":
        owner, sep, name = text.partition("/")
        if not sep or not owner or not name:
            raise ValueError(f"repo must be `owner/name`, got {text!r}")
        return cls(owner=owner, name=name)


class RepoBinding(Enum):
    DIRECTIVE = "directive"   # named explicitly -- pinned before dispatch
    DEFAULT = "default"       # the deployment's default target
    SCRATCH = "scratch"       # resolved at assignment, from the sandbox
    INHERITED = "inherited"   # a sub-task taking its parent's target


@dataclass(frozen=True)
class FolderRef:
    """A node in the containment tree, as a path from the root.

    `("payments", "owner/payments-api", "services/billing")` -- around
    (a group of repos) and inside (a path in one) are the same kind of
    node, which is why one type covers both.
    """
    path: tuple[str, ...]

    def __str__(self) -> str:
        return "/".join(self.path)


# --- capabilities -------------------------------------------------------

class GrantSource(Enum):
    LABEL = "label"          # a human applied it. The trust gate.
    DIRECTIVE = "directive"  # a trusted author wrote it
    FOLDER = "folder"        # a folder's `offers` granted it
    GRAIN = "grain"          # grain applied it to itself


class Provision(Enum):
    MINT = "mint"      # mints a lease from a standing credential
    SELECT = "select"  # names which standing credential to use
    GRANT = "grant"    # needs no credential at all


@dataclass(frozen=True)
class CredentialRef:
    """A credential *name*. Never material -- see docs/data-model.md,
    "the model holds names, never material". Resolving this to a token
    happens on the controller, at the moment of use, and the result never
    reaches this model, the store, a declaration, or a UI.
    """
    name: str


@dataclass(frozen=True)
class Grant:
    capability: str
    # How *this* task got it, which `Capability.source` cannot say: the
    # same capability can be label-requested on one task and folder-
    # inherited on another.
    via: GrantSource
    folder: FolderRef | None = None


@dataclass(frozen=True)
class Lease:
    """Something minted for a task that must be given back.

    `minted_by` is what turns revocation and rotation from control flow
    into data: releasing asks the lease what to call, and "which live
    leases came from the credential I am about to rotate?" becomes a
    query rather than an unanswerable question.
    """
    capability: str
    resource: str
    minted_by: CredentialRef
    issued_at: datetime
    expires_at: datetime | None = None


# --- links --------------------------------------------------------------

class LinkKind(Enum):
    DEPENDS_ON = "depends-on"    # blocks dispatch; evaluated every cycle
    CHILD_OF = "child-of"        # decomposition; blocks the parent
    FIXES = "fixes"              # -> a pull request, not a task
    MERGE_WITH = "merge-with"    # blocks the *merge*, not the run
    ADDRESSES = "addresses"      # -> a review thread
    PROPOSED_BY = "proposed-by"  # provenance only; never blocks


# Which kinds hold a task back from dispatch. `MERGE_WITH` is deliberately
# absent: it gates merging, not running, which is what lets the members of
# a coordinated change be worked in parallel.
_BLOCKING = frozenset({LinkKind.DEPENDS_ON, LinkKind.CHILD_OF})


def blocks(kind: LinkKind) -> bool:
    return kind in _BLOCKING


@dataclass(frozen=True)
class TaskLink:
    kind: LinkKind
    # A `TaskId` for DEPENDS_ON/CHILD_OF/PROPOSED_BY, a pull request or
    # review-thread reference for FIXES/ADDRESSES. Opaque here; the store
    # records which kind of target it is alongside.
    target: str

    @property
    def blocks(self) -> bool:
        return blocks(self.kind)


# --- tasks --------------------------------------------------------------

class TaskIntent(Enum):
    IMPLEMENT = "implement"  # fresh branch -> a new PR
    CONTINUE = "continue"    # more commits on an existing branch
    REVIEW = "review"        # post a draft review, push nothing
    ANALYZE = "analyze"      # answer in a comment; no branch expected


class TaskState(Enum):
    PROPOSED = "proposed"
    QUEUED = "queued"
    RUNNING = "running"
    AWAITING_REPLY = "awaiting_reply"
    COMPLETED = "completed"
    CLOSED = "closed"


@dataclass(frozen=True)
class Task:
    """The declared half: what a human (or grain, proposing) asked for.

    No `state` field. No `state_since`. See `state_of()`.
    """
    id: str
    intent: TaskIntent
    origin: Origin
    title: str

    # None means not approved, which is what makes a task PROPOSED.
    approval: Attribution | None = None

    target: RepoRef | None = None          # the one *write* target
    binding: RepoBinding = RepoBinding.DIRECTIVE
    base: str | None = None
    folder: FolderRef | None = None
    reads: tuple[RepoRef, ...] = ()        # read-only. Grant nothing.

    grants: frozenset[Grant] = frozenset()
    links: tuple[TaskLink, ...] = ()
    tags: frozenset[str] = frozenset()

    auto_merge: bool = False
    body: str = ""
    # Where this task appears for humans, if anywhere -- a GitHub issue,
    # today. A projection, not the identity: nothing here may assume it
    # exists, is an integer, or sorts.
    external_ref: str | None = None
    created_at: datetime | None = None


@dataclass(frozen=True)
class Observation:
    """The half grain writes about a task: what it has seen happen.

    Separate from `Task` because they answer to different records
    (docs/data-model.md, "whoever writes it, owns it") -- and separate in
    the schema for the same reason, which is what would let a declaration
    change be branched and reviewed while observations keep landing.
    """
    task_id: str
    closed_at: datetime | None = None
    completed_at: datetime | None = None
    # The id of the question comment grain itself posted -- the baseline a
    # later poll compares a fresh read against. A baseline, not a belief.
    pending_question_comment_id: int | None = None
    baseline_comment_id: int | None = None
    observed_at: datetime | None = None


@dataclass(frozen=True)
class Run:
    """One attempt. A live run is a `Run` with no `finished_at`."""
    id: str
    task_id: str
    # The concurrency unit and the VM. The same string while sandboxes are
    # long-lived; different once a sandbox is created per task.
    slot: str
    sandbox: str
    started_at: datetime
    attempt: int = 1
    unit: str | None = None
    finished_at: datetime | None = None
    outcome: str | None = None
    leases: tuple[Lease, ...] = ()


# --- derived ------------------------------------------------------------

def state_of(task: Task, observation: Observation | None,
             active_run: bool = False) -> TaskState:
    """`TaskState`, computed. Never stored, never written.

    Order is precedence, not preference: a completed task whose issue was
    then closed reads CLOSED, and a task with a live run reads RUNNING
    whatever its approval says.

    `schema.py` computes the same thing as a SQL view. The duplication is
    deliberate and the test suite holds the two to agreeing -- a view is
    what makes the invariant structural in the store, and this is what
    makes it available to code that has a `Task` in hand and no database.
    """
    if observation is not None:
        if observation.closed_at is not None:
            return TaskState.CLOSED
        if observation.completed_at is not None:
            return TaskState.COMPLETED
        if observation.pending_question_comment_id is not None:
            return TaskState.AWAITING_REPLY
    if active_run:
        return TaskState.RUNNING
    if task.approval is None:
        return TaskState.PROPOSED
    return TaskState.QUEUED


def is_blocked(task: Task, closed: frozenset[str]) -> bool:
    """Whether any blocking link names a task that has not closed.

    `closed` is the set of task ids known closed -- passed in rather than
    looked up, because whether a dependency is still open changes with
    nothing about this task changing, which is why this is re-evaluated
    every cycle rather than pinned at dispatch.
    """
    return any(
        link.target not in closed for link in task.links if link.blocks
    )


def branch_name(task_id: str) -> str:
    """The branch a task's work goes on -- derived, never stored and never
    self-reported. Depends only on the id, so nothing has to agree with
    anything: any two callers compute the same name.
    """
    return f"grain/task-{task_id}"


def lease_expired(lease: Lease, now: datetime,
                  max_age: timedelta | None = None) -> bool:
    """Whether `lease` is past its own expiry, or past an unconditional
    backstop. The backstop exists because materialisation has a window
    that cannot be closed: a failure between minting and recording leaks a
    credential nothing knows to revoke.
    """
    if lease.expires_at is not None and now >= lease.expires_at:
        return True
    if max_age is not None and now - lease.issued_at >= max_age:
        return True
    return False
