"""`grain host` — the operator surface for the host adapter.

Two commands exist mainly for safety rather than convenience:

  grain host rules     print the firewall ruleset without applying it
  --dry-run            print the commands that would run, execute none

Both are there because this program rewrites the firewall of a machine you
may only be able to reach through that firewall. Being able to read exactly
what will happen, before it happens, is worth the small amount of code.
"""

from __future__ import annotations

import argparse
import dataclasses
import os
import shlex
import sys
from pathlib import Path

from .adapter.base import EgressMode, VmState
from .adapter.deploy import DEFAULT_DEST, deploy_tree
from .adapter.diagnostics import dump_guest_diagnostics, dump_host_diagnostics
from .adapter.libvirt import LibvirtAdapter
from .adapter.net_linux import LinuxNetwork, render_host_input_rules, render_ruleset
from .adapter.wait import wait_for_provisioning, wait_for_ssh
from .automation.audit import FileAuditLog
from .automation.cleanup import cleanup
from .automation.config import AutomationConfig
from .automation.configure import (
    configure_agent_gcp_key, configure_claude_token, configure_gcp_key_minter,
    configure_gemini_key, configure_github_credential, configure_janitor,
    configure_named_github_key, configure_repo, configure_scheduled_job,
    configure_scratch_repo, credential_repos,
    require_credential_coverage, required_credential_repos,
)
from .automation.core import Orchestrator
from .automation.credential_audit import Verdict, audit_secrets_dir
from .automation.gcp_keys import GcpKeyConfig
from .automation.gemini_keys import GeminiKeyConfig
from .automation.github import DryRunGitHubClient, GitHubClient, RealTransport
from .automation.health import DEFAULT_DISK_WATERMARK_PERCENT, check_health
from .automation.history import FileSessionHistory
from .automation.janitor import JanitorConfig
from .automation.scheduled_jobs import ScheduledJobsConfig
from .automation.scratch_repo import ScratchRepoConfig
from .automation.ssh import SshRunner
from .automation.state import AutomationState, utcnow
from .automation import tui as sessions_tui
from .bootstrap import BootstrapConfig, bootstrap
from .inventory import Cluster
from .proxy.allowlist import Allowlist
from .proxy.credentials import CredentialSet
from .proxy.tokens import SandboxCredentialStore, SandboxTokenStore
from .run import DryRunRunner, RealRunner, Runner

_REPO_ROOT = Path(__file__).resolve().parent.parent

# The same defaults `AutomationConfig`'s dataclass fields carry — read from
# there rather than repeated as literals, so `grain host health`/`cleanup`
# (which need SSH access but nothing else automation.json holds: no owner,
# no repo) can't quietly drift from what `grain automation run-once`
# actually uses.
_DEFAULT_SSH_USER = AutomationConfig.__dataclass_fields__["ssh_user"].default
_DEFAULT_SSH_KEY_PATH = AutomationConfig.__dataclass_fields__["ssh_key_path"].default
# The admin *private* key's default path -- the counterpart of
# LibvirtAdapter's `admin_public_key_path` default one directory up
# (`.pub`). Used by commands that reach a VM directly from the host/operator
# side (`host wait`, `host deploy`, `sandbox login`, `host bootstrap`)
# rather than from the controller's own automation dispatch path, which is
# what `_DEFAULT_SSH_KEY_PATH` above is for.
_DEFAULT_ADMIN_SSH_KEY_PATH = Path("/var/lib/grain/admin-ssh")


def build_cluster(args: argparse.Namespace) -> Cluster:
    cluster = Cluster.load(Path(args.cluster_file))
    overrides = {}
    if args.sandboxes is not None:
        overrides["sandbox_count"] = args.sandboxes
    if args.image is not None:
        overrides["image"] = args.image
    if overrides:
        cluster = dataclasses.replace(cluster, **overrides)
    return cluster


def build_adapter(cluster: Cluster, runner: Runner, args: argparse.Namespace):
    # libvirt, not Lima: `networks[].lima` (the mechanism lima.py needed) is
    # macOS-only, verified against Lima 2.2.0. A darwin adapter still
    # implements the same interface, with socket_vmnet and pf; see
    # docs/design.md and docs/host-adapter.md.
    network = LinuxNetwork(cluster, runner)
    return LibvirtAdapter(
        cluster, runner, network, config_dir=Path(args.config_dir),
        admin_public_key_path=Path(args.admin_ssh_public_key),
        controller_public_key_path=Path(args.controller_ssh_public_key),
    )


def build_orchestrator(cluster: Cluster, runner: Runner,
                        args: argparse.Namespace) -> tuple[Orchestrator, Path]:
    data_dir = Path(args.data_dir)
    config = AutomationConfig.load(data_dir / "config" / "automation.json")
    # The whole `CredentialSet`, not one repo's token resolved up front:
    # a cycle talks to the task repo *and* to whichever target repos its
    # tasks name, and the narrowest-pattern ladder is what picks the right
    # credential for each (`CredentialSet.token_for`).
    credentials = CredentialSet(data_dir / "secrets" / "github")
    github: GitHubClient | DryRunGitHubClient = GitHubClient(
        RealTransport(config.github_host, use_tls=config.github_use_tls),
        credentials,
    )
    # bwsalmon/agents#159/#186: absence is the off switch, same shape as
    # gemini_key_config/gcp_key_config below -- a deployment that never ran
    # `grain controller configure --scratch-repo-owner ...` has no such
    # file, and `Orchestrator._resolve_target` refuses the
    # `scratch_repo_label` with an explanation. Carries no credential of
    # its own -- a scratch repo's actual credential is just another entry
    # in `credentials` above, see `scratch_repo.py`'s own docstring.
    scratch_repo_config_path = data_dir / "config" / "scratch-repo.json"
    scratch_repo_config = (
        ScratchRepoConfig.load(scratch_repo_config_path)
        if scratch_repo_config_path.exists() else None
    )
    if args.dry_run:
        github = DryRunGitHubClient(github)
    state_path = data_dir / "state" / "automation" / "state.json"
    audit = FileAuditLog(data_dir / "state" / "automation" / "audit.log")
    token_store = SandboxTokenStore(data_dir / "secrets" / "sandbox-tokens.json")
    # bwsalmon/agents#52: the write side of the same file
    # `grain/proxy/server.py`'s `build_proxy` reads for a `grain-github-
    # <name>` label's override -- see `SandboxCredentialStore`'s own
    # docstring for why it lives under config/, not secrets/.
    credential_store = SandboxCredentialStore(data_dir / "config" / "sandbox-github-key.json")
    history = FileSessionHistory(data_dir / "state" / "automation" / "sessions")
    # Absence is the off switch (bwsalmon/agents#47): a deployment that
    # never ran `grain controller configure --gemini-project-id ...` has no
    # such file, and `Orchestrator._resolve_target` refuses a task carrying
    # `gemini_key_label` with an explanation rather than guessing at a
    # project.
    gemini_key_config_path = data_dir / "config" / "gemini-key.json"
    gemini_key_config = (
        GeminiKeyConfig.load(gemini_key_config_path)
        if gemini_key_config_path.exists() else None
    )
    # bwsalmon/agents#126: same "absence is the off switch" shape as
    # gemini_key_config above -- a deployment that never wrote
    # gcp-key.json (`grain controller configure
    # --gcp-agent-service-account-email ...`) gets no GCP access in its
    # sandboxes, not a crash.
    gcp_key_config_path = data_dir / "config" / "gcp-key.json"
    gcp_key_config = (
        GcpKeyConfig.load(gcp_key_config_path)
        if gcp_key_config_path.exists() else None
    )
    # bwsalmon/agents#113: same "absence is the off switch" shape as
    # gemini_key_config/gcp_key_config above -- a deployment that never
    # ran `grain controller configure --janitor-ttl-hours ...` gets no
    # janitor pass, not a crash.
    janitor_config_path = data_dir / "config" / "janitor.json"
    janitor_config = (
        JanitorConfig.load(janitor_config_path)
        if janitor_config_path.exists() else None
    )
    # bwsalmon/agents#163: same "absence is the off switch" shape as
    # janitor_config above, over a directory instead of one file -- a
    # deployment that has never written a `*.md` job template into
    # `/data/config/scheduled-jobs/` gets no scheduled-jobs pass, not a
    # crash. An existing-but-empty directory still loads (to zero jobs),
    # which is just as harmless.
    scheduled_jobs_dir = data_dir / "config" / "scheduled-jobs"
    scheduled_jobs_config = (
        ScheduledJobsConfig.load(scheduled_jobs_dir)
        if scheduled_jobs_dir.is_dir() else None
    )
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=AutomationState.load(state_path), base_runner=runner,
        token_store=token_store,
        # The same file the git proxy enforces (`build_proxy`), read from
        # the same place -- see `Orchestrator.allowlist`.
        allowlist=Allowlist(data_dir / "config" / "repo-allowlist.json"),
        audit=audit, history=history, gemini_key_config=gemini_key_config,
        gcp_key_config=gcp_key_config, janitor_config=janitor_config,
        scratch_repo_config=scratch_repo_config,
        scheduled_jobs_config=scheduled_jobs_config,
        credentials=credentials, credential_store=credential_store,
        # bwsalmon/agents#51: lets `Orchestrator` persist state incrementally,
        # mid-`run_once`, rather than only once at the very end (see
        # `state_path`'s own docstring on `Orchestrator` for why that matters
        # across a controller VM restart). `None` under `--dry-run`, same as
        # `cmd_automation_run_once`'s existing final save already being
        # skipped there -- a dry run must never write real state to disk.
        state_path=None if args.dry_run else state_path,
    )
    return orchestrator, state_path


def cmd_automation_run_once(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    orchestrator, state_path = build_orchestrator(cluster, _runner(args), args)
    orchestrator.run_once(utcnow())
    if not args.dry_run:
        orchestrator.state.save(state_path)
    return 0


def cmd_automation_status(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    data_dir = Path(args.data_dir)
    state = AutomationState.load(data_dir / "state" / "automation" / "state.json")
    for name in cluster.sandbox_names:
        assignment = state.assignments.get(name)
        if assignment is None:
            print(f"{name:<12} free")
        else:
            # The target repo, where the work is actually happening -- the
            # issue number alone names a task in the task repo, which says
            # nothing about which service is being changed. A dash for an
            # assignment written before the task/target split.
            target = (
                f"{assignment.target_owner}/{assignment.target_repo}"
                if assignment.target_owner and assignment.target_repo else "-"
            )
            print(f"{name:<12} {assignment.kind.value} #{assignment.issue:<6} "
                  f"{target:<24} since {assignment.started_at.isoformat()}")
    return 0


def build_session_history(args: argparse.Namespace) -> FileSessionHistory:
    return FileSessionHistory(Path(args.data_dir) / "state" / "automation" / "sessions")


def cmd_sessions_list(args: argparse.Namespace) -> int:
    history = build_session_history(args)
    records = history.all()
    if args.kind:
        records = [r for r in records if r.kind.value == args.kind]
    if args.outcome:
        records = [r for r in records if r.outcome == args.outcome]
    if args.trigger is not None:
        records = [r for r in records if r.issue == args.trigger]
    if not records:
        print("no sessions recorded yet")
        return 0
    for record in records:
        print(sessions_tui.format_row(record))
    return 0


def cmd_sessions_browse(args: argparse.Namespace) -> int:
    history = build_session_history(args)
    sessions_tui.run(history)
    return 0


def cmd_github_audit(args: argparse.Namespace) -> int:
    secrets_dir = Path(args.data_dir) / "secrets" / "github"
    results = audit_secrets_dir(RealTransport(), secrets_dir)
    if not results:
        print(f"no *.token files found under {secrets_dir}")
        return 0
    exit_code = 0
    for r in results:
        print(f"{r.name:<12} {r.kind.value:<45} {r.verdict.value:<12} {r.detail}")
        if r.verdict is Verdict.FLAGGED:
            exit_code = 1
    return exit_code


def cmd_rules(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    if args.input_chain:
        sys.stdout.write(render_host_input_rules(cluster, ssh_port=args.ssh_port))
    else:
        sys.stdout.write(render_ruleset(cluster, EgressMode(args.egress)))
    return 0


def cmd_up(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    runner = _runner(args)
    egress = EgressMode(args.egress)
    LinuxNetwork(cluster, runner).up(egress)
    if args.persist:
        # bwsalmon/agents#111: without this, the bridge/nftables policy
        # `up()` just applied lives only in the running kernel and vanishes
        # on the host's next reboot -- see `render_boot_unit`'s own
        # docstring for what that silently breaks. `Path.cwd()`, not a
        # fixed path: unlike the controller (always deployed to
        # `/opt/grain` by `grain host deploy`), this repo has no canonical
        # checkout location on the host -- wherever this command is being
        # run from is where the boot unit re-invokes it from too.
        LinuxNetwork(cluster, runner).install_boot_unit(Path.cwd(), egress)
    return 0


def cmd_egress(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    adapter.egress_policy(EgressMode(args.mode))
    return 0


def cmd_create(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    _check_provision_target(cluster, args)
    adapter = build_adapter(cluster, _runner(args), args)
    script = Path(args.provision).read_text() if args.provision else None
    for name in _targets(cluster, args.name):
        adapter.create(cluster.spec_of(name), script)
    return 0


def _check_provision_target(cluster: Cluster, args: argparse.Namespace) -> None:
    """`--provision` applies one script to every named target. The
    controller and the sandboxes are provisioned by different scripts
    (`provision/controller.sh` vs. `provision/sandbox.sh`) — 'all' would
    silently apply whichever one was given to both, which is wrong for
    either. Create/recreate the controller and the sandboxes as separate
    invocations instead.
    """
    if args.name == "all" and args.provision:
        raise SystemExit(
            "--provision with target 'all' would apply one script to both "
            "the controller and the sandboxes, which is almost certainly "
            "wrong now that they differ. Run this once for 'controller' "
            "with provision/controller.sh, and once for 'sandboxes' with "
            "provision/sandbox.sh."
        )


def cmd_start(args: argparse.Namespace) -> int:
    return _lifecycle(args, "start")


def cmd_stop(args: argparse.Namespace) -> int:
    return _lifecycle(args, "stop")


def cmd_destroy(args: argparse.Namespace) -> int:
    return _lifecycle(args, "destroy")


def cmd_recreate(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    _check_provision_target(cluster, args)
    targets = _targets(cluster, args.name)
    _check_controller_recreate(cluster, targets, args)
    adapter = build_adapter(cluster, _runner(args), args)
    script = Path(args.provision).read_text() if args.provision else None
    for name in targets:
        adapter.recreate(name, script)
    return 0


def _check_controller_recreate(cluster: Cluster, targets: list[str], args: argparse.Namespace) -> None:
    """`/data` (credentials, automation state) lives on the controller's own
    disk today -- there is no separate persistent disk for it yet (see
    `provision/controller.sh`) -- and `recreate` is destroy-then-create, so
    recreating the controller silently deletes it. `recreate` is described
    as routine maintenance elsewhere (README, runbook), so an operator can
    reach for it without expecting that. This is a guardrail, not a fix:
    once `/data` lives on its own disk that survives destroy, this check
    should come out.
    """
    if cluster.controller_name in targets and not args.i_know_this_deletes_data:
        raise SystemExit(
            f"recreating {cluster.controller_name!r} destroys /data -- every "
            "credential and all automation state -- because /data has no "
            "disk of its own yet and lives on the controller's own qcow2. "
            "Pass --i-know-this-deletes-data to proceed anyway."
        )


def cmd_status(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    known = {info.name: info for info in adapter.list_vms()}
    for name in cluster.names:
        info = known.get(name)
        state = info.state.value if info else VmState.ABSENT.value
        print(f"{name:<12} {state:<8} {cluster.address_of(name)}")
    return 0


def _repo_args(args: argparse.Namespace) -> tuple[str, list[str], str | None]:
    """`--task-repo`/`--target-repo`/`--default-target-repo`, validated into
    the shape `configure_repo` and `BootstrapConfig` both take.

    The no-`--target-repo` case is the single-repo deployment every
    deployment was before the task/target split: the task repo becomes the
    sole allow-listed target *and* the default for a task with no `/repo`
    directive, so nothing about such a deployment has to change. Given
    explicit targets, there is no default unless one is named -- guessing
    which of several repos an ambiguous task meant is exactly what
    `core.py`'s `_park` exists to refuse.
    """
    for flag, value in (("--task-repo", args.task_repo),
                        ("--default-target-repo", args.default_target_repo),
                        *(("--target-repo", t) for t in args.target_repos)):
        if value is None:
            continue
        parts = value.split("/")
        if len(parts) != 2 or not all(parts):
            raise SystemExit(f"{flag} must be 'owner/name', got {value!r}")
    targets = list(dict.fromkeys(args.target_repos)) or [args.task_repo]
    default_target = args.default_target_repo or (
        args.task_repo if not args.target_repos else None
    )
    if default_target is not None and default_target not in targets:
        raise SystemExit(
            f"--default-target-repo {default_target!r} is not one of the "
            f"--target-repo values {targets!r} -- a task falling back to it "
            "would be parked as not allow-listed"
        )
    return args.task_repo, targets, default_target


def build_ssh_runner(cluster: Cluster, base_runner: Runner, name: str,
                      args: argparse.Namespace) -> Runner:
    """The same shape as `Orchestrator._ssh_runner_for`, but standalone: a
    health check or a cleanup run needs SSH access, not a `GitHubClient` or
    a sandbox git-proxy token, so this avoids pulling `build_orchestrator`'s
    whole GitHub/credential wiring into a plain host-lifecycle command —
    and avoids requiring `automation.json` to exist at all, since `owner`/
    `repo` (its only fields with no default) are irrelevant here.
    """
    return SshRunner(
        inner=base_runner, user=args.ssh_user,
        address=cluster.address_of(name), key_path=Path(args.ssh_key),
    )


def cmd_host_cleanup(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    base_runner = _runner(args)
    exit_code = 0
    for name in _sandbox_targets(cluster, args.name):
        runner = build_ssh_runner(cluster, base_runner, name, args)
        result = cleanup(runner)
        for step in result.steps:
            print(f"{name:<12} {step.name:<8} {'ok' if step.ok else 'FAIL':<5} {step.detail}")
        if not result.ok:
            exit_code = 1
    return exit_code


def cmd_host_health(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    base_runner = _runner(args)
    exit_code = 0
    for name in _sandbox_targets(cluster, args.name):
        runner = build_ssh_runner(cluster, base_runner, name, args)
        report = check_health(runner, watermark_percent=args.disk_watermark)
        print(f"{name:<12} {report.status.value:<12} {report.summary()}")
        if not report.ok:
            exit_code = 1
    return exit_code


def cmd_host_wait(args: argparse.Namespace) -> int:
    """`grain host wait <name>` -- docs/bootstrap.md Phase 3's first missing
    verb: block until a VM answers SSH and has finished cloud-init. Lifted
    from `tests/loadtest.py`'s own `_wait_for_ssh`/`_wait_for_provisioning`
    into `grain/adapter/wait.py` so both this command and `host bootstrap`'s
    sequencer share one implementation.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    adapter = build_adapter(cluster, base_runner, args)
    for name in _targets(cluster, args.name):
        ssh = build_ssh_runner(cluster, base_runner, name, args)
        print(f"{name:<12} waiting for SSH (up to {args.timeout:.0f}s) ...")
        # Same diagnostics `host bootstrap`'s stage 5 prints, for the same
        # reason -- this command is the manual half of that stage, and a
        # bare TimeoutError is just as unreadable run by hand.
        try:
            wait_for_ssh(ssh, timeout=args.timeout)
        except TimeoutError:
            dump_host_diagnostics(adapter, name)
            raise
        print(f"{name:<12} waiting for cloud-init ...")
        try:
            wait_for_provisioning(ssh)
        except (RuntimeError, TimeoutError):
            dump_guest_diagnostics(ssh)
            raise
        print(f"{name:<12} ready")
    return 0


def cmd_host_deploy(args: argparse.Namespace) -> int:
    """`grain host deploy [name]` -- docs/bootstrap.md Phase 3's second
    missing verb: push this working tree to `/opt/grain` on the controller.
    No credential; see `grain/adapter/deploy.py`.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    if args.name != cluster.controller_name:
        raise SystemExit(
            f"deploy target must be '{cluster.controller_name}' -- this repo's "
            "code only ever runs there, not on a sandbox"
        )
    deploy_tree(
        base_runner, Path(args.source), user=args.ssh_user,
        address=cluster.address_of(cluster.controller_name),
        key_path=Path(args.ssh_key), dest=args.dest,
    )
    return 0


def _read_named_github_keys(entries: list[str]) -> dict[str, str]:
    """Parses repeated `--github-key NAME=FILE` entries into `{name: token}`,
    shared by `controller configure` and `host bootstrap` (bwsalmon/
    agents#134 added the latter so a named key can be threaded through a
    deployment repo's own bootstrap, not just set by hand afterward).
    """
    keys: dict[str, str] = {}
    for entry in entries:
        name, sep, path = entry.partition("=")
        if not sep or not name or not path:
            raise SystemExit(f"--github-key must be NAME=FILE, got {entry!r}")
        if name == "anonymous":
            raise SystemExit("--github-key name 'anonymous' is reserved")
        keys[name] = sys.stdin.read() if path == "-" else Path(path).read_text()
    return keys


def _read_scheduled_jobs(entries: list[str]) -> dict[str, str]:
    """Parses repeated `--scheduled-job NAME=FILE` entries into
    `{name: template_text}` (bwsalmon/agents#163) -- same `NAME=FILE`
    shape `_read_named_github_keys` already has, kept as its own function
    rather than shared since a job name has none of that one's reserved-
    word ('anonymous') restriction.
    """
    jobs: dict[str, str] = {}
    for entry in entries:
        name, sep, path = entry.partition("=")
        if not sep or not name or not path:
            raise SystemExit(f"--scheduled-job must be NAME=FILE, got {entry!r}")
        jobs[name] = sys.stdin.read() if path == "-" else Path(path).read_text()
    return jobs


def cmd_controller_configure(args: argparse.Namespace) -> int:
    """`grain controller configure` -- docs/bootstrap.md Phase 3's third
    missing verb: writes `/data/config/automation.json`,
    `repo-allowlist.json`, and, if supplied, the GitHub token/credential
    mapping and the Claude Code OAuth token. See
    `grain/automation/configure.py`.

    Restarts `grain-git-proxy.service` afterward -- once, after every write
    below rather than right after `configure_repo`: `build_proxy`
    (`grain/proxy/server.py`) reads both `automation.json` (for
    `git_forward_host`) and `credentials.json` once, at process startup, so
    a live proxy keeps using whatever it started with until restarted.
    Restarting between the two writes (an earlier version of this function
    did) fixes the first but not the second -- found live, adding a new
    target repo's credential and immediately dispatching against it failed
    with a proxied 500 ("no credential configured") because the restart had
    already happened before `configure_github_credential` wrote the new
    mapping. Harmless if the service isn't running yet (a fresh controller
    that hasn't reached `host bootstrap`'s stage 10): `systemctl restart` on
    a stopped-but-installed unit just starts it.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    ssh = build_ssh_runner(cluster, base_runner, cluster.controller_name, args)
    task_repo, targets, default_target = _repo_args(args)
    configure_repo(ssh, task_repo, targets, default_target_repo=default_target,
                    github_host=args.github_host,
                    git_forward_host=args.git_forward_host,
                    github_use_tls=not args.github_insecure_http)
    if args.github_token_file:
        token = (
            sys.stdin.read() if args.github_token_file == "-"
            else Path(args.github_token_file).read_text()
        )
        mapping = configure_github_credential(
            ssh, credential_repos(task_repo, targets), token,
            credential_name=args.credential_name,
        )
        require_credential_coverage(
            mapping, required_credential_repos(task_repo, targets, default_target),
        )
    for name, token in _read_named_github_keys(args.github_key).items():
        configure_named_github_key(ssh, token, name=name)
    if args.claude_token_file:
        configure_claude_token(ssh, Path(args.claude_token_file).read_text())
    if args.gcp_agent_service_account_email and args.gcp_project_id:
        # bwsalmon/agents#126: plain, non-secret config, so no
        # all-or-nothing SystemExit when only part of it is given (a
        # deployment naming just one just doesn't turn this feature on,
        # the same latitude every other optional step here already has).
        configure_agent_gcp_key(
            ssh, service_account_email=args.gcp_agent_service_account_email,
            project_id=args.gcp_project_id,
            max_key_age_hours=args.gcp_key_max_age_hours,
        )
    if args.gcp_key_minter_key_file:
        # bwsalmon/agents#131: the controller's one GCP credential -- the
        # host account, which mints the agent's per-dispatch keys and is
        # impersonated *as* the agent for janitor/Gemini work. The minter
        # must not be the account being minted for; see gcp_keys.py.
        configure_gcp_key_minter(ssh, Path(args.gcp_key_minter_key_file).read_text())
    if args.gemini_project_id:
        # Uses the minter key already placed, impersonating the agent
        # account (bwsalmon/agents#47, #131) -- no separate credential
        # step, only the project id that turns the grain-gemini-key task
        # label on.
        configure_gemini_key(
            ssh, args.gemini_project_id,
            impersonate_service_account=args.gcp_agent_service_account_email,
        )
    if args.janitor_ttl_hours is not None:
        # Same reuse as --gemini-project-id above (bwsalmon/agents#113) --
        # the janitor authenticates with the same primary key.
        if not args.gcp_project_id:
            raise SystemExit("--janitor-ttl-hours requires --gcp-project-id")
        configure_janitor(ssh, args.gcp_project_id, args.janitor_ttl_hours,
                           impersonate_service_account=args.gcp_agent_service_account_email,
                           name_prefix=args.janitor_name_prefix)
    for name, template in _read_scheduled_jobs(args.scheduled_job).items():
        configure_scheduled_job(ssh, name, template)
    if args.scratch_repo_owner:
        # bwsalmon/agents#159/#186: plain, non-secret config -- the
        # credential itself is just another `--github-key NAME=FILE` above
        # plus a `credentials.json` pattern added by hand, not anything
        # this flag provisions.
        configure_scratch_repo(
            ssh, owner=args.scratch_repo_owner, repo_prefix=args.scratch_repo_prefix,
        )
    ssh.run(["sudo", "systemctl", "restart", "grain-git-proxy.service"])
    return 0


def cmd_host_bootstrap(args: argparse.Namespace) -> int:
    """`grain host bootstrap` -- docs/bootstrap.md Phase 4: the sequencer
    that replaces docs/runbook.md's fourteen-step checklist. See
    `grain/bootstrap.py` for the stage-by-stage reasoning.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    adapter = build_adapter(cluster, base_runner, args)
    github_token = None
    if args.github_token_file:
        github_token = (
            sys.stdin.read() if args.github_token_file == "-"
            else Path(args.github_token_file).read_text()
        )
    claude_token = (
        Path(args.claude_token_file).read_text()
        if args.claude_token_file else None
    )
    gcp_key_minter_key = None
    if args.gcp_key_minter_key_file:
        gcp_key_minter_key = Path(args.gcp_key_minter_key_file).read_text()
    task_repo, targets, default_target = _repo_args(args)
    config = BootstrapConfig(
        task_repo=task_repo, targets=tuple(targets),
        default_target_repo=default_target, github_token=github_token,
        credential_name=args.credential_name,
        github_keys=_read_named_github_keys(args.github_key),
        claude_token=claude_token,
        gcp_agent_service_account_email=args.gcp_agent_service_account_email,
        gcp_project_id=args.gcp_project_id,
        gcp_key_max_age_hours=args.gcp_key_max_age_hours,
        gcp_key_minter_key=gcp_key_minter_key,
        gemini_project_id=args.gemini_project_id,
        janitor_ttl_hours=args.janitor_ttl_hours,
        janitor_name_prefix=args.janitor_name_prefix,
        scheduled_jobs=_read_scheduled_jobs(args.scheduled_job),
        scratch_repo_owner=args.scratch_repo_owner,
        scratch_repo_prefix=args.scratch_repo_prefix,
        github_host=args.github_host, git_forward_host=args.git_forward_host,
        github_use_tls=not args.github_insecure_http,
        ssh_user=args.ssh_user, admin_private_key_path=Path(args.admin_ssh_private_key),
        controller_provision_script=(
            Path(args.controller_provision).read_text() if args.controller_provision else None
        ),
        sandbox_provision_script=(
            Path(args.sandbox_provision).read_text() if args.sandbox_provision else None
        ),
        ssh_timeout=args.ssh_timeout,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=base_runner, config=config)

    # Stage 11: verify -- the sequencer itself stops at "converged"; this is
    # the CLI command's own tail, using the admin key it already has rather
    # than re-deriving `grain automation status`/`github audit`/`host
    # health`'s own separate SSH-key flags (which name a different default,
    # the controller's dispatch key -- see `_DEFAULT_SSH_KEY_PATH`).
    print("[bootstrap] verify:")
    data_dir = Path(args.data_dir)
    state = AutomationState.load(data_dir / "state" / "automation" / "state.json")
    for name in cluster.sandbox_names:
        assignment = state.assignments.get(name)
        print(f"  automation: {name:<12} {'free' if assignment is None else assignment.kind.value}")
    for r in audit_secrets_dir(RealTransport(), data_dir / "secrets" / "github"):
        print(f"  github:     {r.name:<12} {r.verdict.value}")
    for name in cluster.sandbox_names:
        ssh = SshRunner(
            inner=base_runner, user=args.ssh_user,
            address=cluster.address_of(name), key_path=Path(args.admin_ssh_private_key),
        )
        report = check_health(ssh)
        print(f"  health:     {name:<12} {report.status.value}")
    return 0


def cmd_sandbox_login(args: argparse.Namespace) -> int:
    """`grain sandbox login <name>` -- direct, interactive admin SSH access
    to one sandbox or the controller, using the admin key rather than
    hopping through the controller first. For debugging: a stuck `kind`
    cluster, a wedged docker daemon, anything `grain host health`/`cleanup`/
    `sessions browse` doesn't give enough visibility into.

    This is deliberately possible only because of docs/bootstrap.md's Phase
    1 fix -- the admin key is now embedded as an authorized key on every VM
    at create time, not just informally reachable by whoever happens to hold
    the controller's own dispatch key. Holding the admin *private* key is
    what gates this, the same way holding any other SSH key gates any other
    login; this command adds no new capability the key itself doesn't
    already grant, it just removes the "hop through the controller" tax.

    Execs `ssh` directly rather than going through `Runner` -- this needs a
    real interactive terminal (raw stdio, a pty, signal passthrough), not a
    captured command result, so it is the one command in this CLI that isn't
    built on `Runner.run`.
    """
    cluster = build_cluster(args)
    if args.name not in cluster.names:
        raise SystemExit(f"unknown VM: {args.name} (known: {', '.join(cluster.names)})")
    address = cluster.address_of(args.name)
    argv = [
        "ssh", "-i", args.ssh_key,
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null",
        f"{args.ssh_user}@{address}",
    ]
    if args.dry_run:
        print("+ " + shlex.join(argv))
        return 0
    os.execvp("ssh", argv)  # noqa: S606 -- replaces this process with an interactive ssh session


# --- helpers --------------------------------------------------------------

def _runner(args: argparse.Namespace) -> Runner:
    return DryRunRunner() if args.dry_run else RealRunner()


def _targets(cluster: Cluster, name: str) -> list[str]:
    if name == "all":
        return cluster.names
    if name == "sandboxes":
        return cluster.sandbox_names
    if name not in cluster.names:
        raise SystemExit(f"unknown VM: {name} (known: {', '.join(cluster.names)})")
    return [name]


def _sandbox_targets(cluster: Cluster, name: str) -> list[str]:
    # cleanup/health apply only to sandboxes -- there is no controller
    # instance to clean up or health-check the same way, so this is the
    # same shape as _targets minus "all" meaning "including the controller."
    if name in ("all", "sandboxes"):
        return cluster.sandbox_names
    if name not in cluster.sandbox_names:
        raise SystemExit(
            f"unknown sandbox: {name} (known: {', '.join(cluster.sandbox_names)})"
        )
    return [name]


def _lifecycle(args: argparse.Namespace, action: str) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    for name in _targets(cluster, args.name):
        getattr(adapter, action)(name)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="grain", description=__doc__)
    parser.add_argument(
        "--cluster-file", default="/var/lib/grain/cluster.toml",
        help="TOML file overriding Cluster's dataclass defaults -- sandbox "
             "count, subnet, bridge, image, per-role sizing (docs/bootstrap.md "
             "Phase 2); falls back to the built-in defaults if absent",
    )
    parser.add_argument("--sandboxes", type=int, default=None,
                        help="number of sandbox VMs (default: 2, or --cluster-file's "
                             "sandbox_count)")
    parser.add_argument("--image", default=None,
                        help="base image path, overriding --cluster-file's image")
    parser.add_argument("--config-dir", default="/var/lib/grain/instances")
    parser.add_argument(
        "--admin-ssh-public-key", default="/var/lib/grain/admin-ssh.pub",
        help="host-local admin SSH public key, embedded as an authorized "
             "key on the controller and every sandbox -- setup, repair, "
             "and admin debugging access (see docs/bootstrap.md, 'key "
             "roles'; 'grain host bootstrap' generates one if absent)",
    )
    parser.add_argument(
        "--controller-ssh-public-key", default="/var/lib/grain/controller-ssh.pub",
        help="host-local copy of the controller's own SSH public key, "
             "embedded as an authorized key on every sandbox only -- the "
             "automation dispatch path (see docs/runbook.md)",
    )
    parser.add_argument("--data-dir", default="/data",
                        help="where automation config/secrets/state live")
    parser.add_argument("--dry-run", action="store_true",
                        help="print commands instead of running them")
    sub = parser.add_subparsers(dest="group", required=True)
    host = sub.add_parser("host", help="VM lifecycle and networking").add_subparsers(
        dest="command", required=True
    )

    p = host.add_parser("rules", help="print the firewall ruleset, apply nothing")
    p.add_argument("--egress", choices=[m.value for m in EgressMode], default="open")
    p.add_argument("--input-chain", action="store_true",
                   help="render a host INPUT policy (never applied automatically)")
    p.add_argument("--ssh-port", type=int, default=22)
    p.set_defaults(func=cmd_rules)

    p = host.add_parser("up", help="create the private network and apply the policy")
    p.add_argument("--egress", choices=[m.value for m in EgressMode], default="open")
    p.add_argument(
        "--persist", action="store_true",
        help="also install a systemd unit that reruns this at every boot "
             "(bwsalmon/agents#111: otherwise the bridge/nftables policy "
             "does not survive a host reboot)",
    )
    p.set_defaults(func=cmd_up)

    p = host.add_parser("egress", help="switch the egress policy")
    p.add_argument("mode", choices=[m.value for m in EgressMode])
    p.set_defaults(func=cmd_egress)

    for name, fn, help_text in (
        ("create", cmd_create, "create VMs"),
        ("start", cmd_start, "start VMs"),
        ("stop", cmd_stop, "stop VMs"),
        ("destroy", cmd_destroy, "destroy VMs and their disks"),
        ("recreate", cmd_recreate, "destroy, rebuild and start VMs"),
    ):
        p = host.add_parser(name, help=help_text)
        p.add_argument("name", help="VM name, or 'all' / 'sandboxes'")
        if name in ("create", "recreate"):
            p.add_argument("--provision", help="path to a provisioning script")
        if name == "recreate":
            p.add_argument(
                "--i-know-this-deletes-data", action="store_true",
                help="required to recreate the controller (or 'all') -- "
                     "/data has no disk of its own yet, so this destroys "
                     "every credential and all automation state",
            )
        p.set_defaults(func=fn)

    p = host.add_parser("status", help="show VM states and addresses")
    p.set_defaults(func=cmd_status)

    for name, fn, help_text in (
        ("cleanup", cmd_host_cleanup,
         "between-task hygiene: kind delete + docker system prune, over SSH"),
        ("health", cmd_host_health,
         "check SSH/docker/systemd/disk on sandbox(es); nonzero exit if unhealthy"),
    ):
        p = host.add_parser(name, help=f"{help_text} (sandboxes only)")
        p.add_argument("name", nargs="?", default="sandboxes",
                        help="sandbox name, or 'sandboxes' for all (default)")
        p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER,
                        help=f"default: {_DEFAULT_SSH_USER}")
        p.add_argument("--ssh-key", default=str(_DEFAULT_SSH_KEY_PATH),
                        help=f"default: {_DEFAULT_SSH_KEY_PATH}")
        if name == "health":
            p.add_argument("--disk-watermark", type=int,
                            default=DEFAULT_DISK_WATERMARK_PERCENT,
                            help=f"percent-used threshold to flag "
                                 f"(default: {DEFAULT_DISK_WATERMARK_PERCENT})")
        p.set_defaults(func=fn)

    p = host.add_parser(
        "wait", help="block until VM(s) answer SSH and finish cloud-init"
    )
    p.add_argument("name", nargs="?", default="all", help="VM name, or 'all' / 'sandboxes'")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.add_argument("--timeout", type=float, default=180.0,
                    help="seconds to wait for SSH per VM (default: 180)")
    p.set_defaults(func=cmd_host_wait)

    p = host.add_parser(
        "deploy", help="push this working tree to /opt/grain on the controller"
    )
    p.add_argument("name", nargs="?", default="controller", help="must be 'controller'")
    p.add_argument("--source", default=str(_REPO_ROOT), help="tree to deploy (default: this checkout)")
    p.add_argument("--dest", default=DEFAULT_DEST, help=f"default: {DEFAULT_DEST}")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_host_deploy)

    p = host.add_parser(
        "bootstrap",
        help="one command: network, controller, deploy, configure, sandboxes, enable (docs/bootstrap.md)",
    )
    p.add_argument("--task-repo", "--repo", dest="task_repo", required=True,
                    metavar="OWNER/NAME",
                    help="the task repo: the one repo polled for labelled issues "
                         "(--repo is accepted as its former name)")
    p.add_argument("--target-repo", dest="target_repos", action="append", default=[],
                    metavar="OWNER/NAME",
                    help="a repo tasks may dispatch into, named by a /repo directive on "
                         "an issue; repeatable. Omit entirely for a single-repo "
                         "deployment: the task repo becomes the only target and the "
                         "default for tasks with no /repo line")
    p.add_argument("--default-target-repo", metavar="OWNER/NAME",
                    help="target repo for a task with no /repo directive (default: none, "
                         "so a task without one is parked with a comment -- unless no "
                         "--target-repo was given at all, in which case the task repo)")
    p.add_argument("--github-token-file",
                    help="path to a file holding the GitHub token, or '-' for stdin")
    p.add_argument("--credential-name", default="bot",
                    help="credentials.json entry name for --github-token-file (default: bot)")
    p.add_argument("--github-key", action="append", default=[], metavar="NAME=FILE",
                    help="write an additional named GitHub credential (bwsalmon/agents#52), "
                         "same shape as controller configure's flag of the same name: a task "
                         "labelled `grain-github-NAME` uses it for every git push instead of "
                         "the default credential -- for a scope (e.g. `workflow`) the default "
                         "deliberately withholds. FILE holds the token, or '-' for stdin. "
                         "Repeatable; never becomes any repo's default credential")
    p.add_argument("--claude-token-file",
                    help="path to a file holding a Claude Code OAuth token (from `claude "
                         "setup-token`) to place on the controller and inject into the "
                         "grain-agent account's environment at dispatch time")
    p.add_argument("--gcp-agent-service-account-email",
                    help="email of the narrow GCP service account grain mints a fresh, "
                         "short-lived key for on every dispatched sandbox (bwsalmon/"
                         "agents#126) -- requires --gcp-project-id and "
                         "--gcp-key-minter-key-file")
    p.add_argument("--gcp-project-id",
                    help="GCP project id -- required with --gcp-agent-service-account-email")
    p.add_argument("--gcp-key-minter-key-file",
                    help="JSON key for the account that mints the agent account's "
                         "per-dispatch keys (the host service account) -- never the "
                         "agent account's own key")
    p.add_argument("--gcp-key-max-age-hours", type=int, default=24,
                    help="reap any --gcp-agent-service-account-email key older than this, "
                         "independent of whether its task session ever ended cleanly "
                         "(default: 24)")
    p.add_argument("--gemini-project-id",
                    help="enables the grain-gemini-key task label (bwsalmon/agents#47): the "
                         "GCP project a short-lived Gemini API key is minted in for a task "
                         "that carries it. Uses the minter key --gcp-key-minter-key-file, "
                         "impersonating the agent account -- pass that too")
    p.add_argument("--janitor-ttl-hours", type=int,
                    help="enables the GCP janitor (bwsalmon/agents#113): deletes GCE "
                         "instances, their unattached disks, and grain-minted Gemini API "
                         "keys older than this many hours, skipping the grain host VM, its "
                         "data disk, and anything labelled managed-by=terraform. Uses the "
                         "minter key --gcp-key-minter-key-file, impersonating the agent -- requires "
                         "--gcp-project-id too")
    p.add_argument("--janitor-name-prefix", default="grain",
                    help="must match this deployment's Terraform name_prefix (default: "
                         "grain) -- names the host/data-disk resources the janitor must "
                         "never delete")
    p.add_argument("--scheduled-job", action="append", default=[], metavar="NAME=FILE",
                    help="write a scheduled job's template file (bwsalmon/agents#163) to "
                         "/data/config/scheduled-jobs/NAME.md -- a Title/Interval-Hours/"
                         "Needs-Approval header block, a blank line, then the issue body "
                         "to file automatically once every Interval-Hours. FILE holds the "
                         "whole template, or '-' for stdin. Repeatable, once per job")
    p.add_argument("--scratch-repo-owner",
                    help="enables the grain-scratch-repo task label (bwsalmon/agents#159): "
                         "the account the grain-scratch-<sandbox> repos live under. The "
                         "credential that reaches them is an ordinary --github-key "
                         "NAME=FILE plus a credentials.json pattern added by hand -- see "
                         "docs/runbook.md, 'Enabling grain-scratch-repo'")
    p.add_argument("--scratch-repo-prefix", default="grain-scratch",
                    help="names the one repo dedicated to each sandbox: "
                         "<prefix>-<sandbox> (default: grain-scratch)")
    p.add_argument("--github-host", default="api.github.com",
                    help="REST API host override for a live test against a mock GitHub "
                         "server (default: api.github.com)")
    p.add_argument("--git-forward-host", default="github.com",
                    help="git-proxy's smart-HTTP forward-target override, same purpose as "
                         "--github-host (default: github.com)")
    p.add_argument("--github-insecure-http", action="store_true",
                    help="talk plain HTTP to --github-host/--git-forward-host instead of "
                         "HTTPS -- only for a mock server; never for the real API")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--admin-ssh-private-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.add_argument("--controller-provision", default=str(_REPO_ROOT / "provision" / "controller.sh"))
    p.add_argument("--sandbox-provision", default=str(_REPO_ROOT / "provision" / "sandbox.sh"))
    p.add_argument("--ssh-timeout", type=float, default=180.0,
                    help="seconds to wait for SSH per VM (default: 180)")
    p.set_defaults(func=cmd_host_bootstrap)

    automation = sub.add_parser(
        "automation", help="issue intake and dispatch"
    ).add_subparsers(dest="command", required=True)

    p = automation.add_parser(
        "run-once", help="sweep stranded work, then poll and dispatch once"
    )
    p.set_defaults(func=cmd_automation_run_once)

    p = automation.add_parser("status", help="show the current pool assignments")
    p.set_defaults(func=cmd_automation_status)

    sessions = sub.add_parser(
        "sessions", help="browse past dispatch sessions by trigger (docs/roadmap.md item 10)"
    ).add_subparsers(dest="command", required=True)

    p = sessions.add_parser(
        "list", help="list recorded sessions, optionally filtered"
    )
    p.add_argument("--kind", choices=["issue", "pr"], help="only this trigger kind")
    p.add_argument("--outcome", choices=["succeeded", "failed", "stranded"],
                    help="only this outcome")
    p.add_argument("--trigger", type=int, help="only sessions for this issue/PR number")
    p.set_defaults(func=cmd_sessions_list)

    p = sessions.add_parser(
        "browse", help="interactive text UI (curses) to browse sessions and view trajectories"
    )
    p.set_defaults(func=cmd_sessions_browse)

    github = sub.add_parser(
        "github", help="GitHub credential hardening"
    ).add_subparsers(dest="command", required=True)

    p = github.add_parser(
        "audit",
        help="check every credential under secrets/github for withheld scopes",
    )
    p.set_defaults(func=cmd_github_audit)

    controller = sub.add_parser(
        "controller", help="one-time per-deployment /data configuration"
    ).add_subparsers(dest="command", required=True)

    p = controller.add_parser(
        "configure",
        help="write automation.json, repo-allowlist.json, and (optionally) "
             "GitHub/Claude credentials to /data on the controller, over SSH",
    )
    p.add_argument("--task-repo", "--repo", dest="task_repo", required=True,
                    metavar="OWNER/NAME",
                    help="the task repo: the one repo polled for labelled issues "
                         "(--repo is accepted as its former name)")
    p.add_argument("--target-repo", dest="target_repos", action="append", default=[],
                    metavar="OWNER/NAME",
                    help="a repo tasks may dispatch into, named by a /repo directive on "
                         "an issue; repeatable. Omit entirely for a single-repo "
                         "deployment: the task repo becomes the only target and the "
                         "default for tasks with no /repo line")
    p.add_argument("--default-target-repo", metavar="OWNER/NAME",
                    help="target repo for a task with no /repo directive (default: none, "
                         "so a task without one is parked with a comment -- unless no "
                         "--target-repo was given at all, in which case the task repo)")
    p.add_argument("--github-token-file",
                    help="path to a file holding the GitHub token, or '-' for stdin")
    p.add_argument("--credential-name", default="bot",
                    help="credentials.json entry name for --github-token-file (default: bot)")
    p.add_argument("--github-key", action="append", default=[], metavar="NAME=FILE",
                    help="write an additional named GitHub credential (bwsalmon/agents#52): "
                         "a task labelled `grain-github-NAME` uses it for every git push "
                         "instead of the default credential -- for a scope (e.g. `workflow`) "
                         "the default deliberately withholds. FILE holds the token, or '-' "
                         "for stdin. Repeatable; never becomes any repo's default credential")
    p.add_argument("--claude-token-file",
                    help="path to a file holding a Claude Code OAuth token (from `claude "
                         "setup-token`) to place on the controller")
    p.add_argument("--gcp-agent-service-account-email",
                    help="email of the narrow GCP service account grain mints a fresh, "
                         "short-lived key for on every dispatched sandbox (bwsalmon/"
                         "agents#126) -- requires --gcp-project-id and "
                         "--gcp-key-minter-key-file")
    p.add_argument("--gcp-project-id",
                    help="GCP project id -- required with --gcp-agent-service-account-email")
    p.add_argument("--gcp-key-minter-key-file",
                    help="JSON key for the account that mints the agent account's "
                         "per-dispatch keys (the host service account) -- never the "
                         "agent account's own key")
    p.add_argument("--gcp-key-max-age-hours", type=int, default=24,
                    help="reap any --gcp-agent-service-account-email key older than this, "
                         "independent of whether its task session ever ended cleanly "
                         "(default: 24)")
    p.add_argument("--gemini-project-id",
                    help="enables the grain-gemini-key task label (bwsalmon/agents#47): the "
                         "GCP project a short-lived Gemini API key is minted in for a task "
                         "that carries it. Reuses the key --gcp-service-account-key-file "
                         "already placed -- run that first")
    p.add_argument("--janitor-ttl-hours", type=int,
                    help="enables the GCP janitor (bwsalmon/agents#113): deletes GCE "
                         "instances, their unattached disks, and grain-minted Gemini API "
                         "keys older than this many hours, skipping the grain host VM, its "
                         "data disk, and anything labelled managed-by=terraform. Uses the "
                         "minter key --gcp-key-minter-key-file, impersonating the agent -- requires "
                         "--gcp-project-id too")
    p.add_argument("--janitor-name-prefix", default="grain",
                    help="must match this deployment's Terraform name_prefix (default: "
                         "grain) -- names the host/data-disk resources the janitor must "
                         "never delete")
    p.add_argument("--scheduled-job", action="append", default=[], metavar="NAME=FILE",
                    help="write a scheduled job's template file (bwsalmon/agents#163) to "
                         "/data/config/scheduled-jobs/NAME.md -- a Title/Interval-Hours/"
                         "Needs-Approval header block, a blank line, then the issue body "
                         "to file automatically once every Interval-Hours. FILE holds the "
                         "whole template, or '-' for stdin. Repeatable, once per job")
    p.add_argument("--scratch-repo-owner",
                    help="enables the grain-scratch-repo task label (bwsalmon/agents#159): "
                         "the account the grain-scratch-<sandbox> repos live under. The "
                         "credential that reaches them is an ordinary --github-key "
                         "NAME=FILE plus a credentials.json pattern added by hand -- see "
                         "docs/runbook.md, 'Enabling grain-scratch-repo'")
    p.add_argument("--scratch-repo-prefix", default="grain-scratch",
                    help="names the one repo dedicated to each sandbox: "
                         "<prefix>-<sandbox> (default: grain-scratch)")
    p.add_argument("--github-host", default="api.github.com",
                    help="REST API host override for a live test against a mock GitHub "
                         "server (default: api.github.com)")
    p.add_argument("--git-forward-host", default="github.com",
                    help="git-proxy's smart-HTTP forward-target override, same purpose as "
                         "--github-host (default: github.com)")
    p.add_argument("--github-insecure-http", action="store_true",
                    help="talk plain HTTP to --github-host/--git-forward-host instead of "
                         "HTTPS -- only for a mock server; never for the real API")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_controller_configure)

    sandbox = sub.add_parser(
        "sandbox", help="direct admin access to one sandbox or the controller"
    ).add_subparsers(dest="command", required=True)

    p = sandbox.add_parser(
        "login",
        help="interactive SSH, using the admin key, for debugging "
             "(docs/bootstrap.md, 'grain sandbox login <name>')",
    )
    p.add_argument("name", help="VM name, e.g. 'sandbox-0' or 'controller'")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_sandbox_login)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
