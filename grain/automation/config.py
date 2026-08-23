"""The automation loop's tunables: which repo, which labels, how fast.

Same loading shape as `Allowlist`/`CredentialSet` — a small JSON file under
`/data/config`, re-read rather than watched, since these values change by an
operator editing a file and restarting the timer, not at runtime.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class AutomationConfig:
    owner: str
    repo: str
    # The prompt-injection gate from docs/design.md: only issues — and,
    # since docs/roadmap.md item 9, only pull requests — a human has already
    # labelled are ever picked up. One label, one human-gate meaning, for
    # both trigger kinds; see core.py's module docstring for why a PR needs
    # no separate label.
    trigger_label: str = "grain-agent"
    in_progress_label: str = "grain-agent-in-progress"
    # Applied instead of in_progress_label once an `ask_question` call is
    # relayed (docs/roadmap.md item 13) -- visible on GitHub itself, the
    # same way trigger_label/in_progress_label already are, so an operator
    # can see at a glance which issues are idle waiting for a human versus
    # genuinely untouched. Removed the moment a trusted reply promotes the
    # issue back to trigger_label.
    awaiting_reply_label: str = "grain-agent-awaiting-reply"
    # The PR base branch — the target repo's own default, almost always,
    # but not assumed: `git`'s "default branch" concept lives in the repo,
    # not here, and `core.py` has no other source for it since it never
    # clones anything itself.
    base_branch: str = "main"
    ssh_user: str = "debian"
    ssh_key_path: Path = Path("/data/secrets/controller-ssh")
    runs_per_hour: int = 10
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
        if "ssh_key_path" in raw:
            raw["ssh_key_path"] = Path(raw["ssh_key_path"])
        return cls(**raw)
