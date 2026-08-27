"""GCP provisioning has two sides, in two repos, that must agree: the
Terraform module and its shell scripts live in this repo's own
terraform/gcp/, templates/gcp/ holds only the deployment's
configuration and the two workflows that pull terraform/gcp/ fresh at
CI time (see docs/roadmap.md and the "Remove duplicated code for gcp
provisioning templates" issue this split closed -- templates/gcp
used to vendor a full copy of terraform/gcp/, which drifted from this
one the moment either repo changed).

So there are still three sides that must agree -- Terraform declares
variables, config/grain.tfvars sets them, and the on-host deploy.sh reads
what Terraform puts in instance metadata -- they just live across two
directories now instead of one. Nothing in CI would catch a drift between
them until a deploy failed on a real VM, so check it here.

Everything is stdlib -- no terraform binary, no yaml -- so it runs
wherever the rest of the suite does.
"""

import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

from grain.inventory import Cluster

ROOT = Path(__file__).resolve().parent.parent
TEMPLATE = ROOT / "templates" / "gcp"
TERRAFORM = ROOT / "terraform" / "gcp"
DEPLOY_SH = TERRAFORM / "files" / "deploy.sh"
TFVARS = TEMPLATE / "config" / "grain.tfvars"
WORKFLOWS = TEMPLATE / ".github" / "workflows"
# Not on the host and not Terraform: the scripts a *config repo's* CI runs
# out of a grain checkout. deploy.yml is a file every deployment forks and
# then owns, so anything written there is something nobody re-syncs -- the
# step bodies live here instead and the workflow only wires them up.
CI = ROOT / "ci"
ENSURE_LABELS = CI / "ensure-task-labels.sh"
TERRAFORM_APPLY = CI / "terraform-apply.sh"
READ_OUTPUTS = CI / "read-terraform-outputs.sh"
PUSH_SECRETS = CI / "push-host-secrets.sh"

# The two steps allowed to keep a body in deploy.yml, because neither can
# come from grain: the first decides which grain to fetch and runs before
# there is one, and the second checks that fetch actually carries the
# scripts every other step needs.
_BOOTSTRAP_STEPS = {
    "Determine which grain ref to pull Terraform and scripts from",
    "Check grain has the deploy scripts this workflow runs",
}

SHELL_SCRIPTS = [
    TERRAFORM / "files" / "startup.sh",
    TERRAFORM / "files" / "config-sync.sh",
    DEPLOY_SH,
    TERRAFORM / "bootstrap-gcp.sh",
    *sorted(CI.glob("*.sh")),
]


def _deploy_run_directives():
    """(step name, the text after `run:`) for every shell step in
    deploy.yml, in order. A `|` value means the step has its own body."""
    directives, current = [], None
    for line in (WORKFLOWS / "deploy.yml").read_text().splitlines():
        named = re.match(r"      - name: (.+)", line)
        if named:
            current = named.group(1).strip()
        runs = re.match(r"        run: (.*)", line)
        if runs:
            directives.append((current, runs.group(1).strip()))
    return directives


def _scripts_deploy_yml_calls():
    """The ci/ scripts deploy.yml invokes, as repo-relative paths."""
    text = (WORKFLOWS / "deploy.yml").read_text()
    return set(re.findall(r"grain-src/(ci/[\w.-]+\.sh)", text))


def _preflight_script_names():
    """The ci/ scripts deploy.yml's preflight step checks for, as
    repo-relative paths -- it enumerates bare names in a `for` loop."""
    text = (WORKFLOWS / "deploy.yml").read_text()
    match = re.search(r"for script in (.*?); do", text, re.S)
    assert match, "deploy.yml no longer preflights the grain checkout"
    names = match.group(1).replace("\\", " ").split()
    return {f"ci/{name}.sh" for name in names}


def _tf_source():
    return "\n".join(p.read_text() for p in sorted(TERRAFORM.glob("*.tf")))


def _declared_variables():
    return set(re.findall(r'^variable "([^"]+)"', _tf_source(), re.M))


def _variables_with_defaults():
    declared = {}
    for block in re.split(r'^variable "', _tf_source(), flags=re.M)[1:]:
        name = block.split('"', 1)[0]
        declared[name] = re.search(r"^  default\s*=", block, re.M) is not None
    return declared


def _tfvars_assignments():
    return set(re.findall(r"^([a-z_][a-z0-9_]*)\s*=", TFVARS.read_text(), re.M))


def _config_renderer():
    """The python program deploy.sh embeds to turn grain-config into shell
    variables and cluster.toml."""
    match = re.search(r"python3 - <<'PY'.*?\n(.*?)\nPY\n", DEPLOY_SH.read_text(), re.S)
    assert match, "deploy.sh no longer embeds a python config renderer"
    return match.group(1)


def test_every_variable_the_tfvars_sets_is_declared():
    undeclared = _tfvars_assignments() - _declared_variables()
    assert not undeclared, f"config/grain.tfvars sets undeclared variables: {sorted(undeclared)}"


def test_every_variable_the_terraform_references_is_declared():
    referenced = set(re.findall(r"\bvar\.([a-z_][a-z0-9_]*)", _tf_source()))
    assert not referenced - _declared_variables()


def test_variables_without_a_default_are_set_in_the_tfvars():
    required = {n for n, has_default in _variables_with_defaults().items() if not has_default}
    # deploy_generation is CI's, passed with -var on every run.
    missing = required - _tfvars_assignments() - {"deploy_generation"}
    assert not missing, f"required variables with nowhere to come from: {sorted(missing)}"


def test_the_host_reads_every_key_terraform_puts_in_grain_config():
    """The likeliest drift: a field added to the grain_config local and
    never read on the host, or the reverse."""
    local = re.search(r"grain_config = \{(.*?)\n  \}", (TERRAFORM / "instance.tf").read_text(), re.S)
    assert local
    published = set(re.findall(r"^\s*([a-z_]+)\s*=", local.group(1), re.M))

    host_side = _config_renderer() + (TERRAFORM / "files" / "config-sync.sh").read_text()
    unread = {key for key in published if f'"{key}"' not in host_side}
    assert not unread, f"instance.tf publishes config the host never reads: {sorted(unread)}"


def test_config_sync_reads_the_deploy_timeout():
    assert "deploy_timeout_secs" in (TERRAFORM / "files" / "config-sync.sh").read_text()


def test_deploy_sh_passes_the_ssh_timeout_to_grain_host_bootstrap():
    """Found live: grain's own --ssh-timeout default (180s) wasn't enough
    for a cold nested-virt boot on a real cloud VM, and deploy.sh had no
    way to override it -- bootstrap_ssh_timeout_seconds closes that."""
    deploy_sh = DEPLOY_SH.read_text()
    assert "bootstrap_ssh_timeout_seconds" in deploy_sh
    assert "--ssh-timeout" in deploy_sh
    assert "BOOTSTRAP_SSH_TIMEOUT_SECONDS" in deploy_sh


def test_sync_source_retries_its_git_commands():
    """Found live: a real deploy failed with exit=128 (git's own
    fatal-error convention) on what was very likely a transient egress
    gap right at boot -- curl's fetch_base_image already retries 3x, but
    git has no equivalent built in, so sync_source's clone/fetch had none
    at all and failed permanently on the first hiccup.
    """
    deploy_sh = DEPLOY_SH.read_text()
    sync_source = re.search(r"^sync_source\(\) \{.*?\n\}", deploy_sh, re.S | re.M)
    assert sync_source, "sync_source() function not found"
    body = sync_source.group(0)
    assert "retry git clone" in body
    assert "retry git -C" in body and "fetch" in body


def _run_terraform_apply_script(fake_terraform):
    """Runs ci/terraform-apply.sh -- the actual script the workflow calls,
    no longer a body extracted out of the YAML -- against a stand-in
    `terraform` and a no-op `sleep`, so the retry/backoff logic is
    exercised for real without a GCP call or an actual wait."""
    with tempfile.TemporaryDirectory() as tmp:
        bin_dir = Path(tmp) / "bin"
        bin_dir.mkdir()
        (bin_dir / "terraform").write_text(fake_terraform)
        (bin_dir / "terraform").chmod(0o755)
        (bin_dir / "sleep").write_text("#!/usr/bin/env bash\nexit 0\n")
        (bin_dir / "sleep").chmod(0o755)

        env = {
            **os.environ,
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "DEPLOY_GENERATION": "test",
            # Only ever handed to the fake terraform, which ignores them.
            "CONFIG_DIR": tmp,
            "CONFIG_REPO": "an-org/a-repo",
        }
        return subprocess.run(
            [str(TERRAFORM_APPLY)], env=env, capture_output=True, text=True,
        )


# init and validate always succeed and are not counted: the retry budget
# is apply's alone, so counting the other two would mask a regression that
# retried the wrong command.
_FAKE_TERRAFORM_COUNTING = """#!/usr/bin/env bash
if [ "$1" != "apply" ]; then
  exit 0
fi
n_file="{n_file}"
n=$(cat "$n_file")
n=$((n + 1))
echo "$n" > "$n_file"
if [ "$n" -le {fail_count} ]; then
  echo "Error: {message}" >&2
  exit 1
fi
echo "Apply complete! Resources: 1 added, 0 changed, 0 destroyed."
"""


def test_deploy_yml_retries_terraform_apply_on_stockout():
    """Found live: GCP stock-outs (ZONE_RESOURCE_POOL_EXHAUSTED and its
    siblings) are a common, transient failure creating a VM or disk in a
    given zone, and used to fail the whole rollout outright -- a retry a
    few minutes later routinely succeeds once the zone frees up capacity.
    """
    with tempfile.NamedTemporaryFile() as n_file:
        Path(n_file.name).write_text("0")
        fake = _FAKE_TERRAFORM_COUNTING.format(
            n_file=n_file.name, fail_count=2,
            message="does not have enough resources available to fulfill the request",
        )
        result = _run_terraform_apply_script(fake)
        assert result.returncode == 0, result.stdout + result.stderr
        assert Path(n_file.name).read_text().strip() == "3", \
            "expected two failed attempts before the third succeeded"
        assert "retrying" in result.stdout


def test_deploy_yml_does_not_retry_a_non_stockout_terraform_failure():
    """Retrying a real config error or quota limit would just waste the
    backoff budget on a failure that will never clear itself."""
    with tempfile.NamedTemporaryFile() as n_file:
        Path(n_file.name).write_text("0")
        fake = _FAKE_TERRAFORM_COUNTING.format(
            n_file=n_file.name, fail_count=999,
            message="Invalid value for variable",
        )
        result = _run_terraform_apply_script(fake)
        assert result.returncode == 1
        assert Path(n_file.name).read_text().strip() == "1", \
            "a non-stockout failure should not be retried"


def test_deploy_yml_gives_up_on_terraform_apply_after_max_attempts():
    """A stock-out that never clears must not retry forever."""
    with tempfile.NamedTemporaryFile() as n_file:
        Path(n_file.name).write_text("0")
        fake = _FAKE_TERRAFORM_COUNTING.format(
            n_file=n_file.name, fail_count=999,
            message="RESOURCE_POOL_EXHAUSTED",
        )
        result = _run_terraform_apply_script(fake)
        assert result.returncode == 1
        attempts = int(Path(n_file.name).read_text().strip())
        assert 2 <= attempts <= 10, f"expected a bounded number of attempts, got {attempts}"


def test_run_bootstrap_dumps_storage_diagnostics_on_failure():
    """Found live: a "Cannot access storage file ... Permission denied"
    failure kept recurring through three straight AppArmor/SELinux fixes,
    and the operator hitting it lacked osLoginExternalUser -- so SSH/IAP
    couldn't confirm the actual file owner vs. what qemu.conf expects
    either. deploy.sh's own stdout, which already reaches Cloud Logging,
    is the one channel guaranteed reachable -- so a bootstrap failure
    dumps the storage ownership facts there instead of just a traceback.
    """
    deploy_sh = DEPLOY_SH.read_text()
    diag = re.search(r"^dump_storage_diagnostics\(\) \{.*?\n\}", deploy_sh, re.S | re.M)
    assert diag, "dump_storage_diagnostics() function not found"
    body = diag.group(0)
    assert "qemu.conf" in body
    assert "dynamic_ownership" in body
    assert "getent passwd" in body and "getent group" in body

    run_bootstrap = re.search(r"^run_bootstrap\(\) \{.*?\n\}", deploy_sh, re.S | re.M)
    assert run_bootstrap, "run_bootstrap() function not found"
    assert "dump_storage_diagnostics" in run_bootstrap.group(0), \
        "run_bootstrap never calls dump_storage_diagnostics on failure"


def test_storage_diagnostics_only_run_for_a_storage_shaped_failure():
    """Reported live: a "stage 5/11: wait for the controller" timeout --
    nothing to do with storage -- was followed by these ownership tables,
    because the dump ran on any non-zero exit at all. grain prints its own
    diagnostics for a failed boot wait now (grain/adapter/diagnostics.py);
    this one has to stay filed under the failure it was written for.

    Notably not a bare "Permission denied": SSH's own "Permission denied
    (publickey)", which is exactly what an unreachable controller prints,
    would match that and put the misfiling right back.
    """
    deploy_sh = DEPLOY_SH.read_text()
    assert "STORAGE_FAILURE_RE" in deploy_sh
    pattern = re.search(r"^readonly STORAGE_FAILURE_RE='(.*)'$", deploy_sh, re.M)
    assert pattern, "STORAGE_FAILURE_RE not found"
    alternatives = pattern.group(1).split("|")
    assert "Cannot access storage file" in alternatives
    assert "Permission denied" not in alternatives

    run_bootstrap = re.search(r"^run_bootstrap\(\) \{.*?\n\}", deploy_sh, re.S | re.M)
    body = run_bootstrap.group(0)
    assert 'grep -qE "$STORAGE_FAILURE_RE"' in body, \
        "run_bootstrap dumps storage diagnostics unconditionally again"
    assert "tee" in body, "the bootstrap output is not captured to classify the failure"


def test_grain_runs_unbuffered_so_a_killed_deploy_keeps_its_output():
    """Through a pipe python block-buffers stdout, so a deploy killed by
    config-sync's deploy_timeout_secs would lose the diagnostics printed
    just before the kill -- the ones such a deploy is read for.
    """
    assert "python3 -u -m grain.cli" in DEPLOY_SH.read_text()


def test_deploy_configures_ops_agent_and_ships_the_full_journal():
    """Found live: a real deploy failure was undiagnosable from CI's own
    guest-attribute summary (a bare "exit=N"), and journalctl -u
    grain-config-sync needs an SSH/IAP path neither the deploy identity
    nor an operator may actually have. Cloud Logging sidesteps both.
    """
    deploy = DEPLOY_SH.read_text()
    assert "google-cloud-ops-agent" in deploy
    assert "systemd_journald" in deploy
    assert "systemctl restart google-cloud-ops-agent" in deploy
    assert re.search(r"^ensure_ops_agent\n", deploy, re.M), \
        "ensure_ops_agent is never actually called"


def test_ops_agent_setup_is_not_boot_only():
    """Found live (bwsalmon/agents#68): this used to be a startup.sh step,
    which only runs at boot -- so a change to it (like adding
    controller_console for bwsalmon/agents#58) never reached an
    already-running host, and Cloud Logging silently stopped reflecting
    reality until someone rebooted the host by hand. deploy.sh is what
    config-sync re-fetches and re-runs on every config-repo push, and
    retries again on its own every few minutes if it fails -- the
    self-healing convergence Cloud Logging needs, the same reason
    `ensure_packages` (not startup.sh) installs the rest of this host's
    packages.
    """
    startup = (TERRAFORM / "files" / "startup.sh").read_text()
    assert "google-cloud-ops-agent" not in startup
    assert "install_ops_agent" not in startup


def test_shell_scripts_are_syntactically_valid_and_fail_fast():
    for script in SHELL_SCRIPTS:
        assert script.exists(), script
        assert "set -euo pipefail" in script.read_text(), f"{script.name} does not fail fast"
        subprocess.run(["bash", "-n", str(script)], check=True)


def test_the_rendered_cluster_file_is_one_grain_can_load():
    """deploy.sh writes cluster.toml from repo config; grain's own loader
    is the only judge of whether that file is valid."""
    with tempfile.TemporaryDirectory() as tmp:
        renderer = _config_renderer().replace("/run/grain-deploy", tmp)
        config = {
            "grain_repo_url": "https://github.com/bwsalmon/grain",
            "grain_ref": "main",
            "debian_image_url": "https://example.invalid/debian-12.qcow2",
            "sandbox_count": 3,
            "cluster_overrides": {"sandbox_mem_mb": 6144, "subnet": "10.100.0.0/24"},
            "task_repo": "an-org/agent-tasks",
            "target_repos": ["an-org/service-a", "an-org/service b"],
            "default_target_repo": "",
            "credential_name": "bot",
            "deploy_timeout_secs": 2700,
        }
        (Path(tmp) / "config.json").write_text(json.dumps(config))
        subprocess.run([sys.executable, "-c", renderer], check=True)

        cluster = Cluster.load(Path(tmp) / "cluster.toml")
        assert cluster.sandbox_count == 3
        assert cluster.sandbox_mem_mb == 6144
        assert cluster.image == "/var/lib/grain/images/debian-12.qcow2"

        # And the shell half: values reach bash intact, quoting and all.
        env = Path(tmp) / "env.sh"
        probe = subprocess.run(
            ["bash", "-c", f'set -euo pipefail; . {env}; printf "%s\\n" "$TASK_REPO" '
                           f'"${{#TARGET_REPOS[@]}}" "${{TARGET_REPOS[1]}}"'],
            capture_output=True, text=True, check=True,
        )
        assert probe.stdout.split("\n")[:3] == ["an-org/agent-tasks", "2", "an-org/service b"]


def test_no_secret_value_is_committed_or_passed_through_terraform():
    """The template's central claim. Terraform never touches Secret
    Manager or a secret value -- the two runtime credentials go straight
    into instance metadata over the Compute API instead -- and nothing in
    either repo holds a credential."""
    source = _tf_source()
    assert "secret_manager" not in source.lower()
    assert "secretmanager" not in source.lower()
    for path in list(TEMPLATE.rglob("*")) + list(TERRAFORM.rglob("*")):
        if path.is_file():
            text = path.read_text(errors="ignore")
            assert "-----BEGIN" not in text, f"{path} looks like it holds a key"
            assert not re.search(r"\bghp_[A-Za-z0-9]{20,}", text), f"{path} holds a GitHub token"


def test_the_config_repo_is_the_task_repo_by_default():
    """The queue is this repository unless the tfvars says otherwise, and
    CI is what supplies its name -- so neither can be dropped."""
    instance = (TERRAFORM / "instance.tf").read_text()
    assert re.search(r"task_repo\s*=\s*var\.task_repo\s*!=\s*\"\"\s*\?", instance), \
        "instance.tf no longer falls back from task_repo to config_repo"
    assert re.search(r"task_repo\s*=\s*local\.task_repo", instance), \
        "grain_config no longer publishes local.task_repo as task_repo"

    # plan.yml deliberately excluded: it never authenticates to GCP or runs
    # `terraform plan`/`apply` at all (only `fmt`/`validate`, neither of
    # which touches variables), specifically so a PR never gets a live
    # deployer credential before a human reviews it -- see plan.yml's own
    # module comment. Nothing there consumes config_repo.
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    assert "CONFIG_REPO: ${{ github.repository }}" in deploy, \
        "deploy.yml does not tell the apply script which repo it is running in"
    assert '-var="config_repo=$config_repo"' in TERRAFORM_APPLY.read_text(), \
        "terraform-apply.sh no longer passes config_repo through to Terraform"


def test_no_step_if_condition_references_the_secrets_context():
    """Found live: GitHub rejects this at parse time -- 'Unrecognized
    named-value: secrets' -- for every workflow in the repo, not just the
    one that shipped with it. The secrets context is available in env,
    with, and run, but never in a step's own if:; a value that needs to
    gate a step has to go through a job-level env: var instead (env *is*
    available in steps.if), the way deploy.yml's HAS_WIF does.
    """
    for path in WORKFLOWS.glob("*.yml"):
        for lineno, line in enumerate(path.read_text().splitlines(), start=1):
            if re.match(r"\s*if\s*:", line):
                assert "secrets." not in line, (
                    f"{path.name}:{lineno} references secrets directly in a "
                    "step if: condition -- GitHub will reject this file"
                )


def test_agent_compute_roles_exclude_the_grain_host_by_iam_condition():
    """instanceAdmin.v1 and osLogin both act on compute.googleapis.com/Instance
    resources and support resource.name conditions (Google's own documented
    pattern for this exact "except this one instance" shape) -- an agent
    must not be able to touch, add an SSH key to, or OS-Login into its own
    deployment's host VM.
    """
    source = _tf_source()
    roles_local = re.search(r"agent_conditioned_compute_roles\s*=.*?\]", source, re.S)
    assert roles_local, "agent_conditioned_compute_roles local not found"
    assert "roles/compute.instanceAdmin.v1" in roles_local.group(0)
    assert "roles/compute.osLogin" in roles_local.group(0)
    # iap.tunnelResourceAccessor deliberately excluded from this list --
    # see test_iap_tunnel_access_is_granted_project_wide_not_conditioned.
    assert "iap.tunnelResourceAccessor" not in roles_local.group(0)

    resource = re.search(
        r'resource "google_project_iam_member" "agent_compute" \{.*?\n\}',
        source, re.S,
    )
    assert resource, "agent_compute resource not found"
    body = resource.group(0)
    assert "condition {" in body
    assert r'resource.type != \"compute.googleapis.com/Instance\"' in body
    assert "local.grain_host_resource" in body

    host_local = re.search(r"grain_host_resource\s*=.*", source)
    assert host_local, "grain_host_resource local not found"
    assert "instances/${var.name_prefix}-host" in host_local.group(0)  # exact exclusion target


def test_iap_tunnel_access_is_granted_project_wide_not_conditioned():
    """Found live (a real GCP user's report): conditioning
    roles/iap.tunnelResourceAccessor to exclude one instance from a
    project-level grant does not reliably work -- it denied *all* tunnel
    access rather than excluding just the target. Deliberately
    unconditioned here; safe only because this role alone grants no
    authentication capability (the two conditioned roles above are what
    would let an agent actually log in). This test exists so a future
    edit adding a condition block to this specific resource gets noticed
    and re-verified against a real project, not silently trusted.
    """
    source = _tf_source()
    resource = re.search(
        r'resource "google_project_iam_member" "agent_iap_tunnel" \{.*?\n\}',
        source, re.S,
    )
    assert resource, "agent_iap_tunnel resource not found"
    assert "condition" not in resource.group(0)


def test_agent_account_exists_for_compute_only_mode_with_no_extra_roles():
    """agent_can_manage_compute_instances alone (agent_service_account_roles
    left empty) must still create the agent account -- gating solely on
    agent_service_account_roles would make this feature a no-op for
    anyone who wants compute access but no other role.
    """
    source = _tf_source()
    local = re.search(r"agent_account_needed\s*=\s*(.+)", source)
    assert local, "agent_account_needed local not found"
    assert "agent_can_manage_compute_instances" in local.group(1)
    assert "agent_service_account_roles" in local.group(1)
    # And every gate that decides whether google_service_account.agent[0]
    # exists has to use it -- the old inline expression drifting back in
    # anywhere would silently break the compute-only path again.
    assert "length(var.agent_service_account_roles) > 0 ?" not in source


def test_os_login_is_toggleable():
    """Found live: OS Login being on by default means every SSH session
    needs roles/compute.osLogin, or roles/compute.osLoginExternalUser (an
    organization-level grant) for an identity outside the project's org
    -- a real operator hit that wall with no self-service way out.
    enable_os_login has to exist and actually reach the instance's own
    metadata, not just be declared and ignored.
    """
    source = _tf_source()
    assert re.search(r'variable "enable_os_login" \{', source)
    assert "var.enable_os_login" in (TERRAFORM / "instance.tf").read_text()


def test_grain_config_publishes_the_agent_service_account_email():
    instance = (TERRAFORM / "instance.tf").read_text()
    assert "agent_service_account_email" in instance
    assert "agent_service_account_email" in DEPLOY_SH.read_text()


def test_grain_config_publishes_gemini_project_id():
    """bwsalmon/agents#49: enable_gemini_key has to reach deploy.sh the
    same way agent_service_account_email already does, or a
    Terraform-managed deployment has no way to turn the grain-gemini-key
    task label on short of a manual `controller configure` afterward.
    """
    instance = (TERRAFORM / "instance.tf").read_text()
    assert "gemini_project_id" in instance
    assert "var.enable_gemini_key" in instance
    deploy_sh = DEPLOY_SH.read_text()
    assert "gemini_project_id" in deploy_sh
    assert "--gemini-project-id" in deploy_sh
    # Only passed alongside the agent account's own key -- gemini_keys.py
    # authenticates gcloud with that same key, so a GEMINI_PROJECT_ID with
    # no key to go with it must never reach `host bootstrap`.
    assert re.search(
        r'MINTER_PLACED.*?GEMINI_PROJECT_ID.*?\bfi\b',
        deploy_sh, re.S,
    ), "--gemini-project-id must be nested inside the successful GCP key fetch"


def test_gemini_key_iam_is_gated_on_enable_gemini_key():
    """The IAM side of bwsalmon/agents#47's Gemini key feature: an
    operator turns it on declaratively with one Terraform variable rather
    than granting roles and enabling the API by hand (bwsalmon/agents#49).
    """
    source = _tf_source()
    local = re.search(r"agent_account_needed\s*=\s*(.+)", source)
    assert local, "agent_account_needed local not found"
    assert "enable_gemini_key" in local.group(1), (
        "enable_gemini_key alone (agent_service_account_roles left empty) "
        "must still create the agent account"
    )

    api = re.search(
        r'resource "google_project_service" "generativelanguage" \{.*?\n\}',
        source, re.S,
    )
    assert api, "google_project_service.generativelanguage not found"
    assert "var.enable_gemini_key" in api.group(0)
    assert "generativelanguage.googleapis.com" in api.group(0)
    # Never auto-disabled: turning the variable back off, or a
    # `terraform destroy`, must not reach into the project and disable an
    # API something else there might also depend on.
    assert "disable_on_destroy = false" in api.group(0)

    role = re.search(
        r'resource "google_project_iam_member" "host_gemini_keys" \{.*?\n\}',
        source, re.S,
    )
    assert role, "google_project_iam_member.host_gemini_keys not found"
    assert "var.enable_gemini_key" in role.group(0)
    assert "roles/serviceusage.apiKeysAdmin" in role.group(0)
    # bwsalmon/agents#131: the *host*, not the agent. Granted to the agent,
    # a sandbox holding a per-dispatch agent key could mint Gemini API keys
    # of its own without limit, and revoke the one grain minted for it --
    # which is the whole thing gemini_keys.py exists to control.
    assert "google_service_account.host.email" in role.group(0)
    assert "google_service_account.agent" not in role.group(0)


def test_a_sandbox_cannot_mint_its_own_gemini_keys():
    """The agent account is what a sandbox's credentials belong to, so any
    project-wide apiKeysAdmin grant to it hands every sandbox unbounded
    minting. Minting is the controller's job (as the host); a sandbox only
    ever receives the one key minted for its task.
    """
    source = _tf_source()
    for block in re.findall(r'resource "google_project_iam_member" "[^"]+" \{.*?\n\}',
                            source, re.S):
        if "roles/serviceusage.apiKeysAdmin" in block:
            assert "google_service_account.agent" not in block, (
                "apiKeysAdmin granted to the agent account: a sandbox could "
                "mint Gemini API keys of its own"
            )


def test_grain_config_publishes_janitor_settings():
    """bwsalmon/agents#113: enable_janitor/janitor_ttl_hours have to reach
    deploy.sh the same way enable_gemini_key/gemini_project_id already do,
    or a Terraform-managed deployment has no way to turn the janitor on
    short of a manual `controller configure` afterward. name_prefix must
    travel alongside it -- the janitor needs to know it to avoid deleting
    this deployment's own host/data-disk resources.
    """
    instance = (TERRAFORM / "instance.tf").read_text()
    assert "janitor_ttl_hours" in instance
    assert "var.enable_janitor" in instance
    assert "name_prefix" in instance

    deploy_sh = DEPLOY_SH.read_text()
    assert "janitor_ttl_hours" in deploy_sh
    assert "--janitor-ttl-hours" in deploy_sh
    assert "--janitor-name-prefix" in deploy_sh
    # Only passed alongside the agent account's own key -- janitor.py
    # authenticates gcloud with that same key, so a JANITOR_TTL_HOURS with
    # no key to go with it must never reach `host bootstrap`.
    assert re.search(
        r'MINTER_PLACED.*?JANITOR_TTL_HOURS.*?\bfi\b',
        deploy_sh, re.S,
    ), "--janitor-ttl-hours must be nested inside the successful GCP key fetch"


def test_janitor_iam_is_gated_on_enable_janitor():
    """The janitor reuses whatever IAM roles agent_can_manage_compute_
    instances/enable_gemini_key already grant the agent account (it has no
    resource type of its own to clean up beyond GCE instances/disks and
    Gemini API keys) -- so its own gate is only that the agent account
    itself gets created when enable_janitor is turned on alone.
    """
    source = _tf_source()
    local = re.search(r"agent_account_needed\s*=\s*(.+)", source)
    assert local, "agent_account_needed local not found"
    assert "enable_janitor" in local.group(1), (
        "enable_janitor alone (agent_can_manage_compute_instances and "
        "enable_gemini_key both left false) must still create the agent "
        "account, so the janitor has a key to authenticate gcloud with"
    )


def test_bootstrap_gcp_sh_grants_service_usage_admin():
    """serviceUsageConsumer alone (already granted) only covers *using* an
    already-enabled API -- enabling generativelanguage.googleapis.com via
    Terraform's google_project_service needs serviceUsageAdmin too, or
    `terraform apply` fails on a fresh project with enable_gemini_key set.
    """
    source = (TERRAFORM / "bootstrap-gcp.sh").read_text()
    assert "roles/serviceusage.serviceUsageAdmin" in source


def test_iam_grants_the_deployer_key_management_only_on_the_narrow_agent_account():
    """Found live: neither of the deployer's project-wide roles
    (bootstrap-gcp.sh's serviceAccountAdmin/serviceAccountUser) includes
    key management (iam.serviceAccountKeys.*) -- that needs its own role,
    and scoping it to just the agent account here, rather than granting it
    project-wide in bootstrap-gcp.sh, keeps the deployer from being able
    to mint keys for every service account in the project.
    """
    source = _tf_source()
    assert "serviceAccountKeyAdmin" in source
    assert re.search(
        r'resource "google_service_account_iam_member" "deployer_manages_agent_keys" \{'
        r'.*?service_account_id\s*=\s*google_service_account\.agent\[0\]\.name',
        source, re.S,
    ), "the deployer-manages-agent-keys binding must be scoped to the agent account"


def test_host_can_mint_and_revoke_the_agents_own_keys():
    """bwsalmon/agents#126: the controller (google_service_account.host)
    needs roles/iam.serviceAccountKeyAdmin on the agent account so
    grain/automation/gcp_keys.py can mint/revoke a fresh key per dispatch
    at runtime, with no static credential of its own -- distinct from
    deployer_manages_agent_keys above, which is CI's own one-time key
    mint for gemini_keys.py's unrelated primary credential.
    """
    source = _tf_source()
    resource = re.search(
        r'resource "google_service_account_iam_member" "host_manages_agent_keys" \{.*?\n\}',
        source, re.S,
    )
    assert resource, "host_manages_agent_keys resource not found"
    body = resource.group(0)
    assert "service_account_id" in body
    assert "google_service_account.agent[0].name" in body
    assert "roles/iam.serviceAccountKeyAdmin" in body
    assert "google_service_account.host.email" in body


def test_the_host_impersonates_the_agent_for_controller_side_work():
    """bwsalmon/agents#126 removed this grant along with the per-sandbox
    metadata-server broker that needed it, and a test here pinned it gone.
    bwsalmon/agents#131 brings it back for an unrelated reason, so that
    pin is replaced rather than deleted: the controller now authenticates
    as the host account -- its one credential -- and impersonates the
    agent for janitor.py's and gemini_keys.py's calls, which is what let
    the long-lived agent key come off the controller entirely.

    Scope matters as much as existence: impersonating the *agent* keeps
    the janitor's blast radius exactly the agent's own roles, which is its
    containment boundary (see janitor.py). A grant pointed anywhere else
    would quietly widen a tool that deletes by exclusion list.
    """
    source = _tf_source()
    assert 'resource "google_service_account_iam_member" "host_impersonates_agent"' in source
    body = source.split('"host_impersonates_agent"')[1].split("}")[0]
    assert "roles/iam.serviceAccountTokenCreator" in body
    assert "google_service_account.agent[0].name" in body, \
        "the controller must impersonate the agent account, nothing broader"
    assert "google_service_account.host.email" in body

    # The broker itself stays gone -- this grant is not its return.
    assert "gce_metadata_server" not in source


def test_read_outputs_step_sets_every_steps_tf_output_used_elsewhere():
    """Found live (issue #69): the "Push secrets to the host" step reads
    steps.tf.outputs.agent_service_account, but "Read outputs" never ran
    `terraform output ... agent_service_account` to populate it -- a
    GitHub Actions expression referencing an output nobody set just
    resolves to an empty string, no error, no warning. AGENT_SERVICE_ACCOUNT
    was therefore always empty, so the block that mints and pushes the
    agent's GCP key never ran, and every host waited out its optional
    secret budget and booted with no GCP access. Catch any output that's
    referenced but never captured, not just this one.
    """
    used = set(re.findall(r"steps\.tf\.outputs\.(\w+)",
                          (WORKFLOWS / "deploy.yml").read_text()))
    assert used, "deploy.yml consumes no terraform outputs at all any more"
    set_names = set(re.findall(r'echo "(\w+)=', READ_OUTPUTS.read_text()))
    missing = used - set_names
    assert not missing, (
        f"steps.tf.outputs referenced but never captured by "
        f"{READ_OUTPUTS.name}: {missing}"
    )


def test_deploy_yml_mints_and_invalidates_the_agent_key_only_when_one_exists():
    """Minted fresh every run, straight to instance metadata -- the
    short-lived-credential principle grain's own docs/design.md argues
    for at the sandbox layer, applied one layer up to the impersonation
    source. Keys older than the previous run's are deleted right after,
    or GCP's 10-key cap eventually breaks deploys -- see
    test_push_host_secrets_keeps_the_previous_key_alive_through_a_rotation
    for why the previous run's key is deliberately spared, not just this
    run's.
    """
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    assert "HOST_SERVICE_ACCOUNT: ${{ steps.tf.outputs.host_service_account }}" in deploy, (
        "deploy.yml no longer hands the host account to the push-secrets script"
    )
    push = PUSH_SECRETS.read_text()
    assert "gcloud iam service-accounts keys create" in push
    assert "gcloud iam service-accounts keys delete" in push
    assert 'if [ -n "$host_service_account" ]; then' in push, (
        "a deployment with no host account must not attempt to mint a key for one"
    )
    # bwsalmon/agents#131: the controller holds one credential. The
    # long-lived agent key this script used to push alongside it is gone --
    # nothing on the controller reads one, and leaving it would put a key
    # under the agent account that gcp_keys.py's reap cannot tell apart
    # from a per-dispatch key.
    assert "grain-agent-service-account-key" not in push


# A stand-in for `gcloud iam service-accounts keys ...` and `gcloud compute
# instances add-metadata`, tracking existing keys as one id per line in
# __STATE__ (append order == creation order) and every invocation in
# __CALLS__, so the rotation logic can be exercised without a real project.
_FAKE_GCLOUD = """#!/usr/bin/env bash
set -euo pipefail
STATE="__STATE__"
CALLS="__CALLS__"
echo "$*" >> "$CALLS"
case "$1 $2 $3" in
  "iam service-accounts keys")
    case "$4" in
      create)
        key_file="$5"
        n=$(( $(wc -l < "$STATE" 2>/dev/null || echo 0) + 1 ))
        new_id="key$n"
        echo "fake-key-content" > "$key_file"
        echo "$new_id" >> "$STATE"
        echo "$new_id"
        ;;
      list)
        tac "$STATE" 2>/dev/null || true
        ;;
      delete)
        key_id="$5"
        grep -v "^${key_id}$" "$STATE" > "$STATE.tmp" 2>/dev/null || true
        mv "$STATE.tmp" "$STATE"
        ;;
    esac
    ;;
  "compute instances add-metadata")
    # Only used by tests that care what actually got pushed (the named-
    # GitHub-key merge tests below) -- $METADATA_DIR is unset for the
    # rotation test above, and this block is then a no-op, same as before.
    if [ -n "${METADATA_DIR:-}" ]; then
      for arg in "$@"; do
        case "$arg" in
          --metadata-from-file=*)
            entry="${arg#--metadata-from-file=}"
            cp "${entry#*=}" "$METADATA_DIR/${entry%%=*}"
            ;;
        esac
      done
    fi
    ;;
esac
"""


def test_push_host_secrets_keeps_the_previous_key_alive_through_a_rotation():
    """Regression test for bwsalmon/agents#93.

    terraform-apply.sh bumps grain-deploy-generation -- which is what
    wakes config-sync up -- *before* this script ever runs, so a host can
    race ahead and re-converge on whatever agent-service-account key is
    already sitting in instance metadata, not necessarily the one this
    run is about to mint (deploy.sh's fetch_secret_to_file has no way to
    tell a stale metadata value from a fresh one; it just reads whatever
    is there). Deleting every key but the brand new one immediately, as
    this script used to, could delete exactly the key such a host just
    started relying on -- and since nothing re-triggers config-sync until
    the next generation bump, every gcloud call on that host then failed
    with `invalid_grant: Invalid JWT Signature` forever, not transiently.
    Keeping the previous run's key alongside the new one closes that
    race: a host that raced ahead stays valid through this run and picks
    up the correct key on the next one.
    """
    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)
        bin_dir = tmp_path / "bin"
        bin_dir.mkdir()
        state = tmp_path / "keys.state"
        calls = tmp_path / "calls.log"
        # Two keys already exist -- the steady state this script's own
        # rotation leaves behind: an older one and the previous run's.
        state.write_text("key1\nkey2\n")

        fake = _FAKE_GCLOUD.replace("__STATE__", str(state)).replace("__CALLS__", str(calls))
        (bin_dir / "gcloud").write_text(fake)
        (bin_dir / "gcloud").chmod(0o755)

        env = {
            **os.environ,
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "PROJECT": "a-project",
            "INSTANCE": "an-instance",
            "ZONE": "us-central1-a",
            "HOST_SERVICE_ACCOUNT": "host@a-project.iam.gserviceaccount.com",
        }
        result = subprocess.run(
            [str(PUSH_SECRETS)], env=env, capture_output=True, text=True,
        )
        assert result.returncode == 0, result.stdout + result.stderr

        remaining = state.read_text().split()
        assert remaining == ["key2", "key3"], (
            "expected the previous run's key (key2) and the new key (key3) to "
            f"survive the rotation, got {remaining}"
        )
        assert "invalidated previous minter key: key1" in result.stdout
        assert "invalidated previous minter key: key2" not in result.stdout

        list_calls = [line for line in calls.read_text().splitlines()
                      if line.startswith("iam service-accounts keys list")]
        assert list_calls, "the script never listed existing keys"
        assert "--sort-by=~validAfterTime" in list_calls[0], (
            "the key list must be sorted newest-first for 'keep the two newest' to be correct"
        )


def test_push_host_secrets_merges_per_secret_named_github_keys():
    """bwsalmon/agents#187: GRAIN_GITHUB_KEYS packs every named credential
    into one Actions secret, which makes adding or removing a single name a
    hand-edit of a blob that also holds every other name's token.
    GITHUB_SECRETS_JSON (`toJSON(secrets)`, the only way Actions exposes a
    secret by name pattern) lets each name be its own
    `GRAIN_GITHUB_KEY_<NAME>` secret instead. This checks the two are
    merged into the one NAME=TOKEN blob deploy.sh on the host already knows
    how to split -- so a deployment that upgrades keeps its existing
    GRAIN_GITHUB_KEYS working, and a name in either place reaches the host.
    """
    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)
        bin_dir = tmp_path / "bin"
        bin_dir.mkdir()
        metadata_dir = tmp_path / "metadata"
        metadata_dir.mkdir()
        state = tmp_path / "keys.state"
        calls = tmp_path / "calls.log"

        fake = _FAKE_GCLOUD.replace("__STATE__", str(state)).replace("__CALLS__", str(calls))
        (bin_dir / "gcloud").write_text(fake)
        (bin_dir / "gcloud").chmod(0o755)

        env = {
            **os.environ,
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "PROJECT": "a-project",
            "INSTANCE": "an-instance",
            "ZONE": "us-central1-a",
            "METADATA_DIR": str(metadata_dir),
            "GRAIN_GITHUB_TOKEN": "ghp_default_secret",
            "GRAIN_GITHUB_KEYS": "legacy=ghp_legacy_secret",
            "GITHUB_SECRETS_JSON": json.dumps({
                "GRAIN_GITHUB_KEY_WORKFLOW": "ghp_workflow_secret",
                "GRAIN_GITHUB_KEY_RELEASE": "ghp_release_secret",
                # Not a name: this is what GRAIN_GITHUB_TOKEN itself looks
                # like in the same dump, and must not leak into the merged
                # named-key blob just because it starts similarly.
                "GRAIN_GITHUB_TOKEN": "ghp_default_secret",
            }),
        }
        result = subprocess.run(
            [str(PUSH_SECRETS)], env=env, capture_output=True, text=True,
        )
        assert result.returncode == 0, result.stdout + result.stderr

        pushed = (metadata_dir / "grain-github-keys").read_text()
        pairs = dict(line.split("=", 1) for line in pushed.splitlines() if line.strip())
        assert pairs == {
            "legacy": "ghp_legacy_secret",
            "workflow": "ghp_workflow_secret",
            "release": "ghp_release_secret",
        }

        token_pushed = (metadata_dir / "grain-github-token").read_text()
        assert token_pushed == "ghp_default_secret"


def test_deploy_yml_never_stores_the_agent_key_as_a_repo_secret():
    """The whole point: unlike GRAIN_GITHUB_TOKEN/GRAIN_CLAUDE_CODE_OAUTH_TOKEN,
    this credential is minted fresh every run and never round-trips
    through a GitHub Actions secret.
    """
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    assert "secrets.GCP_AGENT" not in deploy
    assert "secrets.GRAIN_AGENT" not in deploy


def test_deploy_sh_only_requests_the_gcp_key_when_an_agent_account_is_configured():
    deploy_sh = DEPLOY_SH.read_text()
    assert "MINTER_KEY_ATTR" in deploy_sh
    assert "--gcp-key-minter-key-file" in deploy_sh
    assert "--gcp-agent-service-account-email" in deploy_sh
    assert "--gcp-project-id" in deploy_sh
    assert 'if [ -n "$AGENT_SERVICE_ACCOUNT_EMAIL" ]; then' in deploy_sh


def test_the_deploy_workflow_creates_the_task_labels_from_grains_own_list():
    """Every label this deployment runs on has to exist in the queue repo,
    and a config repo *is* the queue repo -- so its deploy workflow creates
    them. What it must not do is carry the list: this workflow is the file
    every deployment forks and then owns, so a list written into it is one
    nobody re-syncs, and the label added in grain goes on not existing in
    any picker (which is what happened to `grain-agent-completed`,
    `grain-self-debug` and `grain-gemini-key`). It delegates to grain's
    ci/ensure-task-labels.sh instead, out of the grain-src checkout it
    already makes -- so a new label ships with a grain_ref bump.

    Which labels that script creates is grain's own business and pinned in
    tests/test_automation_labels.py, against AutomationConfig's fields.
    """
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    assert "grain-src/ci/ensure-task-labels.sh" in deploy, \
        "deploy.yml no longer runs the label script from the grain checkout"
    assert "gh label create" not in deploy, \
        "deploy.yml has grown its own copy of the label list again"


def test_every_script_deploy_yml_calls_exists_and_is_executable():
    """deploy.yml runs these directly rather than through `bash`, so a file
    committed without the executable bit fails the deploy with nothing but
    "Permission denied" -- and the paths are hardcoded in a workflow this
    repo does not run, so a rename here is invisible until a deploy breaks.
    """
    called = _scripts_deploy_yml_calls()
    assert called, "deploy.yml calls no ci/ scripts at all any more"
    for rel in sorted(called):
        script = ROOT / rel
        assert script.exists(), f"deploy.yml calls {rel}, which does not exist"
        assert os.access(script, os.X_OK), f"{rel} is not executable"


def test_deploy_yml_keeps_no_step_bodies_of_its_own():
    """The rule this whole split exists to hold. A config repo forks
    deploy.yml and then owns its copy, so any logic written there is logic
    nobody re-syncs -- which is how a null-safe agent_service_account
    output and a stock-out retry both sat in the template for months
    without reaching a live deployment. Every step therefore either calls
    a script out of the grain checkout, or is one of the two bootstrap
    steps that cannot (see _BOOTSTRAP_STEPS).
    """
    for name, directive in _deploy_run_directives():
        if directive == "|":
            assert name in _BOOTSTRAP_STEPS, (
                f"step {name!r} has grown a body of its own in deploy.yml; "
                "put it in ci/ and call it, or a fork will freeze this copy"
            )
        else:
            assert directive.startswith("grain-src/ci/"), (
                f"step {name!r} runs {directive!r} inline rather than a ci/ script"
            )


def test_the_preflight_lists_exactly_the_scripts_the_workflow_calls():
    """The preflight turns an old grain_ref into one clear error instead of
    a mid-rollout "No such file or directory". That only holds while its
    list matches reality: a step added with a script missing from the list
    goes back to failing late, and a name left behind after a rename fails
    every deploy against a grain that is perfectly fine.
    """
    listed, called = _preflight_script_names(), _scripts_deploy_yml_calls()
    assert listed == called, (
        f"preflight checks for scripts the workflow never runs: {listed - called}; "
        f"runs scripts the preflight never checks: {called - listed}"
    )


def test_the_ci_scripts_do_not_depend_on_the_config_repo_layout():
    """A script reaching for `config/` or `grain-src/` by name would put
    the config repo's layout back into grain, which is the same coupling
    from the other side: the workflow passes CONFIG_DIR and the script
    finds grain relative to itself.
    """
    for script in sorted(CI.glob("*.sh")):
        body = script.read_text()
        assert "grain-src" not in body, \
            f"{script.name} names the checkout path its caller chose"
        assert not re.search(r"(?<![\w/$\"])config/(backend\.hcl|grain\.tfvars)", body), \
            f"{script.name} hardcodes a config-repo path instead of using CONFIG_DIR"


def test_config_repo_template_vendors_no_terraform_or_scripts():
    """The whole point of this split: a fork of templates/gcp must
    never again carry its own copy of the Terraform module or its
    scripts, or it silently drifts from terraform/gcp/ the moment either
    repo changes -- which is exactly the bug this test suite exists to
    catch before a deploy does.
    """
    assert not (TEMPLATE / "terraform").exists(), \
        "templates/gcp/terraform/ has come back -- Terraform belongs only in terraform/gcp/"
    assert not (TEMPLATE / "scripts").exists(), \
        "templates/gcp/scripts/ has come back -- scripts belong only in terraform/gcp/"


def test_both_workflows_pull_terraform_from_grain_at_the_tfvars_ref():
    """Neither workflow may vendor the Terraform module -- both must check
    out bwsalmon/grain fresh, at whatever ref config/grain.tfvars names,
    into the exact path the later Terraform steps then use.
    """
    for name in ("plan.yml", "deploy.yml"):
        workflow = (WORKFLOWS / name).read_text()
        assert "repository: bwsalmon/grain" in workflow, \
            f"{name} does not check out grain at all"
        assert "path: grain-src" in workflow, \
            f"{name} does not check grain out to grain-src"
        assert "grain_ref" in workflow, \
            f"{name} does not read grain_ref out of config/grain.tfvars"
        # No local terraform/ directory to run any of this from.
        assert re.search(r"working-directory:\s*terraform\b", workflow) is None, \
            f"{name} still points at a vendored terraform/ directory"

    # plan.yml drives Terraform itself (fmt/validate only, deliberately
    # never authenticated), so it names the module directly. deploy.yml
    # names no grain path but ci/: its scripts cd to the module themselves,
    # which is why grain can move terraform/gcp without breaking a fork.
    assert "grain-src/terraform/gcp" in (WORKFLOWS / "plan.yml").read_text(), \
        "plan.yml never points a Terraform step at the checked-out module"
    assert "grain-src/terraform/gcp" not in (WORKFLOWS / "deploy.yml").read_text(), \
        "deploy.yml has gone back to hardcoding grain's internal layout"


def test_deploy_yml_passes_absolute_config_paths_to_terraform():
    """working-directory for every Terraform step is grain-src/terraform/gcp
    now, not this repo's own terraform/ -- a relative ../config/... path
    would resolve inside the grain-src checkout instead of this repo's
    config/, so the var-file and backend-config flags have to be anchored
    with github.workspace instead.
    """
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    assert "CONFIG_DIR: ${{ github.workspace }}/config" in deploy, \
        "deploy.yml no longer anchors the config directory to github.workspace"
    apply = TERRAFORM_APPLY.read_text()
    assert '-backend-config="$config_dir/backend.hcl"' in apply
    assert '-var-file="$config_dir/grain.tfvars"' in apply
    assert "../config/" not in apply, \
        "a relative path here resolves inside the grain checkout, not the config repo"


def test_the_controller_gets_a_minter_key_that_is_not_the_agent_account():
    """bwsalmon/agents#131. gcp_keys.py mints the agent account's
    per-dispatch keys, so it must authenticate as something else -- a
    leaked agent key must not be able to mint its own replacement. The
    whole chain has to name the *host* account: CI mints its key, deploy.sh
    places it, and the two key files stay distinct on disk.
    """
    push = PUSH_SECRETS.read_text()
    assert "grain-key-minter-key" in push, "CI never pushes a minter key"
    assert "--iam-account=\"$host_service_account\"" in push, \
        "the minter key is not minted for the host account"

    outputs = READ_OUTPUTS.read_text()
    assert "host_service_account" in outputs, \
        "the host account is never captured for the push step to use"

    deploy_sh = DEPLOY_SH.read_text()
    assert "grain-key-minter-key" in deploy_sh
    assert "--gcp-key-minter-key-file" in deploy_sh, \
        "deploy.sh never hands the minter key to `grain host bootstrap`"
    # bwsalmon/agents#131: one credential on the controller, the host's.
    assert "gcp-key-minter.json" in deploy_sh
    assert "gcp-service-account.json" not in deploy_sh, \
        "the long-lived agent key is back on the controller"


def test_terraform_lets_ci_mint_the_minter_key():
    """The grant that makes the above possible: without keyAdmin on the
    host account, CI's `keys create` fails and the controller never gets a
    credential at all."""
    iam = (TERRAFORM / "iam.tf").read_text()
    assert 'resource "google_service_account_iam_member" "deployer_manages_host_keys"' in iam
    minter_block = iam.split('"deployer_manages_host_keys"')[1].split("}")[0]
    assert "google_service_account.host.name" in minter_block, \
        "the grant is not scoped to the host account"
    assert "roles/iam.serviceAccountKeyAdmin" in minter_block


# Env values the runner sets itself, or that a ci/ script treats as a
# genuinely optional tuning knob with a working default. Everything else a
# script reads has to be passed by the workflow -- adding a name here is a
# deliberate "this one really is optional", not a way past the test.
_RUNNER_PROVIDED = {"GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY", "GITHUB_REPOSITORY"}
_OPTIONAL_TUNING = {"TF_APPLY_MAX_ATTEMPTS", "TF_APPLY_RETRY_DELAY", "POLL_SECONDS"}


def _env_reads(script: Path) -> set[str]:
    """Env vars a ci/ script reads, by their `${VAR:-}`/`${VAR:?}` sites --
    which is exactly where each script takes a value from its caller."""
    return set(re.findall(r"\$\{([A-Z][A-Z0-9_]*):[-?]", script.read_text()))


def test_the_workflow_passes_every_value_the_ci_scripts_read():
    """bwsalmon/agents#140: moving the step bodies into ci/ (grain#78) took
    the *logic* out of the forked workflow, but the values those scripts
    read still come from it. HOST_SERVICE_ACCOUNT and GRAIN_GITHUB_KEYS
    were each added to a script and to this template but not to a live
    config repo's own copy, and neither failed loudly -- the minter key was
    silently never minted, and named GitHub keys silently did nothing.

    This catches the template half at PR time: a script that starts reading
    a value the workflow never passes fails here. It cannot catch a fork
    that falls behind -- push-host-secrets.sh's own runtime warning is what
    covers that -- but it does stop the template from being the thing that
    is wrong.
    """
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    for script in sorted(CI.glob("*.sh")):
        for name in sorted(_env_reads(script)):
            if name in _RUNNER_PROVIDED or name in _OPTIONAL_TUNING:
                continue
            assert re.search(rf"^\s*{name}:", deploy, re.M), (
                f"{script.name} reads ${name}, but deploy.yml never passes it -- "
                "either wire it into the step's env: block, or add it to "
                "_OPTIONAL_TUNING if it genuinely has a working default"
            )
