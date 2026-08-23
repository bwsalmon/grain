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
import secrets
from pathlib import PurePosixPath

from ..run import Runner

DATA_CONFIG = "/data/config"
DATA_SECRETS_GITHUB = "/data/secrets/github"
CLAUDE_CREDENTIALS_PATH = "/data/secrets/claude-credentials.json"
SANDBOX_TOKENS_PATH = "/data/secrets/sandbox-tokens.json"
# Where the live copy `claude -p` actually reads goes now that it runs on
# the controller as the dedicated `grain-agent` account (docs/roadmap.md
# item 8's "Update", provision/controller.sh) instead of on a sandbox.
_AGENT_CREDENTIALS_DIR = "/home/grain-agent/.claude"
_AGENT_CREDENTIALS_PATH = f"{_AGENT_CREDENTIALS_DIR}/.credentials.json"


def _write_remote_file(runner: Runner, path: str, content: str, *, mode: str,
                        owner: str | None = None) -> None:
    parent = str(PurePosixPath(path).parent)
    runner.run(["sudo", "mkdir", "-p", parent])
    runner.run(["sudo", "dd", f"of={path}", "status=none"], stdin=content)
    runner.run(["sudo", "chmod", mode, path])
    if owner is not None:
        runner.run(["sudo", "chown", f"{owner}:{owner}", path])
        # `mkdir -p` above runs as root, so a freshly-created parent (e.g.
        # ~/.claude, the first time a credential is placed) would otherwise
        # stay root-owned under an account that needs to write other things
        # there itself later (Claude Code's own settings/session state).
        runner.run(["sudo", "chown", f"{owner}:{owner}", parent])


def configure_repo(runner: Runner, owner: str, repo: str, *,
                    github_host: str = "api.github.com", git_forward_host: str = "github.com",
                    github_use_tls: bool = True) -> None:
    """Writes `/data/config/automation.json` and `repo-allowlist.json`
    (docs/runbook.md steps 9 and 11) -- both derivable from the same
    `owner/repo` this command already takes (docs/bootstrap.md).

    `github_host`/`git_forward_host`/`github_use_tls` default to the real
    GitHub and only ever differ for a live test pointed at a mock server
    instead (docs/roadmap.md item 8) -- see `RealTransport` and
    `RealForwarder`. Always written explicitly, matching the rest of this
    file's config, rather than omitted when they equal the default.
    """
    automation_json = json.dumps({
        "owner": owner, "repo": repo,
        "github_host": github_host, "git_forward_host": git_forward_host,
        "github_use_tls": github_use_tls,
    }, indent=2) + "\n"
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
    """Places a Claude Code OAuth credential on the controller -- one login
    placed once, not one per sandbox (docs/bootstrap.md, "The Claude
    credential"). Two copies, both written here now (docs/roadmap.md item
    8's "Update" folded the old per-sandbox injection loop, formerly in
    `grain/bootstrap.py` stage 9, into this single controller-side step,
    since `claude -p` runs on the controller now and no sandbox needs any
    Claude credential at all):

    - `CLAUDE_CREDENTIALS_PATH`, a root-owned reference copy under
      `/data/secrets`, matching every other credential this module places;
    - `_AGENT_CREDENTIALS_PATH`, the live copy `claude -p` actually reads,
      owned by the dedicated `grain-agent` account it runs as
      (`provision/controller.sh`) rather than root.
    """
    _write_remote_file(runner, CLAUDE_CREDENTIALS_PATH, credentials_json, mode="600")
    _write_remote_file(runner, _AGENT_CREDENTIALS_PATH, credentials_json, mode="600",
                        owner="grain-agent")


def ensure_sandbox_tokens(runner: Runner, sandbox_names: list[str]) -> None:
    """Mints a git-proxy bearer token for every name in `sandbox_names` that
    doesn't already have one, merging into the existing file rather than
    overwriting it (an already-dispatched sandbox's token must survive a
    bootstrap re-run).

    Found live: `grain/proxy/tokens.py`'s own `SandboxTokenStore.ensure_token`
    is documented as "safe to call unconditionally before every dispatch,"
    and it is -- but `SandboxTokens` (the *proxy's* read side) loads
    `sandbox-tokens.json` once at process start
    (`grain/proxy/server.py`'s `build_proxy`) and never again
    (docs/design.md, README: "secrets/ is not [hot-reloaded]... a running
    process keeps using the old token until restarted"). Bootstrap's stage
    10 enables `grain-git-proxy.service` before any sandbox has ever been
    dispatched to, so on a fresh deployment the proxy always starts with an
    empty token file -- the very first real dispatch to any sandbox then
    fails with `401`/"Authentication failed" against the proxy, no matter
    how correctly everything else converged, until an operator happens to
    restart the proxy afterward. Minting every sandbox's token here, before
    stage 10, closes that gap: the proxy's first-ever load already has the
    full set, and `ensure_token`'s later calls from `dispatch.py` are then
    genuinely idempotent no-ops, as documented.
    """
    result = runner.run(["sudo", "cat", SANDBOX_TOKENS_PATH], check=False)
    tokens: dict[str, str] = (
        json.loads(result.stdout)
        if result.returncode == 0 and result.stdout.strip()
        else {}
    )
    changed = False
    for name in sandbox_names:
        if name not in tokens:
            tokens[name] = secrets.token_hex(32)
            changed = True
    if changed:
        _write_remote_file(
            runner, SANDBOX_TOKENS_PATH, json.dumps(tokens, indent=2) + "\n", mode="644",
        )
