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
from ..inventory import Cluster
from ..run import Runner

DATA_CONFIG = "/data/config"
CLUSTER_CONFIG_PATH = "/data/config/cluster.toml"
DATA_SECRETS_GITHUB = "/data/secrets/github"
CLAUDE_TOKEN_PATH = "/data/secrets/claude-oauth-token"
SANDBOX_TOKENS_PATH = "/data/secrets/sandbox-tokens.json"
# Must match grain/automation/gemini_keys.py's `GeminiKeyConfig.key_path`
# default -- this module and that one are never imported into each other
# (SSH-remote writes here, local file I/O there), so the path can only be
# kept in sync by hand; a rename on one side and not the other is silent
# until `gemini_keys.py`'s own `gcloud auth activate-service-account` fails
# to find the file.
# bwsalmon/agents#131: the *minter* identity gcp_keys.py authenticates
# as -- a key for the host service account, deliberately a different
# file (and a different account) from the agent key above.
GCP_KEY_MINTER_KEY_PATH = "/data/secrets/gcp-key-minter.json"
# Must match grain/automation/gemini_keys.py's `GeminiKeyConfig` load path
# -- same "kept in sync by hand" caveat as the pair above; see this
# constant's own use in `configure_gemini_key`.
GEMINI_KEY_CONFIG_PATH = "/data/config/gemini-key.json"
# Must match grain/automation/gcp_keys.py's `GcpKeyConfig` load path -- same
# "kept in sync by hand" caveat as the pair above; see this constant's own
# use in `configure_agent_gcp_key`.
GCP_KEY_CONFIG_PATH = "/data/config/gcp-key.json"
# Must match grain/automation/janitor.py's `JanitorConfig` load path --
# same caveat, see this constant's own use in `configure_janitor`.
JANITOR_CONFIG_PATH = "/data/config/janitor.json"
# Must match grain/automation/scratch_repo.py's `ScratchRepoConfig` load
# path -- same "kept in sync by hand" caveat as the pair above, see this
# constant's own use in `configure_scratch_repo`. Carries no credential of
# its own (bwsalmon/agents#186) -- see that module's own docstring for
# where a scratch repo's actual GitHub credential comes from instead.
SCRATCH_REPO_CONFIG_PATH = "/data/config/scratch-repo.json"
# bwsalmon/agents#163: must match grain/automation/scheduled_jobs.py's own
# `/data/config/scheduled-jobs/` path -- the same "kept in sync by hand"
# precedent every other path constant in this module already sets, since
# this module writes over SSH and that one reads locally on the controller.
SCHEDULED_JOBS_DIR = "/data/config/scheduled-jobs"


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


def configure_cluster(runner: Runner, cluster: Cluster) -> None:
    """Writes `/data/config/cluster.toml` with the two `Cluster` fields the
    controller-side automation service actually needs -- `sandbox_count`
    and `subnet`, which is everything `sandbox_names`/`address_of`/
    `controller_ip` derive from (`grain/inventory.py`). Every other
    `Cluster` field (VM sizing, image, bridge name) only matters to `grain
    host bootstrap` itself, which already reads the host's own
    `--cluster-file`.

    The controller has no `cluster.toml` of its own otherwise -- the host's
    copy (`grain/bootstrap.py`'s `build_cluster`/`--cluster-file`) never
    left the host -- so `grain-automation.service`'s `automation run-once`
    silently ran with `Cluster()`'s bare defaults (always two sandboxes),
    no matter what the real deployment's `sandbox_count` said. This file,
    and `provision/controller.sh` pointing that service's `--cluster-file`
    at it, is what closes that gap.

    Written unconditionally on every bootstrap run, the same as
    `configure_repo` above: a sync, not a create, so raising `sandbox_count`
    after the first bootstrap reaches the controller on the very next one
    rather than requiring the controller itself to be recreated.
    """
    cluster_toml = (
        f"sandbox_count = {cluster.sandbox_count}\n"
        f'subnet = "{cluster.subnet}"\n'
    )
    _write_remote_file(runner, CLUSTER_CONFIG_PATH, cluster_toml, mode="644")


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


def configure_named_github_key(runner: Runner, token: str, *, name: str) -> None:
    """Writes an additional named credential's token file only --
    bwsalmon/agents#52's `grain-github-<name>` label, which selects it
    directly (`CredentialSet.get`, `grain/proxy/core.py`) rather than
    through the `credentials.json` owner/repo ladder `configure_github_
    credential` maintains. Deliberately does not touch `credentials.json`:
    unlike that function, this one must never make `name` any repo's
    *default* credential too, since the whole point is a scope (e.g.
    `workflow`) the default deliberately withholds -- see docs/design.md,
    "Scopes to withhold".
    """
    _write_remote_file(
        runner, f"{DATA_SECRETS_GITHUB}/{name}.token",
        token.strip() + "\n", mode="600",
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


def configure_agent_gcp_key(runner: Runner, *, service_account_email: str,
                             project_id: str, max_key_age_hours: int = 24,
                             key_path: str = GCP_KEY_MINTER_KEY_PATH) -> None:
    """Writes `/data/config/gcp-key.json` (bwsalmon/agents#126), the
    on/off switch `Orchestrator.gcp_key_config` (`cli.py`'s
    `build_orchestrator`) checks before minting a GCP service-account key
    for every dispatched sandbox -- absent, a deployment's sandboxes get
    no GCP access at all, the same "unusable feature parks/skips, doesn't
    guess" shape `configure_gemini_key` already has for its own label.

    Places no credential of its own -- this is plain, non-secret
    configuration (`service_account_email` and `project_id` are already
    published as non-secret deploy config, `terraform/gcp/instance.tf`'s
    `grain-config`), unlike the minter key `configure_gcp_key_minter`
    places. It only *names* the minter credential;
    `configure_gcp_key_minter` below is what actually places it.

    bwsalmon/agents#131: `gcp_keys.py` used to need no credential at all,
    on the (false) premise that the controller runs as the host service
    account via a native GCE metadata server. The controller is a nested
    libvirt guest, not a GCE VM, so `gcloud` there had no account at all
    and every mint and reap failed -- see that module's docstring.
    """
    gcp_key_json = json.dumps({
        "service_account_email": service_account_email,
        "project_id": project_id,
        "max_key_age_hours": max_key_age_hours,
        "key_path": key_path,
    }, indent=2) + "\n"
    _write_remote_file(runner, GCP_KEY_CONFIG_PATH, gcp_key_json, mode="644")


def configure_gcp_key_minter(runner: Runner, key: str) -> None:
    """Places the minter credential `gcp_keys.py` authenticates as
    (bwsalmon/agents#131), at `GCP_KEY_MINTER_KEY_PATH`.

    A key for the *host* service account, which `terraform/gcp/iam.tf`
    grants `roles/iam.serviceAccountKeyAdmin` on the agent account. Never
    the agent account's own key: minting has to be done by an identity the
    agent itself does not hold, or a leaked agent key can mint its own
    replacement and the whole expiry premise collapses.

    Read by the automation running as root, so it stays `600` and
    root-owned -- nothing else on the controller has any business
    reading it. This is the controller's only GCP credential.
    """
    _write_remote_file(runner, GCP_KEY_MINTER_KEY_PATH, key.strip() + "\n", mode="600")


def configure_gemini_key(runner: Runner, project_id: str, *,
                          impersonate_service_account: str | None = None) -> None:
    """Writes `/data/config/gemini-key.json` (bwsalmon/agents#47), the
    on/off switch `core.py`'s `_resolve_target` checks before honouring a
    task issue's `gemini_key_label` -- absent, the label is refused with
    an explanation, the same "unusable request parks the task" shape an
    unlisted `/repo` already gets.

    Places no credential of its own: `gemini_keys.py` authenticates with the
    minter key `configure_gcp_key_minter` writes (the host account's), and
    acts as the agent account by impersonating it -- `impersonate_service_account`
    below, bwsalmon/agents#131. The controller holds no agent key at all.
    """
    payload = {"project_id": project_id}
    if impersonate_service_account:
        payload["impersonate_service_account"] = impersonate_service_account
    gemini_key_json = json.dumps(payload, indent=2) + "\n"
    _write_remote_file(runner, GEMINI_KEY_CONFIG_PATH, gemini_key_json, mode="644")


def configure_janitor(runner: Runner, project_id: str, ttl_hours: int, *,
                       name_prefix: str = "grain",
                       impersonate_service_account: str | None = None) -> None:
    """Writes `/data/config/janitor.json` (bwsalmon/agents#113), the
    on/off switch `core.py`'s `_janitor` checks before scanning the
    project for GCE instances, disks, and Gemini API keys past
    `ttl_hours` old -- absent, `_janitor` is a no-op, the same "unusable
    request parks the task" shape `configure_gemini_key` already gets, just
    with nothing to park since this isn't tied to any one task.

    Places no credential of its own: `janitor.py` authenticates with the
    minter key `configure_gcp_key_minter` writes (the host account's), and
    acts as the agent account by impersonating it -- `impersonate_service_account`
    below, bwsalmon/agents#131. The controller holds no agent key at all.

    `name_prefix` must match the Terraform deployment's own `name_prefix`
    (default `"grain"`) -- it names the exact host/data-disk resources the
    janitor must never delete regardless of age. A deployment that
    customises `name_prefix` and skips this argument would have the
    janitor protecting the wrong names, so a Terraform-managed deployment
    always passes its own `var.name_prefix` through (`deploy.sh`).
    """
    payload = {
        "project_id": project_id, "ttl_hours": ttl_hours, "name_prefix": name_prefix,
    }
    if impersonate_service_account:
        payload["impersonate_service_account"] = impersonate_service_account
    janitor_json = json.dumps(payload, indent=2) + "\n"
    _write_remote_file(runner, JANITOR_CONFIG_PATH, janitor_json, mode="644")


def configure_scheduled_job(runner: Runner, name: str, template: str) -> None:
    """Writes one scheduled job's template file (bwsalmon/agents#163) at
    `/data/config/scheduled-jobs/<name>.md` -- the same "one named file
    among many" shape `configure_named_github_key` already has for a named
    credential, callable once per job rather than needing every job passed
    in a single call. `template` is written verbatim (the header lines, a
    blank line, then the body) -- this function does no parsing of its
    own; `scheduled_jobs.py`'s `ScheduledJobsConfig.load` is what
    interprets it, on the controller side, the next `run_once`.

    Removing a job is not handled here (this project's config commands
    only ever add or replace, never delete -- the same latitude every
    other `configure_*` step in this module already takes): an operator
    drops the file with `ssh ... rm` by hand, or a Terraform-managed
    deployment's `deploy.sh` reconciles the directory to exactly what the
    repo currently holds (see docs/runbook.md's "Scheduled jobs").
    """
    _write_remote_file(runner, f"{SCHEDULED_JOBS_DIR}/{name}.md", template, mode="644")


def configure_scratch_repo(runner: Runner, *, owner: str,
                            repo_prefix: str = "grain-scratch") -> None:
    """Writes `/data/config/scratch-repo.json` (bwsalmon/agents#159), the
    on/off switch `config.scratch_repo_label` needs -- absent, `core.py`'s
    `_resolve_target` refuses the label with an explanation, the same
    "unusable request parks the task" shape `configure_gemini_key` already
    has for its own label.

    Places no credential of its own -- `owner` and `repo_prefix` are both
    plain, non-secret configuration, just naming which repos
    `scratch_repo.py`'s `repo_for_sandbox` computes. The credential that
    actually authenticates against those repos is an ordinary named
    GitHub key (`configure_named_github_key`, e.g. `--github-key
    scratch=PATH`) plus an `owner/*` (or narrower) `credentials.json`
    pattern an operator adds by hand -- see `scratch_repo.py`'s own
    docstring, and docs/runbook.md's "Enabling `grain-scratch-repo`", for
    why this deliberately isn't automated any further than
    `configure_github_credential`'s own exact-repo patterns already are.
    """
    scratch_repo_json = json.dumps({
        "owner": owner, "repo_prefix": repo_prefix,
    }, indent=2) + "\n"
    _write_remote_file(runner, SCRATCH_REPO_CONFIG_PATH, scratch_repo_json, mode="644")


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
