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

**Authentication: the same primary key the metadata broker already
holds.** `configure_gcp_service_account` (`configure.py`) is the one place
a raw GCP credential is ever placed on this deployment, at
`/data/secrets/gcp-service-account.json` -- see that function's own
docstring, and docs/design.md's "Secrets on `/data`". Reusing it here
(`GeminiKeyConfig.key_path` defaults to the identical path,
kept in sync by hand the same way `configure.py`'s own
`GCP_SERVICE_ACCOUNT_KEY_PATH`/`METADATA_SERVER_CONFIG_PATH` pair already
is, since the two modules are never imported into each other) avoids a
second GCP credential-provisioning step for a deployment that already did
this one for `grain metadata start`. `gcloud auth activate-service-account`
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

**`create` can hand back an operation, not the key, and that needs a third
call.** `api-keys create` starts a long-running operation; on this
deployment's `gcloud`, `--format=value(name)` on it is observed to print
the *operation's* id (`operations/akmf.p7-<...>`) rather than waiting and
printing the created key's own `projects/.../locations/global/keys/<id>`
name (bwsalmon/agents#100). Feeding that operation id straight into
`get-key-string` -- as this module did before #100 -- 404s every single
time, no matter how long the caller waits first: it isn't a not-ready-yet
race, it's the wrong kind of resource id entirely, one `get-key-string`
can never resolve. `_await_operation` below polls
`services operations describe` (bwsalmon/agents#104: not `services
api-keys operations describe` -- that subcommand does not exist on this
`gcloud`, api-keys' long-running operations are described through the
plain `services operations` group, one level up, the same place `gcloud
services enable`'s operations live) until Google reports the operation
`done`, then reads the created key's resource name back out of its
`response` payload, and only *then* calls `get-key-string`. A `name` that
already looks like a key (doesn't start with `operations/`) skips polling
entirely, so this stays a no-op against a `gcloud` whose `create` already
waits and returns the key directly.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass
from pathlib import Path

from ..run import CommandError, Runner

# How long to poll `api-keys operations describe` for before giving up --
# an ordinary key-creation operation finishes in a couple of seconds, so
# this is generous slack, not a tuned budget. Giving up raises the same
# `CommandError` any other failure here would, so `core.py`'s existing
# "log and move on, retry next cycle" handling around `create_gemini_key`
# applies unchanged rather than hanging that cycle's whole dispatch pass.
_OPERATION_POLL_INTERVAL_SECONDS = 2
_OPERATION_POLL_TIMEOUT_SECONDS = 30

# configure.py's GCP_SERVICE_ACCOUNT_KEY_PATH, restated here rather than
# imported -- see this module's own docstring on why the two are kept in
# sync by hand, the same precedent configure.py's own
# METADATA_SERVER_CONFIG_PATH/GCP_SERVICE_ACCOUNT_KEY_PATH pair set.
_DEFAULT_KEY_PATH = Path("/data/secrets/gcp-service-account.json")

DEFAULT_API_TARGET_SERVICE = "generativelanguage.googleapis.com"


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
    key_path: Path = _DEFAULT_KEY_PATH
    api_target_service: str = DEFAULT_API_TARGET_SERVICE

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


def _await_operation(runner: Runner, config: GeminiKeyConfig, operation_name: str) -> str:
    """Polls a `services api-keys create` long-running operation until
    Google reports it `done`, then returns the created key's own resource
    name from the operation's `response` -- see this module's docstring on
    why `get-key-string` cannot be called with `operation_name` itself.
    """
    deadline = time.monotonic() + _OPERATION_POLL_TIMEOUT_SECONDS
    while True:
        describe_argv = [
            "gcloud", "services", "operations", "describe",
            operation_name, f"--project={config.project_id}", "--format=json",
        ]
        result = runner.run(describe_argv)
        operation = json.loads(result.stdout)
        if operation.get("done"):
            if "error" in operation:
                raise CommandError(describe_argv, 1, json.dumps(operation["error"]))
            return operation["response"]["name"]
        if time.monotonic() >= deadline:
            raise CommandError(
                describe_argv, 1,
                f"operation {operation_name} did not finish within "
                f"{_OPERATION_POLL_TIMEOUT_SECONDS}s",
            )
        time.sleep(_OPERATION_POLL_INTERVAL_SECONDS)


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
    name = create_result.stdout.strip()
    if name.startswith("operations/"):
        name = _await_operation(runner, config, name)
    key_string_result = runner.run([
        "gcloud", "services", "api-keys", "get-key-string", name,
        f"--project={config.project_id}", "--format=value(keyString)",
    ])
    return GeminiKey(name=name, key_string=key_string_result.stdout.strip())


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
