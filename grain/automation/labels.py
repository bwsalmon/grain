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
`config-repo-template/` and then owned by whoever runs it. A label list
written there is a copy, and a copy has to be re-synced by hand into
every fork the day a label is added; the label added in bwsalmon/agents#47
(`gemini_key_label`) and the two before it (`completed_label`,
`self_debug_label`) all shipped without any config repo learning about
them, which is exactly that failure. So the list is here, next to the
config that names the labels, and `ci/ensure-task-labels.sh` applies it
from the grain checkout the workflow already makes -- the same "grain is
the single source, the config repo pulls it fresh at the pinned ref"
split that stopped `config-repo-template/` vendoring the Terraform module.

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
"""

from __future__ import annotations

from dataclasses import dataclass

from .config import AutomationConfig

# Keyed by the `AutomationConfig` field that names the label, never by the
# label string itself: that is what makes a renamed default follow through
# to the workflow automatically, and what lets `task_labels` accept a
# real config for the callers that have one.
_STYLES: dict[str, tuple[str, str]] = {
    "trigger_label": (
        "0e8a16", "Hand this task to the agent set",
    ),
    "in_progress_label": (
        "fbca04", "An agent is working on this",
    ),
    "awaiting_reply_label": (
        "d93f0b", "Parked: the agent needs a human reply",
    ),
    "completed_label": (
        "6f42c1", "The agent's side is done: a PR is open, or an analysis is posted",
    ),
    "gemini_key_label": (
        "1d76db", "Mint a short-lived Gemini API key for this task",
    ),
    "self_debug_label": (
        "c5def5", "Let this task read grain's own controller logs",
    ),
}


@dataclass(frozen=True)
class Label:
    name: str
    color: str
    description: str


def task_labels(config: AutomationConfig | None = None) -> list[Label]:
    """The label set to create on the task repo. `None` (what CI passes)
    means `AutomationConfig`'s defaults -- see the module docstring on why
    a runner never reads a deployment's own overrides.
    """
    labels = []
    for field, (color, description) in _STYLES.items():
        if config is not None:
            name = getattr(config, field)
        else:
            name = AutomationConfig.__dataclass_fields__[field].default
        labels.append(Label(name=name, color=color, description=description))
    return labels


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
