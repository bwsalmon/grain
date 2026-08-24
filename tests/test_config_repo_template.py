"""The config repo template has three sides that must agree: Terraform
declares variables, config/grain.tfvars sets them, and the on-host
deploy.sh reads what Terraform puts in instance metadata. Nothing in CI
would catch a drift between them until a deploy failed on a real VM, so
check it here.

Everything is stdlib -- no terraform binary, no yaml -- so it runs
wherever the rest of the suite does.
"""

import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path

from grain.automation.config import AutomationConfig
from grain.inventory import Cluster

TEMPLATE = Path(__file__).resolve().parent.parent / "config-repo-template"
TERRAFORM = TEMPLATE / "terraform"
DEPLOY_SH = TERRAFORM / "files" / "deploy.sh"
TFVARS = TEMPLATE / "config" / "grain.tfvars"
WORKFLOWS = TEMPLATE / ".github" / "workflows"

SHELL_SCRIPTS = [
    TERRAFORM / "files" / "startup.sh",
    TERRAFORM / "files" / "config-sync.sh",
    DEPLOY_SH,
    TEMPLATE / "scripts" / "bootstrap-gcp.sh",
]


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
    the repo holds a credential."""
    source = _tf_source()
    assert "secret_manager" not in source.lower()
    assert "secretmanager" not in source.lower()
    for path in TEMPLATE.rglob("*"):
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
    assert "config_repo=${{ github.repository }}" in deploy, \
        "deploy.yml does not tell Terraform which repo it is running in"


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


def test_the_deploy_workflow_creates_the_labels_the_orchestrator_moves():
    """Every label grain's automation applies has to exist in the queue
    repo, and this repo *is* the queue repo -- so the workflow creates
    them. A renamed default here would otherwise strand the queue."""
    deploy = (WORKFLOWS / "deploy.yml").read_text()
    config = AutomationConfig(task_owner="an-org", task_repo="a-repo")
    for label in (config.trigger_label, config.in_progress_label,
                  config.awaiting_reply_label):
        assert f"label {label} " in deploy, f"deploy.yml never creates {label!r}"
