"""Shell agent for microvm — interprets JSON envelopes inside the AgentCore microVM.

Envelopes (unary HTTP path, POST /invocations):
    {"op": "exec",      "cmd": ["sh", "-lc", "..."]}
    {"op": "put",       "path": "/tmp/x", "b64": "..."}
    {"op": "get",       "path": "/tmp/x"}
    {"op": "snapshot",  "snap_id": "snp_...", "name": "demo", "mode": "s3", "sandbox_id": "mvm_..."}
    {"op": "resume",    "alias": "sess-1", "locator": "{...}", "mode": "s3", "sandbox_id": "mvm_..."}
    {"op": "checkpoint", "sandbox_id": "mvm_..."}
    {"op": "terminate"}

The interactive shell runs over the WebSocket path (`/ws`) instead of the
unary envelope above. The Go CLI dials it with a SigV4-signed handshake.
"""

from __future__ import annotations

import asyncio
import base64
import json
import os
import pty
import signal
import subprocess
from typing import Any

try:
    from bedrock_agentcore import BedrockAgentCoreApp  # type: ignore
except ImportError:  # pragma: no cover - allow tests without SDK installed
    BedrockAgentCoreApp = None  # type: ignore

from snapshotter import Snapshotter, _safe_id, make_snapshotter

# Resolved lazily on first snapshot/resume; module-level so a single process
# uses one backend for its lifetime (mode is fixed at runtime registration).
# Tests reset this between cases via the reset_snapshotter fixture.
_snapshotter: Snapshotter | None = None


def _get_snapshotter() -> Snapshotter:
    global _snapshotter
    if _snapshotter is None:
        _snapshotter = make_snapshotter()
    return _snapshotter

# Match the Go side: AgentCore caps each WebSocket frame at 32 KB; chunk reads
# slightly below that so we stay clear of any per-frame envelope overhead.
SHELL_READ_CHUNK = 30 * 1024


def _resolve_cwd(req: dict) -> str | None:
    """Pick a default working directory for exec in tiered mode.

    Returns `<MICROVM_S3FILES_MOUNT_PATH>/<sandbox_id>` (creating it on demand)
    when mode is tiered and the sandbox_id passes `_safe_id`. Otherwise returns
    None so the subprocess inherits the agent's cwd (preserving today's
    behavior). A failure to create the directory also falls back to None so a
    broken mount never breaks routine command execution.
    """
    if os.environ.get("MICROVM_SNAPSHOT_MODE", "") != "tiered":
        return None
    sandbox_id = req.get("sandbox_id", "")
    if not sandbox_id:
        return None
    try:
        _safe_id("sandbox_id", sandbox_id)
    except ValueError:
        return None
    workspace = os.environ.get("MICROVM_S3FILES_MOUNT_PATH", "/workspace")
    cwd = os.path.join(workspace, sandbox_id)
    try:
        os.makedirs(cwd, exist_ok=True)
    except OSError:
        return None
    return cwd


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
            cwd=_resolve_cwd(req),
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


def parse_resize(msg: Any) -> tuple[int, int] | None:
    """Decode a {'type':'resize','cols':N,'rows':N} control message.

    Accepts either a dict (already-decoded JSON) or a str/bytes payload.
    Returns (cols, rows) or None if the message isn't a valid resize frame.
    """
    if isinstance(msg, (bytes, bytearray)):
        try:
            msg = msg.decode("utf-8")
        except UnicodeDecodeError:
            return None
    if isinstance(msg, str):
        try:
            msg = json.loads(msg)
        except (ValueError, json.JSONDecodeError):
            return None
    if not isinstance(msg, dict):
        return None
    if msg.get("type") != "resize":
        return None
    try:
        cols = int(msg.get("cols", 0) or 0)
        rows = int(msg.get("rows", 0) or 0)
    except (TypeError, ValueError):
        return None
    if cols <= 0 or rows <= 0:
        return None
    return cols, rows


def _set_winsize(fd: int, cols: int, rows: int) -> None:
    try:
        import fcntl
        import struct
        import termios

        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass


async def shell_session(websocket) -> None:
    """Run an interactive PTY shell, shuttling bytes between websocket <-> pty.

    The first frame is expected (but not required) to be a JSON resize.
    Subsequent text frames are parsed as control messages; binary frames go
    straight to the PTY as stdin. PTY stdout becomes binary websocket frames.
    """
    pid, fd = pty.fork()
    if pid == 0:  # child
        os.execvp("/bin/sh", ["/bin/sh", "-i"])
        return

    loop = asyncio.get_running_loop()
    stop = asyncio.Event()

    async def pump_pty_to_ws() -> None:
        try:
            while not stop.is_set():
                try:
                    chunk = await loop.run_in_executor(None, lambda: os.read(fd, SHELL_READ_CHUNK))
                except OSError:
                    break
                if not chunk:
                    break
                try:
                    await websocket.send_bytes(chunk)
                except Exception:
                    break
        finally:
            stop.set()

    async def pump_ws_to_pty() -> None:
        try:
            while not stop.is_set():
                try:
                    msg = await websocket.receive()
                except Exception:
                    break
                t = msg.get("type")
                if t == "websocket.disconnect":
                    break
                if t != "websocket.receive":
                    continue
                if msg.get("bytes") is not None:
                    try:
                        os.write(fd, msg["bytes"])
                    except OSError:
                        break
                elif msg.get("text") is not None:
                    parsed = parse_resize(msg["text"])
                    if parsed is not None:
                        _set_winsize(fd, *parsed)
                    else:
                        try:
                            os.write(fd, msg["text"].encode("utf-8"))
                        except OSError:
                            break
        finally:
            stop.set()

    await websocket.accept()
    try:
        await asyncio.gather(pump_pty_to_ws(), pump_ws_to_pty())
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
        try:
            os.kill(pid, signal.SIGHUP)
        except (OSError, ProcessLookupError):
            pass
        try:
            # Reap so we don't accumulate zombies.
            os.waitpid(pid, os.WNOHANG)
        except (ChildProcessError, OSError):
            pass


def handle_snapshot(req: dict) -> dict:
    name = req.get("name", "")
    # Go side mints snap_id and sends it down so the shellagent can use it as
    # the S3 key / EFS subdir / etc. Older clients fall back to name; failing
    # both, we stamp a deterministic placeholder so an alias-only response is
    # still well-formed.
    snap_id = req.get("snap_id") or name or "snap"
    sandbox_id = req.get("sandbox_id", "")
    try:
        return _get_snapshotter().snapshot(snap_id, name, sandbox_id=sandbox_id)
    except Exception as e:  # noqa: BLE001 — surface any backend error to the caller
        return {"alias": "", "name": name, "locator": "", "error": str(e)}


def handle_resume(req: dict) -> dict:
    sandbox_id = req.get("sandbox_id", "")
    try:
        return _get_snapshotter().resume(
            req.get("locator", ""),
            req.get("alias", ""),
            sandbox_id=sandbox_id,
        )
    except Exception as e:  # noqa: BLE001
        return {"alias": "", "error": str(e)}


def handle_terminate(req: dict) -> dict:
    return {"ok": True}


def handle_checkpoint(req: dict) -> dict:
    """Promote tier-1 cache contents into the tier-2 workspace.

    In tiered mode, /var/microvm/cache/<sandbox_id>/ is fast scratch storage
    that the snapshot does NOT capture. To include cache contents in the
    next snapshot, the user (or their tooling) drops files into a
    `promote/` subdirectory and invokes this op. We rsync `promote/` into
    `<workspace>/<sandbox_id>/cache-promoted/` so the next snapshot picks
    it up. `rsync -a --delete` keeps the destination an exact mirror so
    removing a file from promote/ also removes it from the workspace copy.
    """
    sandbox_id = req.get("sandbox_id", "")
    try:
        _safe_id("sandbox_id", sandbox_id)
    except ValueError as e:
        return {"ok": False, "error": str(e)}

    mode = os.environ.get("MICROVM_SNAPSHOT_MODE", "")
    if mode != "tiered":
        return {
            "ok": False,
            "error": f"checkpoint only supported in tiered mode (got {mode!r})",
        }

    cache_root = os.environ.get("MICROVM_CACHE_ROOT", "/var/microvm/cache")
    workspace = os.environ.get("MICROVM_S3FILES_MOUNT_PATH", "/workspace")

    src = os.path.join(cache_root, sandbox_id, "promote") + "/"
    dst = os.path.join(workspace, sandbox_id, "cache-promoted") + "/"

    try:
        os.makedirs(src, exist_ok=True)
        os.makedirs(dst, exist_ok=True)
        subprocess.run(
            ["rsync", "-a", "--delete", src, dst],
            check=True,
            capture_output=True,
            text=True,
        )
        return {"ok": True, "synced": dst}
    except subprocess.CalledProcessError as e:
        return {"ok": False, "error": f"rsync failed: {e.stderr or e}"}
    except OSError as e:
        return {"ok": False, "error": str(e)}


def dispatch(req: dict) -> Any:
    op = req.get("op")
    if op == "exec":
        return handle_exec(req)
    if op == "put":
        return handle_put(req)
    if op == "get":
        return handle_get(req)
    if op == "snapshot":
        return handle_snapshot(req)
    if op == "resume":
        return handle_resume(req)
    if op == "terminate":
        return handle_terminate(req)
    if op == "checkpoint":
        return handle_checkpoint(req)
    return {"error": f"unknown op: {op!r}"}


if BedrockAgentCoreApp is not None:  # pragma: no cover - runtime entrypoint
    app = BedrockAgentCoreApp()

    @app.entrypoint
    def handler(req):
        return dispatch(req)

    @app.websocket
    async def shell_ws(websocket, context):  # context is the AgentCore RequestContext
        await shell_session(websocket)

    if __name__ == "__main__":
        app.run()
