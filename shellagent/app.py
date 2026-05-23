"""Shell agent for microvm — interprets JSON envelopes inside the AgentCore microVM.

Envelope:
    {"op": "exec",  "cmd": ["sh", "-lc", "..."]}
    {"op": "put",   "path": "/tmp/x", "b64": "..."}
    {"op": "get",   "path": "/tmp/x"}
    {"op": "shell", "tty": true, "cols": 80, "rows": 24}

Responses are JSON dicts; "shell" yields raw PTY bytes as a stream.
"""

from __future__ import annotations

import base64
import os
import pty
import select
import subprocess
from typing import Any, Iterator

try:
    from bedrock_agentcore import BedrockAgentCoreApp  # type: ignore
except ImportError:  # pragma: no cover - allow tests without SDK installed
    BedrockAgentCoreApp = None  # type: ignore


def handle_exec(req: dict) -> dict:
    cmd = req.get("cmd") or []
    if not cmd:
        return {"stdout": "", "stderr": "", "exit": 2, "error": "exec: empty cmd"}
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=req.get("timeout_sec", 300),
        )
        return {
            "stdout": proc.stdout,
            "stderr": proc.stderr,
            "exit": proc.returncode,
        }
    except subprocess.TimeoutExpired as e:
        return {"stdout": "", "stderr": str(e), "exit": 124, "error": "timeout"}
    except FileNotFoundError as e:
        return {"stdout": "", "stderr": str(e), "exit": 127, "error": "command not found"}


def handle_put(req: dict) -> dict:
    path = req.get("path")
    b64 = req.get("b64", "")
    if not path:
        return {"ok": False, "bytes": 0, "error": "put: path required"}
    try:
        data = base64.b64decode(b64)
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)
        return {"ok": True, "bytes": len(data)}
    except OSError as e:
        return {"ok": False, "bytes": 0, "error": str(e)}


def handle_get(req: dict) -> dict:
    path = req.get("path")
    if not path:
        return {"b64": "", "bytes": 0, "error": "get: path required"}
    try:
        with open(path, "rb") as f:
            data = f.read()
        return {"b64": base64.b64encode(data).decode("ascii"), "bytes": len(data)}
    except OSError as e:
        return {"b64": "", "bytes": 0, "error": str(e)}


def handle_shell(req: dict) -> Iterator[bytes]:
    cols = int(req.get("cols", 80) or 80)
    rows = int(req.get("rows", 24) or 24)
    pid, fd = pty.fork()
    if pid == 0:  # child
        os.execvp("/bin/sh", ["/bin/sh", "-i"])
        return
    try:
        import fcntl
        import struct
        import termios

        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass
    try:
        while True:
            r, _, _ = select.select([fd], [], [], 1.0)
            if fd in r:
                try:
                    chunk = os.read(fd, 4096)
                except OSError:
                    break
                if not chunk:
                    break
                yield chunk
    finally:
        try:
            os.close(fd)
        except OSError:
            pass


def dispatch(req: dict) -> Any:
    op = req.get("op")
    if op == "exec":
        return handle_exec(req)
    if op == "put":
        return handle_put(req)
    if op == "get":
        return handle_get(req)
    if op == "shell":
        return handle_shell(req)
    return {"error": f"unknown op: {op!r}"}


if BedrockAgentCoreApp is not None:  # pragma: no cover - runtime entrypoint
    app = BedrockAgentCoreApp()

    @app.entrypoint
    def handler(req):
        return dispatch(req)

    if __name__ == "__main__":
        app.run()
