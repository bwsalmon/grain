"""A small MCP server, run on the controller as the `claude -p` session's
*only* available tool surface, that lets the dispatched agent do real work
inside its assigned sandbox without the process holding the Claude Code
credential ever running there itself.

Why this exists: see docs/roadmap.md item 8's "Update" -- a full live
session found that running `claude -p` inside the sandbox leaks its own
OAuth credential into any unsandboxed Bash subprocess's environment
trivially (confirmed live via a plain `env`), and that the agent readily
discovers `dangerouslyDisableSandbox: true` on its own to get there.
Claude Code's own sandbox/permission settings cannot close that gap --
`--dangerously-skip-permissions` was tried and made things worse, and
Landlock (kernel-level, immune to `dangerouslyDisableSandbox`) can protect a
*file* but has no concept of environment variables at all. The only real
fix is to never put the credential inside the untrusted execution
environment in the first place: `claude -p` now runs on the controller,
with every native tool disabled (`--tools ""`) and replaced by the four
tools this module implements, which reach the sandbox over SSH instead.

`file_path` on every tool always resolves against the *assigned* sandbox's
workspace -- never the controller, and never model-controllable. The target
sandbox (address/user/key/workspace) is fixed at process startup via CLI
args `dispatch.py` bakes into the per-dispatch `--mcp-config` JSON; nothing
in a tool call's own arguments can redirect it elsewhere.

Newline-delimited JSON-RPC over stdin/stdout, no `Content-Length` framing --
that is the real MCP stdio transport, not a simplification. Hand-rolled
rather than pulled from the `mcp` package: this project has no third-party
runtime dependencies (see pyproject.toml), and the protocol surface this
server actually needs -- `initialize`, `notifications/initialized`,
`tools/list`, `tools/call` -- is small enough that adding a dependency
would cost more than it saves.

Tool schemas deliberately mirror Claude Code's own native `Bash`/`Read`/
`Edit`/`Write` tools field-for-field (mirror-image logic in `read_file`'s
line-numbered output too, not just its parameters) rather than an
OpenAI-Codex-style `apply_patch`/V4A diff -- the agent here is Claude, which
was never trained to produce V4A, but has extensive trained behavior around
its own native tool shapes.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import shlex
import sys
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import TextIO

from ..run import Runner, RealRunner
from .ssh import SshRunner

PROTOCOL_VERSION = "2024-11-05"
SERVER_NAME = "grain-sandbox"

TOOLS = [
    {
        "name": "run_command",
        "description": "Run a shell command in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "command": {"type": "string"},
                "timeout": {
                    "type": "number",
                    "description": "Timeout in milliseconds, max 600000",
                },
                "description": {
                    "type": "string",
                    "description": "Short description of what this command does",
                },
            },
            "required": ["command"],
        },
    },
    {
        "name": "read_file",
        "description": "Read a file from your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "offset": {"type": "integer", "description": "Line number to start from"},
                "limit": {"type": "integer", "description": "Number of lines to read"},
            },
            "required": ["file_path"],
        },
    },
    {
        "name": "edit_file",
        "description": "Replace an exact string in a file in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "old_string": {"type": "string"},
                "new_string": {"type": "string"},
                "replace_all": {"type": "boolean", "default": False},
            },
            "required": ["file_path", "old_string", "new_string"],
        },
    },
    {
        "name": "write_file",
        "description": "Write (create or overwrite) a file in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "content": {"type": "string"},
            },
            "required": ["file_path", "content"],
        },
    },
]


@dataclass(frozen=True)
class ToolResult:
    text: str
    is_error: bool = False


def run_command(runner: Runner, workspace: str, command: str, *,
                 timeout: float | None = None, description: str | None = None) -> ToolResult:
    """`timeout` (milliseconds, matching native Bash's own unit) is applied
    with the `timeout` coreutil on the sandbox side rather than adding
    timeout support to `Runner`/`SshRunner` -- smaller, more surgical, and
    `Runner` stays a plain synchronous call/response abstraction other
    callers already depend on.
    """
    shell = f"cd {shlex.quote(workspace)} && {command}"
    if timeout is not None:
        seconds = max(1, int(timeout / 1000))
        shell = f"timeout {seconds} bash -c {shlex.quote(shell)}"
    result = runner.run(["bash", "-c", shell], check=False)
    text = f"exit={result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    return ToolResult(text=text, is_error=result.returncode != 0)


def _read_remote(runner: Runner, path: str) -> tuple[str | None, ToolResult | None]:
    result = runner.run(["cat", "--", path], check=False)
    if result.returncode != 0:
        return None, ToolResult(text=f"Error reading {path}: {result.stderr.strip()}", is_error=True)
    return result.stdout, None


def read_file(runner: Runner, workspace: str, file_path: str, *,
               offset: int | None = None, limit: int | None = None) -> ToolResult:
    content, error = _read_remote(runner, file_path)
    if error is not None:
        return error
    lines = content.splitlines()
    start = offset or 0
    end = start + limit if limit is not None else len(lines)
    # cat -n-style numbering: the model's own Edit-quoting behavior depends
    # on having seen this exact format, not just the raw file content.
    numbered = "\n".join(f"{i + start + 1:6d}\t{line}" for i, line in enumerate(lines[start:end]))
    return ToolResult(text=numbered)


def _write_remote(runner: Runner, path: str, content: str) -> ToolResult:
    result = runner.run(["dd", f"of={path}", "status=none"], stdin=content, check=False)
    if result.returncode != 0:
        return ToolResult(text=f"Error writing {path}: {result.stderr.strip()}", is_error=True)
    return ToolResult(text=f"Wrote {path}")


def edit_file(runner: Runner, workspace: str, file_path: str, old_string: str,
              new_string: str, *, replace_all: bool = False) -> ToolResult:
    content, error = _read_remote(runner, file_path)
    if error is not None:
        return error
    count = content.count(old_string)
    if count == 0:
        return ToolResult(text=f"String not found in file: {old_string!r}", is_error=True)
    if count > 1 and not replace_all:
        return ToolResult(
            text=(f"String appears {count} times in the file. Use replace_all: "
                  "true to replace every occurrence, or provide more surrounding "
                  "context to uniquely identify the instance you mean."),
            is_error=True,
        )
    new_content = content.replace(old_string, new_string, -1 if replace_all else 1)
    return _write_remote(runner, file_path, new_content)


def write_file(runner: Runner, workspace: str, file_path: str, content: str) -> ToolResult:
    parent = str(PurePosixPath(file_path).parent)
    mkdir_result = runner.run(["mkdir", "-p", parent], check=False)
    if mkdir_result.returncode != 0:
        return ToolResult(text=f"Error creating {parent}: {mkdir_result.stderr.strip()}", is_error=True)
    return _write_remote(runner, file_path, content)


class McpServer:
    """The JSON-RPC method dispatch, kept separate from stdio plumbing
    (`serve()`) so `handle()` can be exercised directly in tests with a
    plain dict in, dict out -- no subprocess, no real stdin/stdout.
    """

    def __init__(self, runner: Runner, workspace: str) -> None:
        self.runner = runner
        self.workspace = workspace

    def handle(self, msg: dict) -> dict | None:
        method = msg.get("method")
        msg_id = msg.get("id")
        if method == "initialize":
            return {
                "jsonrpc": "2.0", "id": msg_id,
                "result": {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": SERVER_NAME, "version": "0.1.0"},
                },
            }
        if method == "notifications/initialized":
            return None
        if method == "tools/list":
            return {"jsonrpc": "2.0", "id": msg_id, "result": {"tools": TOOLS}}
        if method == "tools/call":
            return self._handle_call(msg_id, msg.get("params") or {})
        if msg_id is not None:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32601, "message": f"unknown method {method}"}}
        return None

    def _handle_call(self, msg_id, params: dict) -> dict:
        name = params.get("name")
        args = params.get("arguments") or {}
        try:
            result = self._dispatch_tool(name, args)
        except KeyError as exc:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32602, "message": f"missing argument {exc}"}}
        if result is None:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32602, "message": f"unknown tool {name}"}}
        return {
            "jsonrpc": "2.0", "id": msg_id,
            "result": {"content": [{"type": "text", "text": result.text}],
                       "isError": result.is_error},
        }

    def _dispatch_tool(self, name: str, args: dict) -> ToolResult | None:
        if name == "run_command":
            return run_command(self.runner, self.workspace, args["command"],
                                timeout=args.get("timeout"), description=args.get("description"))
        if name == "read_file":
            return read_file(self.runner, self.workspace, args["file_path"],
                              offset=args.get("offset"), limit=args.get("limit"))
        if name == "edit_file":
            return edit_file(self.runner, self.workspace, args["file_path"],
                              args["old_string"], args["new_string"],
                              replace_all=args.get("replace_all", False))
        if name == "write_file":
            return write_file(self.runner, self.workspace, args["file_path"], args["content"])
        return None


def serve(runner: Runner, workspace: str, *, stdin: TextIO = sys.stdin,
          stdout: TextIO = sys.stdout) -> None:
    server = McpServer(runner, workspace)
    for line in stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        response = server.handle(msg)
        if response is not None:
            stdout.write(json.dumps(response) + "\n")
            stdout.flush()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--address", required=True)
    parser.add_argument("--user", required=True)
    parser.add_argument("--key-path", required=True)
    parser.add_argument("--workspace", required=True)
    args = parser.parse_args()
    runner = SshRunner(
        inner=RealRunner(), user=args.user,
        address=ipaddress.IPv4Address(args.address), key_path=Path(args.key_path),
    )
    serve(runner, args.workspace)


if __name__ == "__main__":
    main()
