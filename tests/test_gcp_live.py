"""Live verification of the `gcloud` contracts grain's GCP code depends on.

Every production failure in `gcp_keys.py`/`gemini_keys.py` so far has been
the same mistake: an assumption about what a `gcloud` command prints or
returns, asserted in a docstring, and never run. `FakeRunner` cannot catch
that class by construction -- it replays whatever stdout the test author
imagined, so a test passes precisely as confidently when the command does
not exist (`services api-keys operations describe`, bwsalmon/agents#104),
returns a different shape (`services operations describe` handing back a
JSON array), or prints nothing at all (`iam service-accounts keys create
--format=value(name.basename())`, bwsalmon/agents#140).

So this suite runs the *real* production functions against a *real*
project, with a real `RealRunner`, and asserts the specific properties the
code relies on. It is not an end-to-end test of minting-for-a-task; it is a
contract test for the half of the system that lives outside this repo.

**It is skipped unless you deliberately point it at a project.** It creates
and deletes real service-account keys, so it needs to be opted into:

    GRAIN_LIVE_GCP_KEY_FILE=/path/to/minter-key.json \\
    GRAIN_LIVE_GCP_PROJECT=my-project \\
    GRAIN_LIVE_GCP_AGENT_SA=grain-agent@my-project.iam.gserviceaccount.com \\
    python -m pytest tests/test_gcp_live.py

The key file must be the *minter's* (the host account), matching what the
controller actually holds -- that is the credential whose permissions are
being verified, not just any credential that happens to work.

**What it deliberately does not do.** `delete_expired_keys` deletes every
user-managed key on the agent account past its TTL, so this never calls it
with a short one: against a live project that would revoke the keys of
tasks currently running. It is exercised at the real 24h default, where the
correct answer is "deletes nothing", which still covers the listing, the
timestamp parse and the age filter -- everything except the delete call
itself, which `test_delete_by_private_key_id_removes_it` covers directly on
a key this suite made.

Gemini API key checks additionally need GRAIN_LIVE_GEMINI=1, since they
mint a real (if immediately revoked) API key.
"""

import json
import os

import pytest

from grain.automation import gcp_keys, gemini_keys
from grain.run import RealRunner

KEY_FILE = os.environ.get("GRAIN_LIVE_GCP_KEY_FILE")
PROJECT = os.environ.get("GRAIN_LIVE_GCP_PROJECT")
AGENT_SA = os.environ.get("GRAIN_LIVE_GCP_AGENT_SA")

pytestmark = pytest.mark.skipif(
    not (KEY_FILE and PROJECT and AGENT_SA),
    reason="needs GRAIN_LIVE_GCP_KEY_FILE, GRAIN_LIVE_GCP_PROJECT and "
           "GRAIN_LIVE_GCP_AGENT_SA pointed at a real project",
)


@pytest.fixture(scope="module")
def runner() -> RealRunner:
    return RealRunner()


@pytest.fixture(scope="module")
def gcp_config() -> "gcp_keys.GcpKeyConfig":
    from pathlib import Path
    return gcp_keys.GcpKeyConfig(
        service_account_email=AGENT_SA, project_id=PROJECT, key_path=Path(KEY_FILE),
    )


@pytest.fixture
def minted_key(runner, gcp_config):
    """One real key, revoked however the test ends."""
    key = gcp_keys.create_key(runner, gcp_config)
    try:
        yield key
    finally:
        try:
            gcp_keys.delete_key(runner, gcp_config, key.key_id)
        except Exception:  # noqa: BLE001 -- cleanup must not mask a failure
            pass


# --- service-account keys -------------------------------------------------

def test_create_key_yields_an_id_the_project_agrees_with(minted_key, runner, gcp_config):
    """bwsalmon/agents#140 in one assertion. `create_key` no longer trusts
    what `keys create` prints -- it reads `private_key_id` out of the file
    gcloud wrote. This checks that id is real: gcloud's own listing has it.
    """
    assert minted_key.key_id, "no key id came back at all"
    assert json.loads(minted_key.key_json)["private_key_id"] == minted_key.key_id

    listed = {k["name"].rsplit("/", 1)[-1] for k in gcp_keys._list_keys(runner, gcp_config)}
    assert minted_key.key_id in listed, (
        "the id create_key returned is not one the project reports"
    )


def test_the_key_file_is_a_usable_credential(minted_key):
    """What actually gets pushed into a sandbox -- a credentials document,
    not a fragment. If this shape changed, a sandbox's ADC would break with
    nothing on the controller noticing."""
    parsed = json.loads(minted_key.key_json)
    assert parsed["type"] == "service_account"
    assert parsed["client_email"] == AGENT_SA
    assert parsed["project_id"] == PROJECT
    assert parsed["private_key"].startswith("-----BEGIN PRIVATE KEY-----")


def test_the_listing_carries_the_fields_the_reap_reads(minted_key, runner, gcp_config):
    """`delete_expired_keys` reads `name` and `validAfterTime` off every
    entry. A listing that stopped carrying either would make the reap
    silently skip every key -- it treats absent data as "leave alone"."""
    keys = gcp_keys._list_keys(runner, gcp_config)
    assert keys, "no user-managed keys listed at all"
    for key in keys:
        assert isinstance(key, dict)
        assert key.get("name"), key
        assert key.get("validAfterTime"), key


def test_the_reap_leaves_a_brand_new_key_alone(minted_key, runner, gcp_config):
    """The whole listing/parse/filter path at the real 24h default, where
    a key minted seconds ago must survive. Deliberately not run with a
    short TTL -- see this module's docstring."""
    from datetime import datetime, timezone
    deleted = gcp_keys.delete_expired_keys(
        runner, gcp_config, now=datetime.now(timezone.utc),
    )
    assert minted_key.key_id not in deleted


def test_delete_by_private_key_id_removes_it(runner, gcp_config):
    """Proves `private_key_id` is the handle `keys delete` takes -- the
    assumption create_key now rests on."""
    key = gcp_keys.create_key(runner, gcp_config)
    gcp_keys.delete_key(runner, gcp_config, key.key_id)
    listed = {k["name"].rsplit("/", 1)[-1] for k in gcp_keys._list_keys(runner, gcp_config)}
    assert key.key_id not in listed


# --- impersonation (bwsalmon/agents#131) ----------------------------------

def test_the_minter_key_can_impersonate_the_agent(runner, gcp_config):
    """janitor.py and gemini_keys.py authenticate as the host and then act
    as the agent. That rests on roles/iam.serviceAccountTokenCreator being
    granted -- verified here rather than assumed, since the last premise
    about how the controller gets its identity was wrong.
    """
    gcp_keys._activate(runner, gcp_config)
    result = runner.run([
        "gcloud", "auth", "print-access-token",
        f"--impersonate-service-account={AGENT_SA}",
    ])
    assert result.stdout.strip(), "impersonation produced no token"


# --- Gemini API keys ------------------------------------------------------

gemini = pytest.mark.skipif(
    os.environ.get("GRAIN_LIVE_GEMINI") != "1",
    reason="mints a real Gemini API key; set GRAIN_LIVE_GEMINI=1 to opt in",
)


@gemini
def test_gemini_create_and_look_up_by_display_name(runner):
    """The contract bwsalmon/agents#100/#104 got wrong twice: what
    `api-keys create` prints, and how to turn that into a key you can read
    a string from. `create_key` looks the key up by display name instead of
    trusting either -- this checks that lookup finds the real thing.
    """
    from pathlib import Path
    config = gemini_keys.GeminiKeyConfig(
        project_id=PROJECT, key_path=Path(KEY_FILE),
    )
    display_name = "grain-livetest-contract-check"
    key = gemini_keys.create_key(runner, config, display_name=display_name)
    try:
        assert key.name.startswith("projects/")
        assert key.key_string, "no key string came back"

        listed = gemini_keys._list_keys(runner, config)
        assert any(k.get("displayName") == display_name for k in listed)
        for entry in listed:
            assert entry.get("name"), entry
            assert entry.get("createTime"), entry
    finally:
        gemini_keys.delete_key(runner, config, key.name)
