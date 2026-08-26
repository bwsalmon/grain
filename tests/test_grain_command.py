"""bwsalmon/agents#137: `grain ...` should work on both the host and the
controller without a `cd` first or spelling out `python3 -m grain.cli`.
`bin/grain` is the wrapper that makes that true; these tests check it
actually resolves its own real location (so it still works once symlinked
onto PATH from an unrelated directory, which is the whole point) and that
the two provisioning scripts that install it automatically still do.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent
GRAIN = REPO_ROOT / "bin" / "grain"
CONTROLLER_SH = REPO_ROOT / "provision" / "controller.sh"
DEPLOY_SH = REPO_ROOT / "terraform" / "gcp" / "files" / "deploy.sh"


def test_bin_grain_exists_and_is_executable():
    assert GRAIN.exists()
    assert GRAIN.stat().st_mode & 0o111, "bin/grain must be executable"


def test_bin_grain_has_a_posix_sh_shebang():
    assert GRAIN.read_text().startswith("#!/bin/sh")


def _run_help(argv: list[str], cwd: Path) -> subprocess.CompletedProcess:
    return subprocess.run(argv + ["--help"], cwd=cwd, capture_output=True, text=True)


def test_bin_grain_run_directly_matches_python_dash_m():
    direct = _run_help([str(GRAIN)], cwd=REPO_ROOT)
    module = subprocess.run(
        [sys.executable, "-m", "grain.cli", "--help"],
        cwd=REPO_ROOT, capture_output=True, text=True,
    )
    assert direct.returncode == 0
    assert direct.stdout == module.stdout


def test_bin_grain_still_resolves_the_repo_root_when_symlinked_elsewhere(tmp_path):
    # The actual use case: a symlink at /usr/local/bin/grain, invoked from
    # any cwd. `dirname "$0"` alone would resolve to the symlink's own
    # directory instead of this checkout -- this is the regression that
    # matters.
    link = tmp_path / "grain"
    link.symlink_to(GRAIN)
    elsewhere = tmp_path / "elsewhere"
    elsewhere.mkdir()
    result = _run_help([str(link)], cwd=elsewhere)
    assert result.returncode == 0
    assert "usage: grain" in result.stdout


def test_provision_controller_symlinks_bin_grain_onto_path():
    text = CONTROLLER_SH.read_text()
    assert "ln -sf /opt/grain/bin/grain /usr/local/bin/grain" in text


def test_gcp_deploy_symlinks_bin_grain_onto_path():
    text = DEPLOY_SH.read_text()
    assert 'ln -sf "$SRC/bin/grain" /usr/local/bin/grain' in text
