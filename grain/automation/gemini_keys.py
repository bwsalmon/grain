"""Mints and revokes short-lived Gemini API keys for a task that asks for
one with `config.gemini_key_label` on the task issue (`core.py`'s
`_resolve_target`, bwsalmon/agents#49) -- bwsalmon/agents#47.

**Why `gcloud`, not the metadata broker.** docs/design.md's "GCP
credentials" deliberately keeps a sandbox from ever holding a raw GCP
credential: `grain/metadata/` runs a per-sandbox `gce_metadata_server`
instance that impersonates a narrow service account, so a sandbox only ever
sees short-lived, revocable *tokens*. A literal Gemini API key doesn't fit
that shape -- it's a bearer string a caller mints once and holds, not
something ADC-style token-probing can hand out on demand, and the Generative
Language API is reached by that string alone, no metadata server involved.
So this stays off the sandbox-facing broker entirely: `apikeys.googleapis.com`
is called from the **controller**, as the controller's own account
(`grain-automation.service`, effectively root -- see `core.py`'s
`Orchestrator._dispatch`, which calls `create_key`/`delete_key` with
`base_runner`, never a per-sandbox `Runner`), and only the resulting key
*string* -- never a credential capable of minting more keys -- ever reaches
a sandbox, over the same stdin-not-argv channel `dispatch.py`'s
`configure_git_credentials` already uses for the git-proxy token.

**Why `gcloud`, not a hand-rolled REST call.** This project is stdlib-only
Python (`pyproject.toml`) specifically so a stock Debian sandbox needs no
package manager beyond `apt`; the controller is not that -- it already
carries `git`, `openssh-client`, and (docs/roadmap.md item 4) a whole
separate Go binary (`gce_metadata_server`), so a second controller-only
dependency costs nothing new in kind. Minting a key means an
OAuth2-authenticated call to a Google API, which needs a signed JWT
exchange; `gcloud` already does that correctly (and is what an operator
running the equivalent command by hand would reach for), so shelling out to
it is less code and less to get wrong than reimplementing that exchange
against Python's stdlib `ssl`/`http.client` with no crypto library
available. See the bwsalmon/agents#47 discussion thread for the fuller
tradeoff against re-using the metadata broker's impersonation path instead
(rejected: it would still end with a raw key string being handed to a
sandbox, so the broker's own "never hand out a raw credential" property
buys nothing extra here) and against `openssl`-plus-hand-rolled-JWT
(rejected: more code, for a controller that already tolerates one runtime
dependency in this kind).

**Authentication: the controller's one credential, acting as the agent.**
`configure_gcp_key_minter` (`configure.py`) places the only raw GCP
credential on this deployment, at `/data/secrets/gcp-key-minter.json` --
a key for the *host* service account (docs/design.md's "Secrets on
`/data`"). This module needs the *agent* account's permissions, not the
host's, so it authenticates with that key and then impersonates the agent
per call (`--impersonate-service-account`, bwsalmon/agents#131).

It used to hold the agent account's own long-lived key instead, at
`/data/secrets/gcp-service-account.json`, purely because the metadata
broker had already placed one. That broker is gone (bwsalmon/agents#126),
and the key with it: a long-lived agent key on the controller also
collided with `gcp_keys.py`'s periodic reap of agent keys older than 24
hours, which had no way to tell it apart from a per-dispatch key. `gcloud auth activate-service-account`
is called immediately before every `gcloud` invocation below, rather than
once at process start, so this stays correct regardless of whatever else
might otherwise be the CLI's active account on a shared controller -- cheap
and idempotent, not a real cost paid per call.

**Scoped to one API.** `--api-target=service=<api_target_service>`
restricts the minted key to the Generative Language API alone (never
`--api-target` omitted, which would mint a key valid against *every* API
the project has enabled) -- the same least-privilege instinct
docs/design.md applies to the metadata broker's own impersonated service
account ("a second, minimally-privileged service account").

**Two `gcloud` calls to mint, not one.** `api-keys create` does not print
the raw key string in its own output; the documented way to read it back is
a separate `api-keys get-key-string` call against the resource `create`
just returned (both take either the bare key id or, as used here, the full
`projects/.../locations/global/keys/<id>` resource name `create` returns,
so nothing here has to parse an id out of it by hand).

**`create` can hand back an operation, not the key, so the key is looked
up rather than read off `create`.** `api-keys create` starts a
long-running operation; on this deployment's `gcloud`,
`--format=value(name)` on it is observed to print the *operation's* id
(`operations/akmf.p7-<...>`) rather than the created key's own
`projects/.../locations/global/keys/<id>` name (bwsalmon/agents#100).
Feeding that operation id into `get-key-string` 404s every time -- it is
the wrong kind of resource id entirely, not a not-ready-yet race.

Two attempts to resolve it by polling the operation both failed against
real `gcloud`: `services api-keys operations describe` does not exist
(#104), and the `services operations describe` it was changed to returns
a JSON *array*, which crashed the poller with `'list' object has no
attribute 'get'` -- taking the whole dispatch pass down with it, since
`AttributeError` is not the `CommandError` `core.py` expects. Both were
written against a `FakeRunner` that returns whatever stdout the test
scripts, so neither the subcommand's existence nor its output shape was
ever checked.

So the operation is not consulted at all. `_find_key_by_display_name`
below lists the project's keys -- `api-keys list --format=json`, an array
of objects carrying `displayName`, `name` and `createTime` -- and picks
the newest entry whose `displayName` matches the one `create` was given
(`core.py` passes `grain-<sandbox>-issue-<n>`, deterministic per task).
Newest, not the only match, because a task that failed part-way could
have left an earlier key under the same display name behind. This uses
only `create`, `list` and `get-key-string`: three subcommands this module
already exercises against the real thing, and no new `gcloud` surface to
be wrong about a third time.
"""

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone
from dataclasses import dataclass
from pathlib import Path

from ..run import CommandError, Runner

# configure.py's GCP_KEY_MINTER_KEY_PATH, restated here rather than
# imported (the same "kept in sync by hand" precedent configure.py's own
# path constants already set). The host account's key: the controller's
# single GCP credential since bwsalmon/agents#131 -- this module acts as
# the *agent* account by impersonating it, not by holding its key.
_DEFAULT_KEY_PATH = Path("/data/secrets/gcp-key-minter.json")

DEFAULT_API_TARGET_SERVICE = "generativelanguage.googleapis.com"

# Mirrors gcp_keys.py's DEFAULT_MAX_KEY_AGE_HOURS, for the same reason and
# with the same caveat: neither a GCP API key nor a user-managed
# service-account key has a native TTL, so "expires" is something grain
# enforces. Keep AutomationConfig.max_runtime_minutes well under this, or
# the reap can revoke a key out from under a task still using it -- the
# tradeoff gcp_keys.py's own docstring already names and accepts.
DEFAULT_MAX_KEY_AGE_HOURS = 24

# Every key this module mints is named `grain-<sandbox>-issue-<n>` by
# core.py. Unlike a service-account key, an API key is not scoped to an
# account -- `api-keys list` returns every key in the *project* -- so this
# prefix is the only thing separating grain's own keys from anyone else's,
# and the reap below must never delete a key without it.
MINTED_DISPLAY_NAME_PREFIX = "grain-"


@dataclass(frozen=True)
class GeminiKeyConfig:
    """Operator-set tunables -- the same JSON-file-under-`/data/config`
    shape as `AutomationConfig`/`MetadataConfig`. Its presence (or absence)
    at `/data/config/gemini-key.json` is the on/off switch for the whole
    feature: `core.py`'s `_resolve_target` refuses a task carrying
    `gemini_key_label` with a parked-and-explained comment when this is
    `None`, the same "unusable request parks the task" shape it already
    applies to an unlisted `/repo`.
    """

    project_id: str
    # bwsalmon/agents#131: the *host* account's key -- the controller's one
    # credential. This module still acts as the agent account, but by
    # impersonating it per call (`impersonate_service_account` below)
    # rather than holding its key: the controller no longer has one.
    key_path: Path = _DEFAULT_KEY_PATH
    # The agent account to act as. Unset means "act as whoever the key
    # file is", which is how this behaved before impersonation.
    impersonate_service_account: str | None = None
    api_target_service: str = DEFAULT_API_TARGET_SERVICE
    max_key_age_hours: int = DEFAULT_MAX_KEY_AGE_HOURS

    @classmethod
    def load(cls, path: Path) -> "GeminiKeyConfig":
        raw = json.loads(path.read_text())
        if "key_path" in raw:
            raw["key_path"] = Path(raw["key_path"])
        return cls(**raw)


@dataclass(frozen=True)
class GeminiKey:
    # The full resource name (`projects/<num>/locations/global/keys/<id>`)
    # -- what `delete_key` needs later, recorded on the `Assignment` that
    # minted it (`state.py`) so a sweep can revoke it without re-deriving
    # anything from the key string itself.
    name: str
    key_string: str


def _activate(runner: Runner, config: GeminiKeyConfig) -> None:
    runner.run([
        "gcloud", "auth", "activate-service-account",
        f"--key-file={config.key_path}", "--quiet",
    ])


def _list_keys(runner: Runner, config: GeminiKeyConfig) -> list[dict]:
    """Every API key in the project, as gcloud's own JSON objects
    (`displayName`, `name`, `createTime`, ...).

    Raises `CommandError` -- never a bare `KeyError`/`AttributeError` --
    on an unexpected payload shape, so a surprise from `gcloud` reaches
    `core.py` as one task's (or one reap's) failure rather than an
    exception that ends the whole pass. Two earlier attempts at this code
    path each died that way, on a payload nobody had looked at; the
    message carries gcloud's own output for exactly that reason.
    """
    list_argv = [
        "gcloud", "services", "api-keys", "list",
        f"--project={config.project_id}", "--format=json",
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
    return [key for key in keys if isinstance(key, dict)]


def _find_key_by_display_name(
    runner: Runner, config: GeminiKeyConfig, display_name: str
) -> str:
    """The resource name of the newest key called `display_name`.

    Newest wins: a task whose earlier attempt died between `create` and
    this lookup can leave a key behind under the same name, and the one
    just minted is the one this call is about.
    """
    matches = [key for key in _list_keys(runner, config)
               if key.get("displayName") == display_name]
    if not matches:
        raise CommandError(
            ["gcloud", "services", "api-keys", "list"], 1,
            f"no API key named {display_name!r} in project {config.project_id} "
            "after creating one",
        )
    # createTime is RFC 3339, so lexicographic order is chronological. A
    # payload without it sorts first and simply loses to any entry that
    # has one, rather than raising.
    newest = max(matches, key=lambda key: key.get("createTime") or "")
    name = newest.get("name")
    if not name:
        raise CommandError(
            ["gcloud", "services", "api-keys", "list"], 1,
            f"API key {display_name!r} has no resource name: {newest!r}",
        )
    return name


def create_key(runner: Runner, config: GeminiKeyConfig, *, display_name: str) -> GeminiKey:
    """Mints a fresh API key scoped to `config.api_target_service`, and
    reads its value back. `runner` is always the controller's own account,
    never a sandbox's -- see this module's docstring.
    """
    _activate(runner, config)
    create_result = runner.run([
        "gcloud", "services", "api-keys", "create",
        f"--project={config.project_id}",
        f"--display-name={display_name}",
        f"--api-target=service={config.api_target_service}",
        "--format=value(name)", "--quiet",
    ])
    created = create_result.stdout.strip()

    # Past this point a key exists in the project whether or not the rest
    # of this function succeeds, and its name is knowable only here: the
    # caller records a name to revoke later (`state.py`'s Assignment) only
    # if this *returns*, so an exception on the way out used to strand a
    # live Generative Language API key with nothing left holding its name.
    # bwsalmon/agents#104 leaked one per retry, every cycle, for exactly
    # that reason. So clean up after ourselves before re-raising.
    name = created
    try:
        # `create` prints the operation's id rather than the key's on this
        # gcloud (see the module docstring); anything that isn't already a
        # key resource name gets resolved by lookup instead.
        if not name or name.startswith("operations/"):
            name = _find_key_by_display_name(runner, config, display_name)
        key_string_result = runner.run([
            "gcloud", "services", "api-keys", "get-key-string", name,
            f"--project={config.project_id}", "--format=value(keyString)",
            ])
    except CommandError:
        _revoke_orphan(runner, config, display_name, name if name != created else None)
        raise

    return GeminiKey(name=name, key_string=key_string_result.stdout.strip())


def _revoke_orphan(
    runner: Runner, config: GeminiKeyConfig, display_name: str, name: str | None
) -> None:
    """Best-effort revocation of a key `create_key` made but could not
    return. Never raises: the caller is already re-raising the failure
    that got us here, and a cleanup error must not replace it with a less
    informative one. If the name was never resolved, one lookup is worth
    attempting -- that is the case where a key is most likely stranded.
    """
    try:
        if name is None:
            name = _find_key_by_display_name(runner, config, display_name)
        delete_key(runner, config, name)
    except CommandError:
        pass


def delete_key(runner: Runner, config: GeminiKeyConfig, name: str) -> None:
    """Revokes a key `create_key` minted earlier, by its full resource
    name. Best-effort from the caller's side (`sweeper.py`'s `_release`
    catches `CommandError` around this) -- a sandbox's slot must still free
    even if Google's API is unreachable this cycle.
    """
    _activate(runner, config)
    runner.run([
        "gcloud", "services", "api-keys", "delete", name,
        f"--project={config.project_id}", "--quiet",
    ])


def delete_expired_keys(runner: Runner, config: GeminiKeyConfig, *,
                         now: datetime) -> list[str]:
    """Deletes every grain-minted API key older than
    `config.max_key_age_hours` -- the same safety net `gcp_keys.py`'s
    function of this name is for the per-dispatch service-account keys,
    and deliberately the same shape (bwsalmon/agents#131).

    Until now the only thing expiring a stranded Gemini key was the
    janitor, a *separate* opt-in feature (`enable_janitor`): a deployment
    with `enable_gemini_key` on and the janitor off never cleaned one up
    at all, while an agent key in the same situation always did. Expiry
    now belongs to the feature that mints the key, on for anyone who has
    minting on, exactly as it does for agent keys.

    Run once per `core.py` cycle, independent of `AutomationState`, so it
    still catches a key whose assignment was lost -- a controller crash
    between mint and `state.assign`, most notably. `sweeper.py`'s revoke
    when a slot frees remains the primary mechanism; this only catches
    what that missed.

    **Only keys this module minted.** An API key is not scoped to a
    service account the way `gcp_keys.py`'s keys are -- `api-keys list`
    returns every key in the *project* -- so the display-name prefix is
    the only thing separating grain's from anyone else's, and a key
    without it is left alone no matter how old. That is a real difference
    from the agent-key reap, which gets its scoping from
    `--iam-account` for free.

    Best-effort per key, and a key with no `createTime` is left alone
    rather than guessed at -- the same "surface it, don't gate on it" and
    "absent data loses, doesn't crash" stances the agent-key reap takes.
    """
    cutoff = now - timedelta(hours=config.max_key_age_hours)
    deleted: list[str] = []
    for key in _list_keys(runner, config):
        name = key.get("name")
        created_at = key.get("createTime")
        display_name = key.get("displayName") or ""
        if not name or not created_at:
            continue
        if not display_name.startswith(MINTED_DISPLAY_NAME_PREFIX):
            continue
        created = datetime.fromisoformat(created_at.replace("Z", "+00:00"))
        if created.tzinfo is None:
            created = created.replace(tzinfo=timezone.utc)
        if created >= cutoff:
            continue
        try:
            delete_key(runner, config, name)
        except CommandError:
            continue
        deleted.append(name)
    return deleted
