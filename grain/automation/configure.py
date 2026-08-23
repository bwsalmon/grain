"""`grain controller configure` -- docs/bootstrap.md Phase 3's third missing
verb. Writes the handful of per-deployment files under `/data` that
`provision/controller.sh` deliberately never does (see its own "what this
does NOT do" comment) -- the credential-and-config placement steps
docs/runbook.md's checklist still needs a human for (steps 8, 9, 11).

Same stdin-not-argv shape `dispatch.configure_git_credentials` already uses
for the sandbox git-proxy token, for the same reason: an argv element lands
in `ps` output and this project's own command logging, and user-data is
ruled out entirely (docs/bootstrap.md: "baked into the seed ISO, which sits
on host disk at rest"). `runner` here is always an admin SSH path to the
controller -- `sudo`, since `/data/config` and `/data/secrets` are root-owned
(`provision/controller.sh`) and the admin SSH user is not.
"""

from __future__ import annotations

import json
from pathlib import PurePosixPath

from ..run import Runner

DATA_CONFIG = "/data/config"
DATA_SECRETS_GITHUB = "/data/secrets/github"
CLAUDE_CREDENTIALS_PATH = "/data/secrets/claude-credentials.json"


def _write_remote_file(runner: Runner, path: str, content: str, *, mode: str) -> None:
    parent = str(PurePosixPath(path).parent)
    runner.run(["sudo", "mkdir", "-p", parent])
    runner.run(["sudo", "dd", f"of={path}", "status=none"], stdin=content)
    runner.run(["sudo", "chmod", mode, path])


def configure_repo(runner: Runner, owner: str, repo: str) -> None:
    """Writes `/data/config/automation.json` and `repo-allowlist.json`
    (docs/runbook.md steps 9 and 11) -- both derivable from the same
    `owner/repo` this command already takes (docs/bootstrap.md).
    """
    automation_json = json.dumps({"owner": owner, "repo": repo}, indent=2) + "\n"
    _write_remote_file(runner, f"{DATA_CONFIG}/automation.json", automation_json, mode="644")
    allowlist_json = json.dumps([f"{owner}/{repo}"], indent=2) + "\n"
    _write_remote_file(runner, f"{DATA_CONFIG}/repo-allowlist.json", allowlist_json, mode="644")


def configure_github_credential(runner: Runner, owner: str, repo: str, token: str,
                                 *, credential_name: str = "bot") -> None:
    """Writes the token file and the `credentials.json` pattern pointing
    `owner/repo` at it (docs/runbook.md step 8). Only ever writes the single
    exact-repo pattern this deployment was given; a broader `owner/*` or `*`
    pattern is a deliberate operator edit made by hand afterward, same as
    today -- this command does not guess at widening its own grant.
    """
    _write_remote_file(
        runner, f"{DATA_SECRETS_GITHUB}/{credential_name}.token",
        token.strip() + "\n", mode="600",
    )
    mapping = {f"{owner}/{repo}": credential_name}
    _write_remote_file(
        runner, f"{DATA_SECRETS_GITHUB}/credentials.json",
        json.dumps(mapping, indent=2) + "\n", mode="644",
    )


def configure_claude_credentials(runner: Runner, credentials_json: str) -> None:
    """Places a Claude Code OAuth credential file on the controller, ready
    for injection into sandboxes (docs/bootstrap.md, "The Claude
    credential") -- one login placed once, not one per sandbox.
    """
    _write_remote_file(runner, CLAUDE_CREDENTIALS_PATH, credentials_json, mode="600")
