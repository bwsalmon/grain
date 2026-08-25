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
from grain.automation.labels import Label, main, task_labels

ROOT = Path(__file__).resolve().parent.parent

# GitHub rejects a longer one outright, which would fail the label at
# create time rather than at review time.
_MAX_DESCRIPTION = 100


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


def test_the_six_labels_are_the_ones_the_deployment_actually_runs_on():
    """Names spelled out once, against the defaults the README, runbook
    and design docs all quote -- so a rename has to be deliberate."""
    assert [label.name for label in task_labels()] == [
        "grain-agent",
        "grain-agent-in-progress",
        "grain-agent-awaiting-reply",
        "grain-agent-completed",
        "grain-gemini-key",
        "grain-self-debug",
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
