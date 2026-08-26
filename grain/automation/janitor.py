"""The GCP janitor (bwsalmon/agents#113): deletes GCE instances, their
unattached disks, and grain-minted Gemini API keys that have sat around
longer than `JanitorConfig.ttl_hours`, so a sandboxed agent that creates
cloud infrastructure as part of a task (docs/roadmap.md item 16 already
tells every dispatched agent to fold its own id into anything it names) and
then never cleans it up -- a crashed run, a task that just forgot -- does
not leak it forever.

**Why `gcloud`, not a client library, and why authenticated the same way
Gemini keys are.** Same reasoning as `gemini_keys.py`'s own docstring, not
restated in full here: this project is stdlib-only Python, the controller
already tolerates one runtime dependency of this kind, and `gcloud` is what
an operator running the equivalent cleanup by hand would reach for anyway.
`_activate` below authenticates with the exact same
`/data/secrets/gcp-service-account.json` key `gemini_keys.py` and
already use -- the *host* account's key, from which this impersonates the
grain-agent account (`--impersonate-service-account`, bwsalmon/agents#131)
-- so this janitor can only ever do what the agent's IAM roles allow it
to do: delete a
compute instance if `agent_can_manage_compute_instances` granted
`roles/compute.instanceAdmin.v1`, revoke an API key if `enable_gemini_key`
granted `roles/serviceusage.apiKeysAdmin`. Turning `enable_janitor` on
without either of those just means each pass logs a listing failure for
that resource kind and cleans up nothing of that kind -- there is nothing
for it to reach.

**Safety model: an exclusion list, not an inclusion list.** Nothing in this
deployment labels or names a resource an agent creates as "agent-created"
(docs/roadmap.md item 16's agent-id convention is a prompt sentence an
agent may or may not act on, never enforced or persisted) -- so there is no
positive signal this code could use to recognise agent-created
infrastructure. What *is* reliable is the other direction: every piece of
grain's own core infrastructure is named and labelled by Terraform
(`terraform/gcp/instance.tf`), so this janitor instead assumes everything
in the project older than the TTL is fair game *except* what it can
positively identify as grain's own -- the host VM and its data disk, by the
exact name Terraform gives them (`<name_prefix>-host`/`<name_prefix>-data`),
and anything carrying the `managed-by=terraform` label Terraform applies to
the host instance, its boot disk, and the data disk. Two independent checks
rather than one, because either alone has a plausible gap: a name-only
check misses a differently-named piece of core infrastructure added later,
a label-only check misses everything if an operator ever overrides
`var.labels` away from its default.

**Why the IAM condition on `agent_can_manage_compute_instances` is not
enough by itself.** `terraform/gcp/iam.tf`'s `agent_compute` grant already
excludes the grain host VM *instance* by IAM condition -- but that
condition is scoped to `compute.googleapis.com/Instance` resources only,
and does not (GCP does not support conditioning a persistent-disk resource
the same way) cover `google_compute_disk.data`, the disk holding every
credential and all automation state. This module's own name/label
exclusion is what actually protects that disk; the IAM condition on the
instance is a second, independent guard on top of it, not a substitute.

**Gemini keys are scoped by display-name prefix, not just age.** `core.py`
mints a key's display name as `grain-<sandbox>-issue-<n>` (see
`gemini_keys.py`'s own docstring); this janitor only ever considers
deleting a key whose display name starts with `<name_prefix>-`, so a
project with unrelated API keys for other purposes is never touched no
matter how old they are.

**Never raises.** A listing failure (permission denied, the API not
enabled, a malformed response) or a single deletion failure is recorded as
a warning and the pass moves on -- the same "one item's failure must not
crash the whole cycle" discipline `gemini_keys.py`'s own lookup and
`sweeper.py`'s health/credential warnings already hold to. `core.py`'s
`_janitor` turns each into an audit-log line, the same visibility-only
treatment those warnings already get.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from pathlib import Path

from ..run import CommandError, Runner

# Restated, not imported, from gemini_keys.py -- the two modules are never
# imported into each other (same "kept in sync by hand" precedent
# configure.py's own path constants already set).
# configure.py's GCP_KEY_MINTER_KEY_PATH, restated here rather than
# imported (the same "kept in sync by hand" precedent configure.py's own
# path constants already set). The host account's key: the controller's
# single GCP credential since bwsalmon/agents#131 -- this module acts as
# the *agent* account by impersonating it, not by holding its key.
_DEFAULT_KEY_PATH = Path("/data/secrets/gcp-key-minter.json")

DEFAULT_TTL_HOURS = 24
DEFAULT_NAME_PREFIX = "grain"
DEFAULT_PROTECTED_LABEL_KEY = "managed-by"
DEFAULT_PROTECTED_LABEL_VALUE = "terraform"


@dataclass(frozen=True)
class JanitorConfig:
    """Same `/data/config`-JSON-file shape as `GeminiKeyConfig` -- its
    presence (or absence) at `/data/config/janitor.json` is the on/off
    switch for the whole feature: `core.py`'s `_janitor` is a no-op when
    this is `None`.
    """

    project_id: str
    ttl_hours: int = DEFAULT_TTL_HOURS
    # bwsalmon/agents#131: the *host* account's key -- the controller's one
    # credential. This module still acts as the agent account, but by
    # impersonating it per call (`impersonate_service_account` below)
    # rather than holding its key: the controller no longer has one.
    key_path: Path = _DEFAULT_KEY_PATH
    # The agent account to act as. Unset means "act as whoever the key
    # file is", which is how this behaved before impersonation.
    impersonate_service_account: str | None = None
    # Must match terraform/gcp's var.name_prefix for a Terraform-managed
    # deployment -- what names the host VM (`<name_prefix>-host`) and its
    # data disk (`<name_prefix>-data`), the two resources this janitor must
    # never delete regardless of age.
    name_prefix: str = DEFAULT_NAME_PREFIX
    protected_label_key: str = DEFAULT_PROTECTED_LABEL_KEY
    protected_label_value: str = DEFAULT_PROTECTED_LABEL_VALUE

    @classmethod
    def load(cls, path: Path) -> "JanitorConfig":
        raw = json.loads(path.read_text())
        if "key_path" in raw:
            raw["key_path"] = Path(raw["key_path"])
        return cls(**raw)


@dataclass(frozen=True)
class DeletedResource:
    kind: str  # "instance" | "disk" | "gemini-key"
    name: str


@dataclass(frozen=True)
class JanitorWarning:
    kind: str
    name: str
    detail: str


@dataclass(frozen=True)
class JanitorResult:
    deleted: list[DeletedResource] = field(default_factory=list)
    warnings: list[JanitorWarning] = field(default_factory=list)


def _activate(runner: Runner, config: JanitorConfig) -> None:
    runner.run([
        "gcloud", "auth", "activate-service-account",
        f"--key-file={config.key_path}", "--quiet",
    ])


def _impersonated(config) -> list[str]:
    """The impersonation flag every gcloud call here carries, or nothing.

    bwsalmon/agents#131: the controller authenticates as the *host* account
    (the one credential it holds), then acts as the agent account for the
    duration of each call. That keeps this code's effective permissions
    exactly the agent's -- unchanged from when it held a long-lived agent
    key file -- while removing that key from the controller entirely. The
    flag is per-command rather than `gcloud config set
    auth/impersonate_service_account`, which would be process-global and so
    would silently apply to `gcp_keys.py` too, whose whole point is to act
    as the host and *not* the agent.

    Empty when unset, so a deployment configured before this change (or a
    test) still behaves exactly as it did.
    """
    target = getattr(config, "impersonate_service_account", None)
    if not target:
        return []
    return [f"--impersonate-service-account={target}"]


def _list_resources(runner: Runner, argv: list[str]) -> tuple[list[dict], str | None]:
    """Runs a `gcloud ... list --format=json` command without raising --
    a listing failure (permission denied because the feature that would
    grant it is off, the API not enabled, gcloud itself missing) means
    "nothing of this kind to clean up this pass," reported as a warning,
    never a crashed cycle.
    """
    result = runner.run(argv, check=False)
    if result.returncode != 0:
        return [], (result.stderr.strip() or f"gcloud exited {result.returncode}")
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        return [], f"could not parse as JSON ({exc}): {result.stdout!r}"
    if not isinstance(data, list):
        return [], f"expected a JSON array, got {type(data).__name__}: {result.stdout!r}"
    return data, None


def _older_than(timestamp: str | None, cutoff: datetime) -> bool:
    """`False` (never delete) for a missing or unparseable timestamp,
    rather than raising -- an item this code can't confidently date is one
    it must leave alone.
    """
    if not timestamp:
        return False
    try:
        parsed = datetime.fromisoformat(timestamp)
    except ValueError:
        return False
    return parsed < cutoff


def _is_label_protected(resource: dict, config: JanitorConfig) -> bool:
    labels = resource.get("labels")
    if not isinstance(labels, dict):
        return False
    return labels.get(config.protected_label_key) == config.protected_label_value


def _maybe_delete_instance(runner: Runner, config: JanitorConfig, instance: object,
                            cutoff: datetime, protected_names: frozenset[str],
                            result: JanitorResult) -> None:
    if not isinstance(instance, dict):
        return
    name = instance.get("name")
    if not name:
        return
    if name in protected_names or _is_label_protected(instance, config):
        return
    if not _older_than(instance.get("creationTimestamp"), cutoff):
        return
    zone = str(instance.get("zone") or "").rsplit("/", 1)[-1]
    if not zone:
        result.warnings.append(JanitorWarning("instance", name, "listing carried no zone"))
        return
    argv = [
        "gcloud", "compute", "instances", "delete", name,
        f"--zone={zone}", f"--project={config.project_id}", "--quiet",
        *_impersonated(config),
    ]
    try:
        runner.run(argv)
    except CommandError as exc:
        result.warnings.append(JanitorWarning("instance", name, str(exc)))
        return
    result.deleted.append(DeletedResource("instance", name))


def _maybe_delete_disk(runner: Runner, config: JanitorConfig, disk: object,
                        cutoff: datetime, protected_names: frozenset[str],
                        result: JanitorResult) -> None:
    if not isinstance(disk, dict):
        return
    name = disk.get("name")
    if not name:
        return
    if name in protected_names or _is_label_protected(disk, config):
        return
    # Attached disks are left alone: `gcloud compute disks delete` refuses
    # one anyway, and a disk still attached to a *live* (younger-than-TTL)
    # instance is exactly what must not be touched. A disk whose instance
    # this same pass just deleted still shows as attached here (both lists
    # were read at the start of this pass) and is picked up unattached on
    # the next cycle instead -- the same cron-not-webhooks convergence
    # every other pass in this codebase already relies on.
    if disk.get("users"):
        return
    if not _older_than(disk.get("creationTimestamp"), cutoff):
        return
    zone = str(disk.get("zone") or "").rsplit("/", 1)[-1]
    if not zone:
        result.warnings.append(JanitorWarning("disk", name, "listing carried no zone"))
        return
    argv = [
        "gcloud", "compute", "disks", "delete", name,
        f"--zone={zone}", f"--project={config.project_id}", "--quiet",
        *_impersonated(config),
    ]
    try:
        runner.run(argv)
    except CommandError as exc:
        result.warnings.append(JanitorWarning("disk", name, str(exc)))
        return
    result.deleted.append(DeletedResource("disk", name))


def _maybe_delete_gemini_key(runner: Runner, config: JanitorConfig, key: object,
                              cutoff: datetime, protected_key_names: frozenset[str],
                              result: JanitorResult) -> None:
    if not isinstance(key, dict):
        return
    name = key.get("name")
    if not name or name in protected_key_names:
        return
    display_name = key.get("displayName") or ""
    if not display_name.startswith(f"{config.name_prefix}-"):
        return
    if not _older_than(key.get("createTime"), cutoff):
        return
    argv = [
        "gcloud", "services", "api-keys", "delete", name,
        f"--project={config.project_id}", "--quiet",
        *_impersonated(config),
    ]
    try:
        runner.run(argv)
    except CommandError as exc:
        result.warnings.append(JanitorWarning("gemini-key", name, str(exc)))
        return
    result.deleted.append(DeletedResource("gemini-key", name))


def run_janitor(runner: Runner, config: JanitorConfig, now: datetime, *,
                 protected_gemini_key_names: frozenset[str] = frozenset()) -> JanitorResult:
    """One pass: list instances, disks, and Gemini API keys in
    `config.project_id`, and delete whichever of them are both older than
    `config.ttl_hours` and not protected. `protected_gemini_key_names`
    (`core.py`'s `_janitor` passes every `Assignment.gemini_key_name` still
    live in `AutomationState`) is a defensive extra check alongside the age
    cutoff -- task runtimes are always well under a sane TTL in practice,
    so this should never actually matter, but it costs nothing to check
    data this call already has in hand.
    """
    cutoff = now - timedelta(hours=config.ttl_hours)
    result = JanitorResult()
    _activate(runner, config)

    protected_names = frozenset({
        f"{config.name_prefix}-host", f"{config.name_prefix}-data",
    })

    instances, error = _list_resources(runner, [
        "gcloud", "compute", "instances", "list",
        f"--project={config.project_id}", "--format=json",
        *_impersonated(config),
    ])
    if error is not None:
        result.warnings.append(JanitorWarning("instance", "", f"could not list instances: {error}"))
    else:
        for instance in instances:
            _maybe_delete_instance(runner, config, instance, cutoff, protected_names, result)

    disks, error = _list_resources(runner, [
        "gcloud", "compute", "disks", "list",
        f"--project={config.project_id}", "--format=json",
        *_impersonated(config),
    ])
    if error is not None:
        result.warnings.append(JanitorWarning("disk", "", f"could not list disks: {error}"))
    else:
        for disk in disks:
            _maybe_delete_disk(runner, config, disk, cutoff, protected_names, result)

    keys, error = _list_resources(runner, [
        "gcloud", "services", "api-keys", "list",
        f"--project={config.project_id}", "--format=json",
        *_impersonated(config),
    ])
    if error is not None:
        result.warnings.append(JanitorWarning("gemini-key", "", f"could not list API keys: {error}"))
    else:
        for key in keys:
            _maybe_delete_gemini_key(
                runner, config, key, cutoff, protected_gemini_key_names, result
            )

    return result
