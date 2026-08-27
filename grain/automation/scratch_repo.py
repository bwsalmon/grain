"""Names the dedicated test repo for a sandbox -- bwsalmon/agents#159's
scratch-repo-for-testing-grain feature: one repo per sandbox slot
(`repo_for_sandbox`), so a task that opts in (`config.scratch_repo_label`,
`core.py`'s `_resolve_target`) always lands in a repo nothing else ever
touches.

**No credential lives here.** An earlier version of this feature
(bwsalmon/agents#159) minted its own short-lived GitHub App installation
tokens, one per scratch repo, to keep the credential no broader than the
single repo it covered. That bought tight scoping at the cost of real
complexity: a GitHub App, a JWT signed by shelling out to `openssl`, and
two independent minting call sites (`cli.py`'s orchestrator and the git
proxy) that each had to re-derive a fresh token on their own clock.

bwsalmon/agents#186 traded that scoping for simplicity: a single personal
access token with elevated permissions, covering every scratch repo,
placed the same way any other named GitHub credential is
(`grain controller configure --github-key <name>=PATH`, see
`configure.py`'s `configure_named_github_key`) and reached the same way
too -- an `owner/*` (or narrower) pattern an operator adds to
`credentials.json` by hand, the same "deliberate operator edit, this
project doesn't guess at widening its own grant" convention
`configure_github_credential`'s own docstring already describes for any
credential broader than one exact repo. `grain/proxy/credentials.py`'s
`CredentialSet` already resolves that pattern for both the git proxy's own
push and the orchestrator's API calls (`cli.py`'s `build_orchestrator`) --
nothing scratch-repo-specific needs to exist in either place any more.

What's left here is purely naming: which repo a given sandbox's scratch
task should land in, computed deterministically so `core.py` can pick it
the moment a sandbox is assigned, before ever making a GitHub call.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

DEFAULT_REPO_PREFIX = "grain-scratch"


@dataclass(frozen=True)
class ScratchRepoConfig:
    """Operator-set tunables -- the same JSON-file-under-`/data/config`
    shape as `GcpKeyConfig`/`GeminiKeyConfig`. Its presence (or absence) at
    `/data/config/scratch-repo.json` is the on/off switch for
    `config.scratch_repo_label`: `cli.py`'s `build_orchestrator` leaves
    `Orchestrator.scratch_repo_config` `None` when the file is absent,
    which makes `core.py`'s `_resolve_target` refuse the label with an
    explanation, the same "feature not configured" shape `gemini_key_config`
    already has. Plain, non-secret configuration -- no credential of any
    kind lives on this dataclass; see this module's own docstring for where
    the credential actually comes from.
    """

    # The account the scratch repos live under -- never guessed from a
    # task's own directives (a scratch-repo task carries no `/repo` line
    # at all; see `core.py`'s `_resolve_target`).
    owner: str
    repo_prefix: str = DEFAULT_REPO_PREFIX

    @classmethod
    def load(cls, path: Path) -> "ScratchRepoConfig":
        return cls(**json.loads(path.read_text()))


def repo_for_sandbox(config: ScratchRepoConfig, sandbox: str) -> str:
    """The one repo dedicated to `sandbox` -- deterministic, so `core.py`
    can pick a scratch task's target the moment a sandbox is assigned,
    before anything is ever read from GitHub.
    """
    return f"{config.repo_prefix}-{sandbox}"
