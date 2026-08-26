"""Mints and revokes short-lived service-account keys for the narrow "agent"
service account (`terraform/gcp/iam.tf`'s `google_service_account.agent`),
one per sandbox dispatch -- bwsalmon/agents#126, replacing the per-sandbox
`gce_metadata_server` broker `grain/metadata/` used to run (removed by this
same change).

**Why a real key now, not an impersonated token.** The metadata broker's
whole point was that a sandbox never held anything more than a short-lived,
non-exportable *token* -- see docs/design.md's "GCP credentials". A key file
is the opposite of that: a static, bearer credential that is valid on its
own wherever it is copied. bwsalmon/agents#126 asks for exactly this
tradeoff anyway (mirroring `gemini_keys.py`'s own "mint one per task, push
the raw secret into the sandbox, revoke at end of session" shape) --
presumably because the broker's own operational cost (a `gce_metadata_server`
binary, a DNAT rule per sandbox, a fake instance-metadata document) wasn't
worth it against a design that instead leans on the same three mitigations
`gemini_keys.py` already relies on: mint narrow, mint short-lived, and
revoke promptly. See `sweeper.py`'s `_revoke_gcp_key` for the "promptly"
half, and `max_key_age_hours` below for the "short-lived" half.

**Authentication: the controller's own attached identity, never a static
file.** Unlike `gemini_keys.py` (which activates the same primary key the
old metadata broker held, at `/data/secrets/gcp-service-account.json`),
nothing here calls `gcloud auth activate-service-account` at all. Minting a
*new* agent-account key has to be done by some identity other than the
agent account itself -- a sandbox holding a leaked agent key must not be
able to mint itself a fresh one and defeat the whole "expires" premise this
module exists for. bwsalmon/agents#126 asks for exactly that separation:
"the automation / host service account should have permission to mint new
keys for the agent service account." The controller VM already runs
*as* that host service account (`terraform/gcp/instance.tf`'s
`google_compute_instance.host.service_account`, `scopes = ["cloud-platform"]`)
via the real GCE metadata server -- a completely different thing from the
fake per-sandbox one this change removes -- so `gcloud` on the controller
is already authenticated as the host account with no file to place or
rotate, as long as `terraform/gcp/iam.tf` grants that account
`roles/iam.serviceAccountKeyAdmin` on the agent account (this change adds
that grant). This is also *safer* than a static file: the host account
already holds no credential at rest (docs/design.md, "The host holds no
system credentials"), and this stays true.

**"24-hour expiry" is enforced by grain, not GCP.** User-managed IAM
service-account keys have no native TTL -- `iam.serviceAccountKeys.create`
mints a key that is valid until explicitly deleted, unlike (for instance) an
OAuth access token or a signed JWT. So bwsalmon/agents#126's "these keys
should have an expiry of 24 hours" cannot be a property of the key itself;
it is enforced by `delete_expired_keys` below, a periodic reap (wired into
`core.py`'s `_sweep`, run every cycle) that deletes any key under the agent
account older than `GcpKeyConfig.max_key_age_hours`, independent of whether
its task ever finished cleanly. This is a *safety net*, not the primary
mechanism: the primary mechanism is `sweeper.py`'s `_revoke_gcp_key`,
called the moment a sandbox's slot frees (success, failure, or stranded) --
see that module's docstring. The reap only ever catches a key that
mechanism missed (a controller crash between mint and the assignment
being recorded, an operator-invalidated `AutomationState`, ...); a
deployment whose `max_runtime_minutes` (`AutomationConfig`) is kept well
under 24 hours will essentially never see the reap fire under normal
operation.

**One key per dispatch, not one per task label.** Unlike a Gemini key
(`gemini_keys.py`, gated on the `grain-gemini-key` label so most tasks never
mint one), the old metadata broker ran unconditionally for *every*
dispatched sandbox whenever a deployment had one configured at all --
"Point a sandbox at its instance and GCP access just works," no directive
or label required. This module preserves that parity: `core.py`'s
`_dispatch` mints a key whenever `Orchestrator.gcp_key_config` is not
`None`, the same "deployment-wide on/off switch, never a per-task one"
shape `MetadataLauncher`/`_ensure_metadata_server` used to have.
"""

from __future__ import annotations

import json
import secrets as secrets_module
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

from ..run import CommandError, Runner

DEFAULT_MAX_KEY_AGE_HOURS = 24


@dataclass(frozen=True)
class GcpKeyConfig:
    """Operator-set tunables -- the same JSON-file-under-`/data/config`
    shape as `GeminiKeyConfig`/`MetadataConfig`. Its presence (or absence)
    at `/data/config/gcp-key.json` is the on/off switch for the whole
    feature: `cli.py`'s `build_orchestrator` leaves
    `Orchestrator.gcp_key_config` `None` when the file is absent, which
    makes every call site below a no-op, the same "feature not configured"
    shape `gemini_key_config`/the old `metadata_launcher` already had.
    """

    service_account_email: str
    project_id: str
    max_key_age_hours: int = DEFAULT_MAX_KEY_AGE_HOURS

    @classmethod
    def load(cls, path: Path) -> "GcpKeyConfig":
        return cls(**json.loads(path.read_text()))


@dataclass(frozen=True)
class GcpKey:
    # The bare key id -- e.g. the last path segment of
    # `projects/<p>/serviceAccounts/<email>/keys/<id>` -- what `delete_key`
    # needs later, recorded on the `Assignment` that minted it (`state.py`)
    # so a sweep can revoke it without re-deriving anything from the key
    # material itself.
    key_id: str
    # The raw JSON key file content (a Google credentials-file document:
    # type, project_id, private_key, client_email, ...) -- what actually
    # gets pushed into the sandbox (`dispatch.py`'s `configure_gcp_key`).
    key_json: str


def _keys_create_argv(tmp_path: str, config: GcpKeyConfig) -> list[str]:
    return [
        "gcloud", "iam", "service-accounts", "keys", "create", tmp_path,
        f"--iam-account={config.service_account_email}",
        "--format=value(name.basename())",
    ]


def create_key(runner: Runner, config: GcpKeyConfig) -> GcpKey:
    """Mints a fresh key for `config.service_account_email` and reads its
    value back. `runner` is always the controller's own account (the host
    service account, via its native GCE identity -- see this module's own
    docstring) -- never a sandbox's, and never the agent account itself.

    `gcloud iam service-accounts keys create` writes the key's private
    material to a file, not stdout -- unlike `gemini_keys.py`'s
    `api-keys create`, there is no separate "look the value up afterward"
    step needed: `--format=value(name.basename())` on this (synchronous,
    non-long-running) call reliably prints the created key's own bare id,
    the same shape `ci/push-host-secrets.sh`'s existing (and, on this
    project, actually-run) use of the identical command already relies on.
    """
    tmp_path = f"/tmp/grain-agent-gcp-key-{secrets_module.token_hex(8)}.json"
    create_argv = _keys_create_argv(tmp_path, config)
    create_result = runner.run(create_argv)
    key_id = create_result.stdout.strip()
    if not key_id:
        raise CommandError(
            create_argv, 1, f"gcloud printed no key id: {create_result.stdout!r}"
        )

    # A key now exists in the project whether or not the rest of this
    # function succeeds -- read the file back (and remove it from the
    # controller's own /tmp) inside a try/except, the same
    # bwsalmon/agents#104 lesson `gemini_keys.py`'s own `create_key`
    # learned: a caller only ever gets a name to revoke later if this
    # function *returns*, so a failure on the way out must clean up here,
    # not leave an orphan for nothing to ever revoke.
    try:
        key_json = runner.run(["cat", tmp_path]).stdout
        if not key_json.strip():
            raise CommandError(["cat", tmp_path], 1, "created key file was empty")
    except CommandError:
        _revoke_orphan(runner, config, key_id)
        raise
    finally:
        # Best-effort: the temp file lives only on the controller's own
        # disk, briefly, and its absence changes nothing about whether the
        # *key* itself needs revoking above.
        runner.run(["rm", "-f", tmp_path], check=False)

    return GcpKey(key_id=key_id, key_json=key_json)


def _revoke_orphan(runner: Runner, config: GcpKeyConfig, key_id: str) -> None:
    """Best-effort revocation of a key `create_key` minted but could not
    return. Never raises -- the caller is already re-raising the failure
    that got us here, and a cleanup error must not replace it with a less
    informative one (the same discipline `gemini_keys.py`'s own
    `_revoke_orphan` holds to).
    """
    try:
        delete_key(runner, config, key_id)
    except CommandError:
        pass


def delete_key(runner: Runner, config: GcpKeyConfig, key_id: str) -> None:
    """Revokes a key `create_key` minted earlier, by its bare id.
    Best-effort from the caller's side (`sweeper.py`'s `_release` catches
    `CommandError` around this, same as it already does for
    `gemini_keys.delete_key`) -- a sandbox's slot must still free even if
    Google's API is unreachable this cycle.
    """
    runner.run([
        "gcloud", "iam", "service-accounts", "keys", "delete", key_id,
        f"--iam-account={config.service_account_email}", "--quiet",
    ])


def _list_keys(runner: Runner, config: GcpKeyConfig) -> list[dict]:
    """Every user-managed key currently live under
    `config.service_account_email`, as gcloud's own JSON objects (`name`,
    `validAfterTime`, ...). `--managed-by=user` excludes the
    Google-managed keys every service account also has and which this
    project never touches (they cannot be listed, downloaded, or deleted
    by this API at all).

    Raises `CommandError` -- never a bare exception from malformed JSON --
    on an unexpected payload shape, the same "whatever gcloud hands back,
    this stays one task's failure" bar `gemini_keys.py`'s own
    `_find_key_by_display_name` holds to.
    """
    list_argv = [
        "gcloud", "iam", "service-accounts", "keys", "list",
        f"--iam-account={config.service_account_email}",
        "--managed-by=user", "--format=json",
    ]
    result = runner.run(list_argv)
    try:
        keys = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise CommandError(list_argv, 1, f"could not parse as JSON ({exc}): {result.stdout!r}")
    if not isinstance(keys, list):
        raise CommandError(
            list_argv, 1,
            f"expected a JSON array of keys, got {type(keys).__name__}: {result.stdout!r}",
        )
    return keys


def delete_expired_keys(runner: Runner, config: GcpKeyConfig, *,
                         now: datetime) -> list[str]:
    """Deletes every key under `config.service_account_email` older than
    `config.max_key_age_hours` -- the actual enforcement of "24-hour
    expiry" this module's own docstring explains GCP has no native concept
    of. A safety net, run once per `core.py` cycle (`_sweep`), independent
    of `AutomationState` entirely: it acts on whatever the project itself
    reports, so it still catches an orphaned key even if the assignment
    that minted it was lost.

    Best-effort per key -- one already-deleted or otherwise-unreachable key
    must not stop the rest from being reaped this cycle, the same
    "surface it, don't gate on it" treatment `sweeper.py`'s own credential
    warnings already get. Returns the bare ids of the keys this call
    actually deleted, for `core.py` to log.

    A key with no `validAfterTime` in gcloud's own listing (never observed
    in practice, but the payload is not this project's to fully trust) is
    left alone rather than guessed at -- the same "absent data loses,
    doesn't crash" stance `gemini_keys.py`'s own newest-wins lookup takes
    with a missing `createTime`.
    """
    cutoff = now - timedelta(hours=config.max_key_age_hours)
    deleted: list[str] = []
    for key in _list_keys(runner, config):
        valid_after = key.get("validAfterTime")
        name = key.get("name")
        if not valid_after or not name:
            continue
        created = datetime.fromisoformat(valid_after.replace("Z", "+00:00"))
        if created.tzinfo is None:
            created = created.replace(tzinfo=timezone.utc)
        if created >= cutoff:
            continue
        key_id = name.rsplit("/", 1)[-1]
        try:
            delete_key(runner, config, key_id)
        except CommandError:
            continue
        deleted.append(key_id)
    return deleted
