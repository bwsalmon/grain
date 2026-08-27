"""The automation loop's tunables: which repos, which labels, how fast.

Same loading shape as `Allowlist`/`CredentialSet` — a small JSON file under
`/data/config`, re-read rather than watched, since these values change by an
operator editing a file and restarting the timer, not at runtime.

**Two different repo roles live here**, since the orchestrator stopped
being a single-repo thing: `task_owner`/`task_repo` name the *task repo* —
the one queue polled for labelled issues, where every label, question
comment and human reply lives — while the *target* repo (the one actually
cloned, pushed to, and opened a PR against) is named per-task by a `/repo`
directive in the issue itself (`directives.py`). `default_target_repo`
covers the deployment where those are the same repo: set it and an issue
with no `/repo` line still dispatches, which is exactly what a single-repo
deployment written before the split looks like.

Which target repos are permitted at all is *not* configured here: it is
`/data/config/repo-allowlist.json`, the same operator-owned, hot-reloaded
file the git proxy already enforces (`grain/proxy/allowlist.py`). One
source of truth for "which repos may this deployment touch", checked on
both the API side (`core.py`) and the git-transport side (the proxy),
rather than two lists that can disagree.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

# Keys an older `/data/config/automation.json` may still carry. `owner`/
# `repo` predate the task/target split and meant exactly what `task_owner`/
# `task_repo` mean now; `base_branch` was the single global PR base, which
# no longer has a coherent meaning across many target repos (each one's own
# default branch is read from GitHub instead — see `core.py`'s
# `_resolve_target`). Accepted on load rather than rejected, so pointing an
# already-deployed controller at a new version doesn't need `/data` edited
# first.
_LEGACY_ALIASES = {"owner": "task_owner", "repo": "task_repo"}
_LEGACY_DROPPED = ("base_branch",)


@dataclass(frozen=True)
class AutomationConfig:
    # The task repo: the one repo polled for labelled issues, and the only
    # repo this deployment ever writes labels or comments to. Never
    # necessarily the repo the work happens in — see the module docstring.
    task_owner: str
    task_repo: str
    # Used as the target repo for a task that names none. `None` (the
    # default) makes `/repo` mandatory: an issue without one is parked with
    # a comment rather than guessed at. A single-repo deployment sets this
    # to its own repo and never writes a directive at all.
    default_target_repo: str | None = None
    # The prompt-injection gate from docs/design.md: only issues a human
    # has already labelled are ever picked up. One label, and only on the
    # task repo — a PR-continuation task (docs/roadmap.md item 9) is a task
    # issue carrying a `/pr` directive, not a labelled PR in some other
    # repo, so there is still exactly one trigger surface to watch.
    trigger_label: str = "grain-agent"
    in_progress_label: str = "grain-agent-in-progress"
    # Applied instead of in_progress_label once an `ask_question` call is
    # relayed (docs/roadmap.md item 13), and equally when a task is parked
    # for an unusable `/repo` directive -- visible on GitHub itself, the
    # same way trigger_label/in_progress_label already are, so an operator
    # can see at a glance which issues are idle waiting for a human versus
    # genuinely untouched. Removed the moment a trusted reply promotes the
    # issue back to trigger_label.
    awaiting_reply_label: str = "grain-agent-awaiting-reply"
    # Applied once a task's work is done from the agent's own side --
    # a PR opened (or continued) for a fresh-branch/PR-continuation task, or
    # an analysis posted -- regardless of whether the task issue itself
    # closes automatically (bwsalmon/agents#54: an analysis never does; a
    # fresh-branch task now waits for its PR to close, see
    # `state.py`'s `OpenPullRequest`). Never removed once applied -- unlike
    # the other labels above, this one isn't part of the dispatch queue
    # state machine, just a visible marker of "the agent's part is done."
    completed_label: str = "grain-agent-completed"
    # bwsalmon/agents#83: applied instead of `trigger_label` to a fix task
    # `core.py`'s `_suggest_fix` files for a task whose PR has conflicts
    # with its base or a failing check -- so it sits in the queue visibly,
    # but `_dispatch`'s own poll (which only ever lists `trigger_label`)
    # never picks it up on its own. A human applies `trigger_label` to it
    # once they're satisfied the fix is worth attempting, the same action
    # that starts every other task -- or, equivalently, comments `/lgtm`
    # on it (bwsalmon/agents#136, `core.py`'s `_promote_lgtm_comments`);
    # `_dispatch` then strips this label off on the same cycle it
    # dispatches, the same "exactly one state at a time" invariant
    # `labels.py`'s own docstring holds every other state label to.
    # bwsalmon/agents#175: a task an agent itself proposed via the
    # `propose_task` tool (`core.py`'s `_file_proposed_tasks`) carries this
    # same label for the same reason -- grain suggesting the work is not
    # the same as a human wanting it attempted.
    needs_approval_label: str = "grain-agent-needs-approval"
    # Asks for a short-lived Gemini API key for the task it's applied to
    # (bwsalmon/agents#47), minted and placed in its sandbox, revoked once
    # the task's slot frees. A label, not a body directive
    # (bwsalmon/agents#49, `directives.py`'s own docstring on why) -- the
    # same "a human decided this" trust tier the trigger label carries,
    # checked directly against `issue.labels` in `core.py`'s
    # `_resolve_target` rather than parsed out of untrusted issue text.
    gemini_key_label: str = "grain-gemini-key"
    # bwsalmon/agents#62: read-only access to grain's own controller logs
    # (grain-automation.service, grain-git-proxy.service), via the
    # `read_grain_logs` MCP tool -- for triaging a bug in grain itself,
    # not the target repo's code. The same "a human decided this" label
    # tier `gemini_key_label` already carries, checked directly against
    # `issue.labels` in `core.py`'s `_resolve_target`. Unlike
    # `gemini_key_label` this needs no per-deployment config to turn on:
    # the controller-side group grant (provision/controller.sh) is
    # unconditional, so the label alone is enough.
    self_debug_label: str = "grain-self-debug"
    # bwsalmon/agents#99: the mutating counterpart to `self_debug_label`,
    # deliberately a second label rather than folded into it -- gates
    # `restart_grain_service`/`reboot_sandbox`/`reformat_sandbox`/
    # `reboot_controller` (`mcp_server.py`'s self-repair roster) via the
    # `--self-repair` flag `dispatch.py` only ever passes when a task's
    # issue carries this label. Like `self_debug_label`, this never needs
    # refusing for lack of deployment config -- the sudo grant that makes
    # it work (`provision/controller.sh`) is unconditional, same as the
    # `systemd-journal` group membership `self_debug_label` relies on.
    self_repair_label: str = "grain-self-repair"
    # bwsalmon/agents#159: routes a task straight into its own sandbox's
    # dedicated scratch repo (`scratch_repo.py`'s `repo_for_sandbox`),
    # overriding any `/repo` directive entirely -- which scratch repo that
    # is can't be known until a sandbox is actually assigned, so it can't
    # be a directive a task author writes in advance the way `/repo`
    # normally is. The same "a human decided this" label tier
    # `gemini_key_label` already carries, checked directly against
    # `issue.labels` in `core.py`'s `_resolve_target`, and refused the
    # same way when this deployment has no `scratch_repo_config` (`grain
    # controller configure --scratch-repo-owner ...`) to honour it with.
    scratch_repo_label: str = "grain-scratch-repo"
    ssh_user: str = "debian"
    ssh_key_path: Path = Path("/data/secrets/controller-ssh")
    runs_per_hour: int = 60
    max_runtime_minutes: int = 120
    # Overrides RealTransport's/RealForwarder's host+scheme -- unset in
    # every real deployment, set only to point a live test at a mock GitHub
    # server instead of the real thing (docs/roadmap.md item 8).
    # `github_host` is the REST API (real default "api.github.com"),
    # `git_forward_host` is the git-proxy's smart-HTTP forward target (real
    # default "github.com") -- genuinely different hosts in production, so
    # kept as separate fields even though a mock run typically points both
    # at the same address. `github_use_tls` applies to both connections --
    # a single shared toggle, since nothing about this project ever mixes
    # "mock one, real the other."
    github_host: str = "api.github.com"
    git_forward_host: str = "github.com"
    github_use_tls: bool = True

    @classmethod
    def load(cls, path: Path) -> "AutomationConfig":
        raw = json.loads(path.read_text())
        for old, new in _LEGACY_ALIASES.items():
            if old in raw and new not in raw:
                raw[new] = raw.pop(old)
            else:
                raw.pop(old, None)
        for key in _LEGACY_DROPPED:
            raw.pop(key, None)
        if "ssh_key_path" in raw:
            raw["ssh_key_path"] = Path(raw["ssh_key_path"])
        return cls(**raw)
