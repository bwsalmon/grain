"""Pulls a finished session's Claude Code trajectory off the sandbox before
its slot is freed for reuse (docs/roadmap.md item 10) — capture-on-
completion, not fetch-on-demand. Sandboxes are long-lived and a unit's name
is fixed per sandbox, reused verbatim by the next dispatch
(`dispatch.py`'s `unit_name`) — so is the transcript path
(`dispatch.transcript_path`) that name derives. A later "fetch it when
someone wants to browse" would therefore find either nothing (the sandbox
already ran another task and the file was overwritten) or the wrong task's
content. The file has to come off *before* `sweeper.py` frees the slot for
reuse — this module is called from exactly that release path.

**What `claude -p` actually leaves behind, checked rather than assumed.**
Claude Code persists a session's transcript to disk by default — an
interactive session and a `-p` (headless) one alike; `--no-session-
persistence` is the opt-out, confirmed against Claude Code's own docs
(`code.claude.com/docs/en/headless`: "Sessions can be resumed with
`--resume`... The `--no-session-persistence` flag disables session
persistence entirely"). That default location is
`~/.claude/projects/<cwd, "/" replaced with "-">/<session-id>.jsonl` —
confirmed directly against a real transcript file on the machine this was
built on (not guessed): every line is one JSON object carrying a `type`
field (`user`, `assistant`, `system`, ...); a `user`/`assistant` line's
`message` is an Anthropic-API-shaped object (`role`, and `content` either a
plain string or a list of typed blocks — `text`, `tool_use`, `tool_result`,
`thinking`); every line also carries `session_id`, `timestamp`, `cwd`,
`uuid`. That default path depends on an *undocumented* encoding of the
working directory and a session ID Claude Code assigns itself — reverse-
engineering both, inside a bare, unprovisioned sandbox, to find one file
this project needs anyway, is a needless coupling to an internal Claude
Code implementation detail this project doesn't control.

So `dispatch.py` asks for the *documented* structured stream explicitly
instead: `claude -p --output-format stream-json --verbose`, redirected to a
fixed path this module and `dispatch.py` both derive from `unit_name()`
alone (`dispatch.transcript_path`). Per Claude Code's own docs
(`code.claude.com/docs/en/headless`, "Stream responses"): "newline-delimited
JSON... The last line of the stream is a `result` message with the final
response text, cost, and session metadata." Same JSONL shape, same
per-line `type`-tagged event structure as the real on-disk session file
(confirmed above) — assistant/user/system events plus a final `result`
event carrying `total_cost_usd`/`num_turns`/`is_error` — but at a location
under this project's own control rather than one it would have to guess.
"""

from __future__ import annotations

from .dispatch import transcript_path
from ..run import Runner


def capture_trajectory(runner: Runner, unit: str) -> str | None:
    """Reads `unit`'s transcript file over the same `Runner` (typically an
    `SshRunner`) `dispatch.py`/`sweeper.py` already use for everything else
    — no new channel, no `scp`/`sftp`, just `cat` over the existing SSH exec
    path.

    Never raises — the same discipline `cleanup.py`/`health.py` already
    hold to for this exact call site (a step that runs on every sandbox
    release must not be the reason a slot fails to free): a unit that never
    started, a `claude -p` that crashed before writing anything, or a
    transient `cat` failure all come back as `None`, not an exception.
    `None` specifically (never `""`) is what lets `SessionHistory.record`
    tell "nothing to capture" apart from "captured an empty file" without a
    second sentinel.
    """
    result = runner.run(["cat", transcript_path(unit)], check=False)
    if result.returncode != 0 or not result.stdout:
        return None
    return result.stdout
