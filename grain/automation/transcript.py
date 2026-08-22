"""Turns a captured Claude Code trajectory (docs/roadmap.md item 10; see
`capture.py`'s docstring for exactly what format this is and why) into a
small sequence of renderable events — pure parsing, no `curses`, so
`tui.py`'s actual screen-drawing code can stay a thin, mostly-untested
wrapper around this instead of the other way around. See `tui.py`'s own
docstring for why that split matters here specifically.

Each JSONL line is one Claude Code event, tagged by its own `type` field
(`system`, `user`, `assistant`, `result`, ...) — confirmed against a real
transcript, see `capture.py`. This module reads only the handful of fields a
trajectory viewer actually needs to render something useful; a field or
event type it doesn't recognize is shown plainly (its raw JSON), never an
error — Claude Code's own event shape grows new fields and subtypes across
versions, and this parser seeing one it doesn't know about should never make
a whole session unbrowsable.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class TranscriptEvent:
    role: str      # "user" | "assistant" | "system" | "result" | "raw" | ...
    summary: str    # one line, for the scrolling list view
    detail: str      # the fuller text, shown when this event is selected


def _text_of(content: Any) -> str:
    """A message's `content` is either a plain string (a typed-in user
    prompt) or a list of typed blocks (`text`, `thinking`, `tool_use`,
    `tool_result`) — both shapes appear in a real transcript (confirmed
    directly, see `capture.py`'s docstring).
    """
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts: list[str] = []
    for block in content:
        if not isinstance(block, dict):
            continue
        block_type = block.get("type")
        if block_type == "text":
            parts.append(block.get("text", ""))
        elif block_type == "tool_use":
            name = block.get("name", "tool")
            args = block.get("input", {})
            parts.append(f"[tool_use: {name} {json.dumps(args)}]")
        elif block_type == "tool_result":
            result_content = block.get("content", "")
            if isinstance(result_content, list):
                result_content = " ".join(
                    b.get("text", "") for b in result_content if isinstance(b, dict)
                )
            marker = "error" if block.get("is_error") else "ok"
            parts.append(f"[tool_result ({marker}): {result_content}]")
        elif block_type == "thinking":
            continue  # internal reasoning -- not shown in the trajectory view
    return "\n".join(p for p in parts if p)


def _one_line(text: str, limit: int = 100) -> str:
    text = " ".join(text.split())
    return text if len(text) <= limit else text[: limit - 1] + "…"


def parse_event(raw_line: str) -> TranscriptEvent | None:
    """One JSONL line -> one event, or None for a blank line (a trailing
    newline in the captured file, most commonly).
    """
    line = raw_line.strip()
    if not line:
        return None
    try:
        data = json.loads(line)
    except json.JSONDecodeError:
        # Not every byte on the wire is guaranteed valid JSON (a truncated
        # capture from a killed run, say) -- shown as-is rather than
        # dropped, so a malformed tail doesn't silently hide from the
        # viewer.
        return TranscriptEvent(role="raw", summary=_one_line(line), detail=line)

    event_type = data.get("type", "unknown")
    if event_type in ("user", "assistant"):
        message = data.get("message") or {}
        text = _text_of(message.get("content"))
        role = message.get("role", event_type)
        return TranscriptEvent(role=role, summary=f"{role}: {_one_line(text)}", detail=text)
    if event_type == "result":
        result_text = data.get("result", "") or ""
        cost = data.get("total_cost_usd")
        turns = data.get("num_turns")
        suffix = f" (cost=${cost}, turns={turns})" if cost is not None else ""
        return TranscriptEvent(
            role="result", summary=f"result:{suffix} {_one_line(result_text)}",
            detail=result_text or json.dumps(data, indent=2),
        )
    if event_type == "system":
        subtype = data.get("subtype", "")
        return TranscriptEvent(
            role="system", summary=f"system[{subtype}]",
            detail=json.dumps(data, indent=2),
        )
    return TranscriptEvent(
        role=str(event_type), summary=f"{event_type}", detail=json.dumps(data, indent=2),
    )


def parse_transcript(text: str) -> list[TranscriptEvent]:
    events = (parse_event(line) for line in text.splitlines())
    return [e for e in events if e is not None]
