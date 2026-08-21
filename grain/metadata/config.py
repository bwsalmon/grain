"""Per-sandbox `gce_metadata_server` configuration: what to write to disk
and what argv to launch it with.

docs/design.md's "GCP credentials": one `gce_metadata_server` instance per
sandbox, each bound to `Cluster.controller_ip:Cluster.metadata_port(name)`
(never the sandbox's own address -- see `net_linux.py`'s DNAT rule, these
processes live on the controller), impersonating a single narrow service
account rather than serving the primary key's own tokens.

Pure functions only; nothing here touches a filesystem beyond the paths
it's handed, so `grain/metadata/launcher.py` is the only place that
actually writes a config file or starts a process -- the same split
`grain/proxy/server.py` keeps between wiring and the real I/O.

Verified against a real `gce_metadata_server` v4.2.5 binary (installed and
run locally; see docs/roadmap.md item 4), not assumed from the README
alone:

- `-interface`/`-port` bind exactly the address given, no `0.0.0.0`
  fallback -- confirmed live: a request to a *different* local address
  than the one passed to `-interface` fails to connect, while the bound
  one serves fine. This is what lets every instance sit on the same
  controller host without colliding.
- `--impersonate` reads the *source* credential from the
  `GOOGLE_APPLICATION_CREDENTIALS` environment variable, not
  `-serviceAccountFile` -- confirmed live via the server's own startup
  log line, `"credential_configuration"` with
  `"type":"impersonated_credential"` and
  `"target_principal":"<serviceAccounts.default.email from configFile>"`.
  Passing the primary (broad) key via `-serviceAccountFile` instead would
  serve *that* key's own tokens directly -- the shape docs/design.md
  explicitly rejects ("rather than serve the primary key's own tokens").
  `GOOGLE_APPLICATION_CREDENTIALS` therefore has to be set in the launched
  process's environment, which is why `launcher.py` starts each instance
  via `systemd-run --setenv=...` rather than passing it on argv.
- `-logTarget` is a plain file path (`os.OpenFile(path,
  O_APPEND|O_CREATE|O_WRONLY, 0644)` in the tool's own `cmd/main.go`), so
  pointing it at a per-sandbox file under `/data/state/metadata-server/`
  is what `grain/metadata/audit.py` tails to build the audit log
  docs/design.md calls for ("every mint is audit-logged").
- With `-jsonLog -debug`, each request logs a `msg="request"` line with
  its `path`; a failed token mint logs a following `level="ERROR"` line.
  There is no webhook or callback hook in this tool -- confirmed by
  reading its README and `cmd/main.go` end to end -- so per-mint audit
  logging has to come from tailing this log, not from a callback the
  binary invokes for us.
"""

from __future__ import annotations

import json
import zlib
from dataclasses import dataclass
from pathlib import Path

from ..inventory import Cluster

DEFAULT_SCOPES = ["https://www.googleapis.com/auth/cloud-platform"]


@dataclass(frozen=True)
class MetadataConfig:
    """Operator-set tunables -- the same JSON-file-under-`/data/config`
    shape as `AutomationConfig`. Values change by an operator editing a
    file and restarting the affected instance(s), not at runtime.
    """

    # The narrow, minimally-privileged service account every instance
    # impersonates. Singular and shared across sandboxes: docs/design.md
    # calls for "a second... service account" (one), not one per sandbox
    # -- the per-sandbox multiplicity is about network position and
    # attribution, not differing privilege.
    service_account_email: str
    project_id: str
    numeric_project_id: int = 0
    # The broad primary key, read via GOOGLE_APPLICATION_CREDENTIALS as
    # the impersonation *source* -- never served directly. docs/design.md,
    # "Secrets on /data".
    key_path: Path = Path("/data/secrets/gcp-service-account.json")
    binary: str = "gce_metadata_server"
    # System user each instance runs as. provision/controller.sh creates
    # it (docs/roadmap.md item 3); an operator placing key_path by hand
    # still has to chown it to this user -- see provision/controller.sh's
    # own comments on /data/secrets' permissions, and docs/runbook.md.
    metadata_user: str = "grain-metadata"

    @classmethod
    def load(cls, path: Path) -> "MetadataConfig":
        raw = json.loads(path.read_text())
        if "key_path" in raw:
            raw["key_path"] = Path(raw["key_path"])
        return cls(**raw)


@dataclass(frozen=True)
class InstancePaths:
    config_path: Path
    log_path: Path


def instance_paths(data_dir: Path, sandbox: str) -> InstancePaths:
    state_dir = data_dir / "state" / "metadata-server"
    return InstancePaths(
        config_path=state_dir / f"{sandbox}.config.json",
        log_path=state_dir / f"{sandbox}.log",
    )


def render_instance_config(config: MetadataConfig, sandbox: str) -> str:
    """The `gce_metadata_server` config file content for one sandbox's
    instance.

    Only the fields impersonation mode actually reads: the target service
    account email and the project -- confirmed live, "the serviceAccountEmail
    and project are taken from the config file's default service account."
    Everything else the real metadata server can report (disks,
    network-interfaces, hostname, ...) is decorative instance metadata ADC
    never asks for on the token-minting path, so it's left out rather than
    faked.
    """
    doc = {
        "computeMetadata": {
            "v1": {
                "instance": {
                    "id": _stable_instance_id(sandbox),
                    "serviceAccounts": {
                        "default": {
                            "aliases": ["default"],
                            "email": config.service_account_email,
                            "scopes": list(DEFAULT_SCOPES),
                        }
                    },
                },
                "project": {
                    "projectId": config.project_id,
                    "numericProjectId": config.numeric_project_id,
                },
            }
        }
    }
    return json.dumps(doc, indent=2) + "\n"


def _stable_instance_id(sandbox: str) -> int:
    # Nothing on the token-minting path reads this; it only has to look
    # like a real (positive, distinct-per-sandbox) instance id. crc32, not
    # the builtin hash() -- that's salted per-process (PYTHONHASHSEED) and
    # would make the rendered config change on every restart for no reason.
    return zlib.crc32(sandbox.encode())


def build_argv(config: MetadataConfig, cluster: Cluster, sandbox: str,
                paths: InstancePaths) -> list[str]:
    """The full argv for one sandbox's instance. `GOOGLE_APPLICATION_CREDENTIALS`
    is deliberately not here -- see the module docstring -- it belongs in
    the launched process's environment, which `launcher.py` sets via
    `systemd-run --setenv`.
    """
    return [
        config.binary,
        f"-configFile={paths.config_path}",
        f"-interface={cluster.controller_ip}",
        f"-port=:{cluster.metadata_port(sandbox)}",
        "--impersonate",
        "-jsonLog",
        "-debug",
        f"-logTarget={paths.log_path}",
    ]
