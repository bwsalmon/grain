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

from .dispatch import CONTROLLER_AGENT_TOKEN_PATH
from ..run import Runner

DATA_CONFIG = "/data/config"
DATA_SECRETS_GITHUB = "/data/secrets/github"
CLAUDE_TOKEN_PATH = "/data/secrets/claude-oauth-token"
SANDBOX_TOKENS_PATH = "/data/secrets/sandbox-tokens.json"
# Must match grain/metadata/config.py's MetadataConfig.key_path default and
# the "metadata-server.json" name build_launcher's build_launcher() loads --
# this module and that one are never imported into each other (SSH-remote
# writes here, local file I/O there), so the paths can only be kept in sync
# by hand; a rename on one side and not the other is silent until a real
# `grain metadata start` fails to find its config.
GCP_SERVICE_ACCOUNT_KEY_PATH = "/data/secrets/gcp-service-account.json"
METADATA_SERVER_CONFIG_PATH = "/data/config/metadata-server.json"
# Must match grain/automation/gemini_keys.py's `GeminiKeyConfig` load path
# -- same "kept in sync by hand" caveat as the pair above; see this
# constant's own use in `configure_gemini_key`.
GEMINI_KEY_CONFIG_PATH = "/data/config/gemini-key.json"


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


def configure_repo(runner: Runner, task_repo: str, targets: list[str], *,
                    default_target_repo: str | None = None,
                    github_host: str = "api.github.com", git_forward_host: str = "github.com",
                    github_use_tls: bool = True) -> None:
    """Writes `/data/config/automation.json` and `repo-allowlist.json`
    (docs/runbook.md steps 9 and 11).

    Two roles, two arguments. `task_repo` (`"owner/name"`) is the queue the
    orchestrator polls for labelled issues; `targets` are the repos those
    issues may actually dispatch into, named per-task by a `/repo` directive
    (`grain/automation/directives.py`). Only the targets go in the
    allowlist: the allowlist gates *git transport*, and no sandbox ever
    clones the task repo -- the orchestrator reads it over the API, which
    `credentials.json` covers instead (`configure_github_credential`).

    `default_target_repo` is what a task with no `/repo` line gets. A
    single-repo deployment (task repo == the code) passes its own repo for
    all three and never writes a directive at all, which is exactly how
    every deployment behaved before the split.

    `github_host`/`git_forward_host`/`github_use_tls` default to the real
    GitHub and only ever differ for a live test pointed at a mock server
    instead (docs/roadmap.md item 8) -- see `RealTransport` and
    `RealForwarder`. Always written explicitly, matching the rest of this
    file's config, rather than omitted when they equal the default.
    """
    task_owner, _, task_name = task_repo.partition("/")
    automation_json = json.dumps({
        "task_owner": task_owner, "task_repo": task_name,
        "default_target_repo": default_target_repo,
        "github_host": github_host, "git_forward_host": git_forward_host,
        "github_use_tls": github_use_tls,
    }, indent=2) + "\n"
    _write_remote_file(runner, f"{DATA_CONFIG}/automation.json", automation_json, mode="644")
    allowlist_json = json.dumps(list(targets), indent=2) + "\n"
    _write_remote_file(runner, f"{DATA_CONFIG}/repo-allowlist.json", allowlist_json, mode="644")


def credential_repos(task_repo: str, targets: list[str]) -> list[str]:
    """Every repo the orchestrator's own credential has to cover: the task
    repo (read issues, move labels, comment) plus each target repo (check a
    branch, open a PR). Deduplicated, order preserved -- a single-repo
    deployment names the same repo in both roles and must not produce a
    duplicated entry. Lives here rather than at either call site (`grain
    host bootstrap` and `grain controller configure` both need it) so the
    two can't drift.
    """
    return list(dict.fromkeys([task_repo, *targets]))


def configure_github_credential(runner: Runner, repos: list[str], token: str,
                                 *, credential_name: str = "bot") -> None:
    """Writes the token file and the `credentials.json` patterns pointing
    each repo in `repos` at it (docs/runbook.md step 8). Only ever writes
    exact-repo patterns for the repos this deployment was given; a broader
    `owner/*` or `*` pattern is a deliberate operator edit made by hand
    afterward, same as today -- this command does not guess at widening its
    own grant.

    `repos` is the task repo plus every target repo: the orchestrator now
    resolves a credential per repo (`CredentialSet.token_for`, wired into
    `GitHubClient`), so a repo absent from this mapping is one it will talk
    to anonymously and, for anything private, fail on.
    """
    _write_remote_file(
        runner, f"{DATA_SECRETS_GITHUB}/{credential_name}.token",
        token.strip() + "\n", mode="600",
    )
    mapping = {repo: credential_name for repo in repos}
    _write_remote_file(
        runner, f"{DATA_SECRETS_GITHUB}/credentials.json",
        json.dumps(mapping, indent=2) + "\n", mode="644",
    )


def configure_claude_token(runner: Runner, token: str) -> None:
    """Places a Claude Code OAuth token on the controller -- one token
    placed once, not one per sandbox (docs/bootstrap.md, "The Claude
    credential"). Two copies, both written here (docs/roadmap.md item 8's
    "Update" folded the old per-sandbox injection loop, formerly in
    `grain/bootstrap.py` stage 9, into this single controller-side step,
    since `claude -p` runs on the controller now and no sandbox needs any
    Claude credential at all):

    - `CLAUDE_TOKEN_PATH`, a root-owned reference copy under
      `/data/secrets`, matching every other credential this module places;
    - `CONTROLLER_AGENT_TOKEN_PATH`, the live copy `dispatch.py`'s own
      unit script reads into `CLAUDE_CODE_OAUTH_TOKEN` at runtime, owned by
      the dedicated `grain-agent` account it runs as
      (`provision/controller.sh`) rather than root.

    `token` is a bare `claude setup-token` value (e.g. `sk-ant-oat01-...`),
    not a full `~/.claude/.credentials.json` -- deliberately a *different*
    credential from any operator's own interactive `claude login` session,
    kept separate so this deployment's dispatch traffic never rides on a
    personal login (`dispatch.py`'s `CONTROLLER_AGENT_TOKEN_PATH` docstring
    has the full reasoning for the env-var delivery this implies, including
    why it's safe now in a way it wasn't when `claude -p` ran on a sandbox).
    """
    stripped = token.strip()
    _write_remote_file(runner, CLAUDE_TOKEN_PATH, stripped, mode="600")
    _write_remote_file(runner, CONTROLLER_AGENT_TOKEN_PATH, stripped, mode="600",
                        owner="grain-agent")


def configure_gcp_service_account(runner: Runner, key: str, *, service_account_email: str,
                                   project_id: str, numeric_project_id: int = 0,
                                   metadata_user: str = "grain-metadata") -> None:
    """Places the two files `grain metadata start` needs and nothing wrote
    before this (docs/design.md's "GCP credentials", docs/roadmap.md item
    4's remaining gap): the impersonation-source key, owned by
    `metadata_user` so only the metadata service can read it, and
    `MetadataConfig`'s own JSON file naming the narrow account every
    instance impersonates.

    `key` is minted fresh per deploy and never lands in this repo or
    Actions secrets long-lived (config-repo-template's deploy.yml creates
    it via the deployer's own WIF session and immediately invalidates the
    previous one) -- the short-lived-credential principle docs/design.md
    argues for at the *sandbox* layer applied one layer up, to the source
    key itself.

    Deliberately does not pass `owner=` to `_write_remote_file`: that
    helper also chowns the file's *parent*, which is correct for
    `CONTROLLER_AGENT_TOKEN_PATH` (a private grain-agent home directory)
    but wrong here -- `/data/secrets` is shared with the GitHub and
    Claude credentials, which must stay root-owned. Only the file itself
    is handed to `metadata_user`.
    """
    _write_remote_file(runner, GCP_SERVICE_ACCOUNT_KEY_PATH, key.strip() + "\n", mode="640")
    runner.run(["sudo", "chown", f"{metadata_user}:{metadata_user}", GCP_SERVICE_ACCOUNT_KEY_PATH])
    metadata_config = json.dumps({
        "service_account_email": service_account_email,
        "project_id": project_id,
        "numeric_project_id": numeric_project_id,
        "metadata_user": metadata_user,
    }, indent=2) + "\n"
    _write_remote_file(runner, METADATA_SERVER_CONFIG_PATH, metadata_config, mode="644")


def configure_gemini_key(runner: Runner, project_id: str) -> None:
    """Writes `/data/config/gemini-key.json` (bwsalmon/agents#47), the
    on/off switch `core.py`'s `_resolve_target` checks before honouring a
    task issue's `gemini_key_label` -- absent, the label is refused with
    an explanation, the same "unusable request parks the task" shape an
    unlisted `/repo` already gets.

    Deliberately places no new credential of its own: `gemini_keys.py`
    authenticates with the same primary GCP service-account key
    `configure_gcp_service_account` already writes to
    `GCP_SERVICE_ACCOUNT_KEY_PATH` -- run that first (this function does not
    check that it exists, the same "operator sequencing, not enforced code"
    latitude every other step in this module already gets).
    """
    gemini_key_json = json.dumps({"project_id": project_id}, indent=2) + "\n"
    _write_remote_file(runner, GEMINI_KEY_CONFIG_PATH, gemini_key_json, mode="644")


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
