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

**Authentication: a minter key file, activated before every call.**
Minting a *new* agent-account key has to be done by some identity other
than the agent account itself -- a sandbox holding a leaked agent key must
not be able to mint itself a fresh one and defeat the whole "expires"
premise this module exists for. bwsalmon/agents#126 asks for exactly that
separation: "the automation / host service account should have permission
to mint new keys for the agent service account."

This module originally tried to get that identity for free, reasoning that
the controller "already runs *as* the host service account via the real GCE
metadata server" and so needed no credential of its own. That is true of
the **host** and false of the **controller**, which is where this code
actually runs (bwsalmon/agents#131): `terraform/gcp/` declares exactly one
`google_compute_instance`, the host. The controller is a nested libvirt
guest on grain's private subnet (`inventory.py`, offset `.2`) -- not a GCE
VM, with no attached service account, and no route to `169.254.169.254`
(the metadata anycast DNAT is installed per *sandbox*, and pointed at the
fake broker this change removed). So `gcloud` on the controller had no
credentials at all, and every mint and every reap failed with "You do not
currently have an active account selected" -- silently, since `core.py`'s
`_dispatch` treats a `CommandError` as one task's failure and moves on.

So the minter identity travels as a key file, exactly like
`gemini_keys.py`'s: `GcpKeyConfig.key_path` (default
`/data/secrets/gcp-key-minter.json`, placed by
`configure.py`'s `configure_gcp_key_minter`) holds a key for the *host*
service account, which `terraform/gcp/iam.tf` already grants
`roles/iam.serviceAccountKeyAdmin` on the agent account. Deliberately a
second, separate file from the agent key at
`/data/secrets/gcp-service-account.json` that `gemini_keys.py` and
`janitor.py` still authenticate with: that one *is* the agent account, so
using it here would be the agent minting for itself -- the exact thing the
separation above exists to prevent (and it would fail anyway, since the
agent account holds no `serviceAccountKeyAdmin` grant).

`_activate` runs immediately before *every* `gcloud` invocation below
rather than once at import, for the same reason `gemini_keys.py` does it:
`gcloud auth activate-service-account` sets the active account globally for
the invoking user, so two modules on one controller authenticating as two
different accounts would otherwise race, and whichever ran last would
silently win. Cheap and idempotent, not a real per-call cost.

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

# configure.py's GCP_KEY_MINTER_KEY_PATH, restated here rather than
# imported -- see this module's docstring, and the identical precedent
# gemini_keys.py's own _DEFAULT_KEY_PATH sets against
# configure.py's GCP_KEY_MINTER_KEY_PATH.
_DEFAULT_MINTER_KEY_PATH = Path("/data/secrets/gcp-key-minter.json")


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
    # The *minter's* credential, never the agent account's own -- see this
    # module's docstring. Kept in sync with configure.py's
    # GCP_KEY_MINTER_KEY_PATH by hand, the same precedent
    # gemini_keys.py's own _DEFAULT_KEY_PATH already set.
    key_path: Path = _DEFAULT_MINTER_KEY_PATH

    @classmethod
    def load(cls, path: Path) -> "GcpKeyConfig":
        raw = json.loads(path.read_text())
        if "key_path" in raw:
            raw["key_path"] = Path(raw["key_path"])
        return cls(**raw)


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


def _activate(runner: Runner, config: GcpKeyConfig) -> None:
    """Authenticate `gcloud` as the minter. Called before every command
    below -- see this module's docstring on why not once at startup."""
    runner.run([
        "gcloud", "auth", "activate-service-account",
        f"--key-file={config.key_path}", "--quiet",
    ])


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
    material to a file, not stdout. This module used to take the key's id
    from that command's own `--format=value(name.basename())` output --
    and on the controller's gcloud that prints *nothing*, so every mint
    died on "gcloud printed no key id" (bwsalmon/agents#140). The same
    mistake `gemini_keys.py` made twice: an assumption about what a gcloud
    command prints, never checked against the real thing. `gcloud`
    versions differ here, and CI's is not the controller's.

    So the id comes from the key file instead. A Google credentials file
    carries `private_key_id`, which *is* the id `keys delete` takes -- it
    is written by the same call that created the key, needs no second
    gcloud invocation, and cannot disagree with the key actually on disk.
    `create`'s stdout is kept only as a fallback for the case where the
    file is unreadable, which is the one situation the file cannot answer.
    """
    tmp_path = f"/tmp/grain-agent-gcp-key-{secrets_module.token_hex(8)}.json"
    _activate(runner, config)
    create_argv = _keys_create_argv(tmp_path, config)
    create_result = runner.run(create_argv)
    printed_id = create_result.stdout.strip()

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
        key_id = _key_id_from_key_file(key_json) or printed_id
        if not key_id:
            raise CommandError(
                create_argv, 1,
                "the created key file carries no private_key_id and gcloud "
                f"printed no id either: {key_json[:200]!r}",
            )
    except CommandError:
        # `printed_id` may be empty, in which case there is no id to revoke
        # by and the key is left for the periodic reap to catch -- see
        # `delete_expired_keys`, which exists for exactly this.
        _revoke_orphan(runner, config, printed_id)
        raise
    finally:
        # Best-effort: the temp file lives only on the controller's own
        # disk, briefly, and its absence changes nothing about whether the
        # *key* itself needs revoking above.
        runner.run(["rm", "-f", tmp_path], check=False)

    return GcpKey(key_id=key_id, key_json=key_json)


def _key_id_from_key_file(key_json: str) -> str | None:
    """The key's own id, out of the credentials file gcloud just wrote.

    `private_key_id` is the same bare id `keys delete` takes. `None` for
    anything unparseable rather than raising, so the caller can fall back
    to whatever `create` printed before giving up.
    """
    try:
        parsed = json.loads(key_json)
    except json.JSONDecodeError:
        return None
    if not isinstance(parsed, dict):
        return None
    key_id = parsed.get("private_key_id")
    return key_id if isinstance(key_id, str) and key_id else None


def _revoke_orphan(runner: Runner, config: GcpKeyConfig, key_id: str) -> None:
    """Best-effort revocation of a key `create_key` minted but could not
    return. Never raises -- the caller is already re-raising the failure
    that got us here, and a cleanup error must not replace it with a less
    informative one (the same discipline `gemini_keys.py`'s own
    `_revoke_orphan` holds to).

    An empty `key_id` is a no-op: `create_key` reaches this with one only
    when the key file was unreadable *and* gcloud printed nothing, so
    there is no id to revoke by. `delete_expired_keys` is the net for that
    case -- calling `keys delete ""` would just be a confusing second
    error on top of the real one.
    """
    if not key_id:
        return
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
    _activate(runner, config)
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
    _activate(runner, config)
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
