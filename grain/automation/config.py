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
    # The prompt-injection gate from docs/design.md: only issues a human has
    # already labelled are ever picked up.
    trigger_label: str = "grain-agent"
    in_progress_label: str = "grain-agent-in-progress"
    ssh_user: str = "debian"
    ssh_key_path: Path = Path("/data/secrets/controller-ssh")
    runs_per_hour: int = 10
    max_runtime_minutes: int = 120

    @classmethod
    def load(cls, path: Path) -> "AutomationConfig":
        raw = json.loads(path.read_text())
        if "ssh_key_path" in raw:
            raw["ssh_key_path"] = Path(raw["ssh_key_path"])
        return cls(**raw)
