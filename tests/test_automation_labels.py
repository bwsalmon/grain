"""The label table the config repo's deploy workflow creates from.

The property worth pinning is coverage: a label `AutomationConfig` names
but `labels.py` omits is one no repo ever puts in its picker, which makes
the feature behind it unreachable rather than merely unstyled -- how
`grain-agent-completed`, `grain-self-debug` and `grain-gemini-key` each
shipped without a picker entry. So the first test below derives the
expected set from `AutomationConfig`'s own fields rather than restating
it, and fails the day a seventh label lands without a row here.
"""

import subprocess
import sys
from dataclasses import fields
from pathlib import Path

from grain.automation.config import AutomationConfig
from grain.automation.labels import CAPABILITY, STATE, Label, agent_label, main, task_labels

ROOT = Path(__file__).resolve().parent.parent

# GitHub rejects a longer one outright, which would fail the label at
# create time rather than at review time.
_MAX_DESCRIPTION = 100

# The two tiers are held apart by HSL lightness: a state label is a dark,
# solid pill that carries down a list of issues, a capability label is
# pale enough to read as an annotation beside it. The real palette sits at
# 0.50 and 0.87, so these bounds are loose -- they exist to catch a
# capability label picked in a loud colour (or the reverse), not to police
# the exact shade.
_STATE_MAX_LIGHTNESS = 0.60
_CAPABILITY_MIN_LIGHTNESS = 0.75


def _lightness(color: str) -> float:
    """HSL lightness of a six-digit hex colour, 0 (black) to 1 (white)."""
    r, g, b = (int(color[i:i + 2], 16) / 255 for i in (0, 2, 4))
    return (max(r, g, b) + min(r, g, b)) / 2


def _label_fields() -> set[str]:
    return {f.name for f in fields(AutomationConfig) if f.name.endswith("_label")}


def test_every_label_automation_config_names_has_a_row():
    """The whole point of the module: no label the automation knows about
    may be missing from the set CI creates."""
    expected = {
        AutomationConfig.__dataclass_fields__[name].default
        for name in _label_fields()
    }
    assert {label.name for label in task_labels()} == expected


def test_the_eight_labels_are_the_ones_the_deployment_actually_runs_on():
    """Names spelled out once, against the defaults the README, runbook
    and design docs all quote -- so a rename has to be deliberate."""
    assert [label.name for label in task_labels()] == [
        "grain-agent",
        "grain-agent-in-progress",
        "grain-agent-awaiting-reply",
        "grain-agent-needs-approval",
        "grain-agent-completed",
        "grain-gemini-key",
        "grain-self-debug",
        "grain-self-repair",
    ]


def test_a_renamed_label_follows_through_to_the_created_set():
    """A caller that has a real config gets that config's names -- the
    table is keyed by `AutomationConfig` field, not by label string."""
    config = AutomationConfig(
        task_owner="an-org", task_repo="a-repo",
        trigger_label="do-it", gemini_key_label="needs-gemini",
    )
    names = {label.name for label in task_labels(config)}
    assert "do-it" in names and "needs-gemini" in names
    assert "grain-agent" not in names
    # Untouched fields still come from the defaults.
    assert "grain-agent-in-progress" in names


def test_no_config_means_the_defaults_not_an_error():
    """What CI passes: a runner has no /data/config/automation.json to
    read, so the class defaults are the answer rather than a failure."""
    assert task_labels() == task_labels(None)
    assert all(isinstance(label, Label) for label in task_labels())


def test_every_row_is_something_github_will_accept():
    labels = task_labels()
    assert len({label.name for label in labels}) == len(labels), "duplicate label name"
    for label in labels:
        assert len(label.color) == 6 and all(c in "0123456789abcdef" for c in label.color), \
            f"{label.name}: {label.color!r} is not a six-digit hex colour"
        assert label.description, f"{label.name} has no description"
        assert len(label.description) <= _MAX_DESCRIPTION, \
            f"{label.name}: GitHub rejects a description over {_MAX_DESCRIPTION} chars"


def test_no_field_carries_a_tab_that_would_break_the_shell_protocol():
    """`ensure-task-labels.sh` reads this table as TSV -- a tab anywhere in
    a name or description would silently shift a column and mint a label
    with the wrong colour."""
    for label in task_labels():
        for value in (label.name, label.color, label.description):
            assert "\t" not in value and "\n" not in value, f"{label.name}: {value!r}"


def test_the_module_prints_one_parseable_row_per_label():
    """The exact interface the shell script consumes, run the way it runs
    it -- `python3 -m`, from grain's root, with nothing installed."""
    result = subprocess.run(
        [sys.executable, "-m", "grain.automation.labels"],
        cwd=ROOT, capture_output=True, text=True, check=True,
    )
    rows = [line.split("\t") for line in result.stdout.splitlines()]
    assert rows == [[l.name, l.color, l.description] for l in task_labels()]


def test_main_returns_success():
    assert main() == 0


def test_every_label_is_in_one_of_the_two_tiers():
    assert {label.kind for label in task_labels()} <= {STATE, CAPABILITY}


def test_the_state_tier_is_exactly_the_dispatch_state_machine():
    """A task is in exactly one of these at a time, which is what makes
    colouring them worth doing. The capability labels are modifiers -- a
    task can carry both, and neither says where it has got to."""
    by_kind = {}
    for label in task_labels():
        by_kind.setdefault(label.kind, []).append(label.name)
    assert by_kind[STATE] == [
        "grain-agent",
        "grain-agent-in-progress",
        "grain-agent-awaiting-reply",
        "grain-agent-needs-approval",
        "grain-agent-completed",
    ]
    assert by_kind[CAPABILITY] == ["grain-gemini-key", "grain-self-debug", "grain-self-repair"]


def test_state_labels_are_solid_and_capability_labels_are_pale():
    """The property the palette exists for: state reads first when you
    scan the queue, and an opt-in modifier never out-shouts it."""
    for label in task_labels():
        lightness = _lightness(label.color)
        if label.kind == STATE:
            assert lightness <= _STATE_MAX_LIGHTNESS, (
                f"{label.name} (#{label.color}) is too pale for a state label: "
                f"lightness {lightness:.2f} > {_STATE_MAX_LIGHTNESS}"
            )
        else:
            assert lightness >= _CAPABILITY_MIN_LIGHTNESS, (
                f"{label.name} (#{label.color}) is too loud for a capability "
                f"label: lightness {lightness:.2f} < {_CAPABILITY_MIN_LIGHTNESS}, "
                "which competes with the state pill next to it"
            )


def test_the_two_tiers_do_not_overlap_in_lightness():
    """Not implied by the bounds above: both could drift toward the middle
    and still pass individually, leaving nothing to see at a glance."""
    lightness = {kind: [_lightness(l.color) for l in task_labels() if l.kind == kind]
                 for kind in (STATE, CAPABILITY)}
    assert max(lightness[STATE]) < min(lightness[CAPABILITY])


def test_no_two_labels_share_a_colour():
    """Two labels the same colour is two states that look like one."""
    colors = [label.color for label in task_labels()]
    assert len(set(colors)) == len(colors), f"duplicate colour among {colors}"


def test_agent_label_names_the_sandbox_by_index():
    """bwsalmon/agents#95: `agent_label` is what `core.py` puts on a task
    issue to say which sandbox is working it -- named after the sandbox's
    own index, not its full `sandbox-N` name, so the label reads as a short
    "agent N" tag rather than repeating "sandbox" on every row. `working`,
    not a bare number (bwsalmon/agents#101), so it doesn't read as one more
    row of the `grain-agent-*` state family."""
    assert agent_label("sandbox-0") == "grain-agent-working-0"
    assert agent_label("sandbox-1") == "grain-agent-working-1"


def test_agent_label_is_not_one_of_the_pre_created_rows():
    """Deliberately excluded from `task_labels()` -- there is one of these
    per sandbox, and how many sandboxes a deployment runs isn't something
    `ci/ensure-task-labels.sh` (a GitHub runner, no access to the host's
    `Cluster` config) can know ahead of time. GitHub creates it itself the
    first time `core.py` applies it to an issue."""
    names = {label.name for label in task_labels()}
    assert agent_label("sandbox-0") not in names
    assert agent_label("sandbox-1") not in names
