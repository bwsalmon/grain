"""Every label the task queue runs on, in one table.

The labels themselves are `AutomationConfig`'s business -- `trigger_label`
and friends are what `core.py` reads off an issue and what `dispatch.py`
swaps as a task moves -- but *creating* them is not something the
controller can do: it only ever labels issues that already exist, and a
label a repo has never seen cannot be applied by a human either, so it
never reaches the queue at all. Somebody has to put them in the config
repo's label picker first, and the only thing that runs there is that
repo's own deploy workflow.

**Why the list lives here and not in that workflow.** The workflow is in
the *config* repo -- one per deployment, forked from
`templates/gcp/` and then owned by whoever runs it. A label list
written there is a copy, and a copy has to be re-synced by hand into
every fork the day a label is added; the label added in bwsalmon/agents#47
(`gemini_key_label`) and the two before it (`completed_label`,
`self_debug_label`) all shipped without any config repo learning about
them, which is exactly that failure. So the list is here, next to the
config that names the labels, and `ci/ensure-task-labels.sh` applies it
from the grain checkout the workflow already makes -- the same "grain is
the single source, the config repo pulls it fresh at the pinned ref"
split that stopped `templates/gcp/` vendoring the Terraform module.

**Defaults, not a deployment's overrides.** `task_labels()` with no
argument reads `AutomationConfig`'s *class* defaults rather than a live
config, because the workflow that calls this runs on a GitHub runner and
has no access to `/data/config/automation.json` -- that file lives on the
host. A deployment that renames a label there is therefore renaming it
only for the controller, and owns creating the renamed label itself. That
is the pre-existing behaviour of the hardcoded list this replaced, kept
deliberately: CI creating labels a deployment never asked for is a worse
failure than CI creating the stock ones.

**Colour and description are presentation, so they are not on
`AutomationConfig`.** An operator renames a label; they do not restyle
it. Keeping the styling in this table means `AutomationConfig` stays what
it is -- the automation loop's tunables -- and gains no field the loop
itself never reads.

**The palette is two tiers, and that is the whole point of it.** A task
issue is in exactly one *state* at a time, and the reason to look at the
queue at all is usually to see which -- so the state labels are dark
and saturated, which GitHub renders as a solid pill that carries down a
list of issues at a glance. The *capability* labels are not states: they
are opt-in modifiers that a human adds to one task, they say nothing
about where that task has got to, and a task can carry both at once. They
are pale, so they read as an annotation on the row rather than competing
with the state pill next to them. `Label.kind` records which tier a label
is in, and tests/test_automation_labels.py holds the two apart by
lightness, so a capability label added in a loud colour fails the suite
rather than quietly eroding the scan.

Within the state tier the hues follow the lifecycle and the ordinary
meaning of the colours, rather than the order they were added in: blue is
queued and not yet started, amber is running now, red is stopped and
waiting on a *human* (the only state that needs somebody to do
something), and green is done. That green means finished and not
"approved to start" is the one deliberate break with what these labels
used to be coloured -- `grain-agent` was green because it is the label
you apply to start a task, which read as a success colour on every issue
that had not actually finished anything.
"""

from __future__ import annotations

from dataclasses import dataclass

from .config import AutomationConfig

STATE = "state"
CAPABILITY = "capability"

# Keyed by the `AutomationConfig` field that names the label, never by the
# label string itself: that is what makes a renamed default follow through
# to the workflow automatically, and what lets `task_labels` accept a
# real config for the callers that have one. Ordered by lifecycle, which
# is also the order the colours run through -- see the module docstring on
# why the two tiers look as different as they do.
_STYLES: dict[str, tuple[str, str, str]] = {
    # Queued: labelled by a human, not yet claimed by a polling pass.
    "trigger_label": (
        STATE, "1d76db", "Hand this task to the agent set",
    ),
    # Running right now, in a claimed sandbox.
    "in_progress_label": (
        STATE, "fbca04", "An agent is working on this",
    ),
    # The one state that is waiting on a person -- loudest on purpose.
    "awaiting_reply_label": (
        STATE, "d93f0b", "Parked: the agent needs a human reply",
    ),
    # bwsalmon/agents#83: also waiting on a person, same reason
    # awaiting_reply_label above is red -- grain filed this task itself (a
    # fix for a conflicting or failing PR) and it must not be picked up
    # until a human applies trigger_label to say the fix is worth
    # attempting. A distinct shade of red, not the same one: the two are
    # different states (a stalled *existing* task vs. a *new* one grain is
    # asking permission to run) and `test_no_two_labels_share_a_colour`
    # holds every label to a colour of its own regardless.
    "needs_approval_label": (
        STATE, "b60205", "Grain suggested this task -- apply the trigger label to run it",
    ),
    # Terminal, and green in the sense every CI badge is green.
    "completed_label": (
        STATE, "0e8a16", "The agent's side is done: a PR is open, or an analysis is posted",
    ),
    # Modifiers, not states: pale, so a row's state still reads first.
    "gemini_key_label": (
        CAPABILITY, "d4c5f9", "Mint a short-lived Gemini API key for this task",
    ),
    "self_debug_label": (
        CAPABILITY, "c5def5", "Let this task read grain's own controller logs",
    ),
    # bwsalmon/agents#99: the mutating counterpart to self_debug_label
    # above, kept as its own row/colour rather than reusing self-debug's --
    # `test_no_two_labels_share_a_colour` holds every label in this table
    # to a colour of its own regardless, and the two are genuinely
    # different grants (read-only vs. restart/reboot/reformat).
    "self_repair_label": (
        CAPABILITY, "fef2c0", "Let this task restart grain services or reboot/reformat its sandbox",
    ),
}


@dataclass(frozen=True)
class Label:
    name: str
    color: str
    description: str
    # STATE or CAPABILITY -- which tier of the palette this belongs to.
    # Not sent to GitHub (a label has no such concept); it is what keeps
    # the two tiers honest, here and in the tests.
    kind: str


def task_labels(config: AutomationConfig | None = None) -> list[Label]:
    """The label set to create on the task repo. `None` (what CI passes)
    means `AutomationConfig`'s defaults -- see the module docstring on why
    a runner never reads a deployment's own overrides.
    """
    labels = []
    for field, (kind, color, description) in _STYLES.items():
        if config is not None:
            name = getattr(config, field)
        else:
            name = AutomationConfig.__dataclass_fields__[field].default
        labels.append(Label(name=name, color=color, description=description, kind=kind))
    return labels


def agent_label(sandbox: str) -> str:
    """The label that names *which* sandbox is currently working a task --
    e.g. `"sandbox-1"` becomes `"grain-agent-1"` -- applied alongside
    `in_progress_label` the moment a task is dispatched (`core.py`'s
    `_dispatch`), re-applied every `run_once` cycle for as long as the
    assignment lasts (`_refresh_agent_labels`) so a label knocked off by
    hand, or lost to an API call that failed partway, heals on the very
    next cycle rather than staying wrong for the rest of the run, and
    removed the moment the assignment ends, however it ends (bwsalmon/agents#95).

    Deliberately not one of `_STYLES`/`task_labels()`: those are the fixed,
    deployment-wide set `ci/ensure-task-labels.sh` creates ahead of time
    from `AutomationConfig`'s defaults, but there is one of *these* per
    sandbox, and how many sandboxes a deployment runs (`Cluster.sandbox_count`)
    is host-side config a GitHub runner never has access to -- the same gap
    the module docstring above already describes for why `task_labels()`
    can't read a live config either. Nothing needs to pre-create it: GitHub
    itself creates any label named in an "add labels" call that doesn't
    already exist on the repo, with a default colour, exactly the way one
    typed into the label picker by hand would be.
    """
    return f"grain-agent-{sandbox.removeprefix('sandbox-')}"


def main() -> int:
    """`python3 -m grain.automation.labels` -- one tab-separated row per
    label, which is what `ci/ensure-task-labels.sh` loops over. Tab, not
    JSON, so the shell side stays a `read -r` and needs no parser on a
    runner that has only stock python.
    """
    for label in task_labels():
        print(f"{label.name}\t{label.color}\t{label.description}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
