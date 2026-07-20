#!/usr/bin/env python3
"""Fixed-command authentication helper for VibeDock.

The public control plane selects one of the fixed operations below. Provider
output is never copied to this process' stderr or to the Docker log stream.
"""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, BinaryIO, Iterator
from urllib.parse import urlsplit

PROTOCOL_VERSION = 1
CODEX_VERSION = "0.144.6"
CLAUDE_VERSION = "2.1.215"
TTYD_VERSION = "1.7.7"
CREDENTIALS_ROOT = Path("/vibedock/credentials")
HELPER_PATH = "/usr/local/bin/vibedock-auth-helper"
TTYD_PATH = "/usr/local/bin/ttyd"
TTYD_PORT = 7681
MAX_MESSAGE_BYTES = 1 << 20
MAX_STATE_BYTES = 64 << 10
ACCOUNT_RE = re.compile(r"^[a-z0-9]{12}$")
ATTEMPT_RE = re.compile(r"^[a-z0-9]{24}$")
CLEANUP_RE = re.compile(r"^[a-z0-9]{32}$")
USER_CODE_RE = re.compile(r"^[A-Z0-9-]{4,32}$")
PROVIDERS = {"codex", "claude"}
TERMINAL_STATES = {"completed", "cancelled", "expired", "failed"}
OFFICIAL_CODEX_URLS = {("auth.openai.com", "/codex/device")}
GENERIC_MESSAGES = {
    "starting": "Official login helper is starting",
    "awaiting_user": "Waiting for official provider login",
    "verifying": "Verifying login with the official CLI",
    "completed": "Official CLI login verified",
    "cancelled": "Login cancelled",
    "expired": "Login expired",
    "failed": "Official login helper failed",
}


class HelperError(Exception):
    """An expected failure whose details must not cross the helper boundary."""

    def __init__(self, code: str, message: str = "") -> None:
        super().__init__(message or code)
        self.code = code


class ProtocolError(HelperError):
    def __init__(self, code: str = "provider_protocol_incompatible") -> None:
        super().__init__(code)


def validate_account(value: str) -> str:
    if not ACCOUNT_RE.fullmatch(value or ""):
        raise HelperError("invalid_account")
    return value


def validate_attempt(value: str) -> str:
    if not ATTEMPT_RE.fullmatch(value or ""):
        raise HelperError("invalid_attempt")
    return value


def validate_provider(value: str) -> str:
    if value not in PROVIDERS:
        raise HelperError("invalid_provider")
    return value


def validate_cleanup(value: str) -> str:
    if not CLEANUP_RE.fullmatch(value or ""):
        raise HelperError("invalid_cleanup")
    return value


def validate_expiry(value: int) -> int:
    now = int(time.time())
    if value <= now or value > now + 16 * 60:
        raise HelperError("invalid_expiry")
    return value


def _inside(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
        return True
    except ValueError:
        return False


def _ensure_root(root: Path) -> Path:
    if not root.is_absolute():
        raise HelperError("unsafe_credentials_path")
    try:
        info = os.lstat(root)
    except FileNotFoundError:
        raise HelperError("credentials_unavailable") from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise HelperError("unsafe_credentials_path")
    resolved = root.resolve(strict=True)
    if resolved != root:
        raise HelperError("unsafe_credentials_path")
    return resolved


def _assert_components(root: Path, path: Path, *, allow_leaf_symlink: bool = False) -> None:
    root = _ensure_root(root)
    if not path.is_absolute() or not _inside(root, path):
        raise HelperError("unsafe_credentials_path")
    relative = path.relative_to(root)
    current = root
    for index, component in enumerate(relative.parts):
        if component in {"", ".", ".."}:
            raise HelperError("unsafe_credentials_path")
        current /= component
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            continue
        is_leaf = index == len(relative.parts) - 1
        if stat.S_ISLNK(info.st_mode) and not (allow_leaf_symlink and is_leaf):
            raise HelperError("unsafe_credentials_path")
        if not is_leaf and not stat.S_ISDIR(info.st_mode):
            raise HelperError("unsafe_credentials_path")


def _mkdir_secure(root: Path, path: Path) -> None:
    _assert_components(root, path)
    current = root
    for component in path.relative_to(root).parts:
        current /= component
        try:
            os.mkdir(current, 0o700)
        except FileExistsError:
            info = os.lstat(current)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                raise HelperError("unsafe_credentials_path") from None
    os.chmod(path, 0o700)


def _assert_tree_safe(root: Path, path: Path) -> None:
    _assert_components(root, path)
    if not path.exists():
        raise HelperError("credentials_missing")
    for directory, dirs, files in os.walk(path, topdown=True, followlinks=False):
        directory_path = Path(directory)
        _assert_components(root, directory_path)
        for name in dirs + files:
            child = directory_path / name
            info = os.lstat(child)
            if stat.S_ISLNK(info.st_mode):
                raise HelperError("unsafe_credentials_path")
            if not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)):
                raise HelperError("unsafe_credentials_path")


def _remove_tree(root: Path, path: Path) -> bool:
    _assert_components(root, path, allow_leaf_symlink=True)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        os.unlink(path)
        return True
    _assert_tree_safe(root, path)
    shutil.rmtree(path)
    return True


def _atomic_json(root: Path, path: Path, value: dict[str, Any]) -> None:
    _assert_components(root, path)
    _mkdir_secure(root, path.parent)
    encoded = (json.dumps(value, separators=(",", ":"), sort_keys=True) + "\n").encode()
    if len(encoded) > MAX_STATE_BYTES:
        raise HelperError("state_too_large")
    descriptor, temporary = tempfile.mkstemp(prefix=".state-", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb", closefd=True) as output:
            output.write(encoded)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)


def _read_json(root: Path, path: Path) -> dict[str, Any]:
    _assert_components(root, path)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise HelperError("state_not_found") from None
    if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_STATE_BYTES:
        raise HelperError("invalid_state")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise HelperError("invalid_state") from None
    if not isinstance(value, dict):
        raise HelperError("invalid_state")
    return value


def paths(root: Path, account: str, attempt: str, provider: str) -> dict[str, Path]:
    validate_account(account)
    validate_attempt(attempt)
    validate_provider(provider)
    root = _ensure_root(root)
    base = root / ".auth-attempts" / account / attempt
    result = {
        "root": root,
        "base": base,
        "stage": base / provider,
        "state": base / "state.json",
        "marker": base / "commit.json",
        "previous": base / (provider + ".previous"),
        "stable_parent": root / account,
        "stable": root / account / provider,
        "cancel": base / "cancel",
        "claim": base / "claude.claim",
        "lock": base / "operation.lock",
    }
    for item in result.values():
        _assert_components(root, item)
    return result


@contextlib.contextmanager
def attempt_lock(root: Path, path: Path) -> Iterator[None]:
    _mkdir_secure(root, path.parent)
    _assert_components(root, path)
    descriptor = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        os.close(descriptor)


def base_state(account: str, attempt: str, provider: str, expiry: int) -> dict[str, Any]:
    return {
        "protocol": PROTOCOL_VERSION,
        "account_id": account,
        "attempt_id": attempt,
        "provider": provider,
        "state": "starting",
        "expires_at": expiry,
        "poll_after_ms": 2000,
        "verification_url": "",
        "user_code": "",
        "error_code": "",
        "message": GENERIC_MESSAGES["starting"],
        "terminal_available": False,
    }


def update_state(root: Path, path: Path, state: dict[str, Any], status: str, **fields: Any) -> None:
    if status not in set(GENERIC_MESSAGES):
        raise HelperError("invalid_state")
    state["state"] = status
    state["message"] = GENERIC_MESSAGES[status]
    state.update(fields)
    if status in TERMINAL_STATES:
        state["verification_url"] = ""
        state["user_code"] = ""
        state["terminal_available"] = False
    _atomic_json(root, path, state)


def validate_codex_url(value: str) -> str:
    if not isinstance(value, str) or len(value) < 12 or len(value) > 256:
        raise ProtocolError()
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        raise ProtocolError() from None
    if (
        parsed.scheme != "https"
        or parsed.username is not None
        or parsed.password is not None
        or port not in (None, 443)
        or parsed.query
        or parsed.fragment
        or (parsed.hostname or "").lower() != parsed.hostname
        or ((parsed.hostname or "").lower(), parsed.path) not in OFFICIAL_CODEX_URLS
    ):
        raise ProtocolError()
    return value


def sanitized_environment(provider: str, profile: Path) -> dict[str, str]:
    allowed = {
        "PATH": "/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "HOME": "/home/coder",
        "USER": "coder",
        "LOGNAME": "coder",
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "NO_COLOR": "1",
        "DISABLE_AUTOUPDATER": "1",
    }
    if provider == "codex":
        allowed["CODEX_HOME"] = str(profile)
    elif provider == "claude":
        allowed["CLAUDE_CONFIG_DIR"] = str(profile)
    else:
        raise HelperError("invalid_provider")
    return allowed


def _version_output(command: list[str]) -> str:
    try:
        completed = subprocess.run(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=10,
            check=False,
            env=sanitized_environment("codex", Path("/nonexistent")),
        )
    except (OSError, subprocess.SubprocessError):
        raise HelperError("provider_protocol_incompatible") from None
    if completed.returncode != 0 or len(completed.stdout) > 4096:
        raise HelperError("provider_protocol_incompatible")
    return completed.stdout.decode("utf-8", "replace").strip()


def compatibility_manifest(check_commands: bool = True) -> dict[str, Any]:
    if check_commands:
        actual = {
            "codex": _version_output(["codex", "--version"]),
            "claude": _version_output(["claude", "--version"]),
            "ttyd": _version_output([TTYD_PATH, "--version"]),
        }
        expected_patterns = {
            "codex": re.compile(rf"(?:^|\s){re.escape(CODEX_VERSION)}(?:\s|$)"),
            "claude": re.compile(rf"(?:^|\s){re.escape(CLAUDE_VERSION)}(?:\s|$)"),
            # The pinned ttyd 1.7.7 binary identifies its exact release as
            # `1.7.7-<commit>` rather than ending the version at 1.7.7.
            "ttyd": re.compile(rf"(?:^|\s){re.escape(TTYD_VERSION)}(?:-|\s|$)"),
        }
        if any(not expected_patterns[name].search(output) for name, output in actual.items()):
            raise HelperError("provider_protocol_incompatible")
    return {
        "protocol": PROTOCOL_VERSION,
        "codex_version": CODEX_VERSION,
        "claude_version": CLAUDE_VERSION,
        "ttyd_version": TTYD_VERSION,
    }


class JSONRPC:
    def __init__(self, process: subprocess.Popen[bytes]) -> None:
        if process.stdin is None or process.stdout is None:
            raise ProtocolError()
        self.process = process
        self.input: BinaryIO = process.stdin
        self.output: BinaryIO = process.stdout
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.output, selectors.EVENT_READ)
        self.next_id = 1
        self.pending: list[dict[str, Any]] = []

    def send(self, value: dict[str, Any]) -> None:
        # The app-server JSONL transport carries JSON-RPC messages without the
        # optional `jsonrpc` envelope member. Keep the wire format aligned with
        # the documented stdio protocol while accepting either response form.
        wire_value = dict(value)
        wire_value.pop("jsonrpc", None)
        encoded = (json.dumps(wire_value, separators=(",", ":")) + "\n").encode()
        if len(encoded) > MAX_MESSAGE_BYTES:
            raise ProtocolError()
        try:
            self.input.write(encoded)
            self.input.flush()
        except (BrokenPipeError, OSError):
            raise ProtocolError() from None

    def receive(self, deadline: float) -> dict[str, Any]:
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not self.selector.select(remaining):
            raise HelperError("provider_timeout")
        try:
            line = self.output.readline(MAX_MESSAGE_BYTES + 1)
        except OSError:
            raise ProtocolError() from None
        if not line or len(line) > MAX_MESSAGE_BYTES or not line.endswith(b"\n"):
            raise ProtocolError()
        try:
            value = json.loads(line)
        except (UnicodeError, json.JSONDecodeError):
            raise ProtocolError() from None
        # Codex app-server uses JSON-RPC 2.0 semantics over JSONL, but omits
        # the protocol header on the wire. Accept the documented wire form;
        # reject an explicitly supplied, invalid header.
        if not isinstance(value, dict) or ("jsonrpc" in value and value.get("jsonrpc") != "2.0"):
            raise ProtocolError()
        return value

    def request(self, method: str, params: Any, deadline: float) -> Any:
        request_id = self.next_id
        self.next_id += 1
        message: dict[str, Any] = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            message["params"] = params
        self.send(message)
        while True:
            pending_index = next((index for index, item in enumerate(self.pending) if item.get("id") == request_id), None)
            value = self.pending.pop(pending_index) if pending_index is not None else self.receive(deadline)
            if value.get("id") != request_id:
                self.pending.append(value)
                if len(self.pending) > 256:
                    raise ProtocolError()
                continue
            if "error" in value or "result" not in value:
                raise ProtocolError()
            return value["result"]

    def notification(self, method: str) -> None:
        self.send({"jsonrpc": "2.0", "method": method})

    def wait_notification(self, method: str, deadline: float, cancelled: threading.Event) -> Any:
        while True:
            if cancelled.is_set():
                raise HelperError("cancelled")
            pending_index = next((index for index, item in enumerate(self.pending) if item.get("method") == method and "params" in item), None)
            if pending_index is not None:
                return self.pending.pop(pending_index)["params"]
            short_deadline = min(deadline, time.monotonic() + 0.5)
            try:
                value = self.receive(short_deadline)
            except HelperError as error:
                if error.code == "provider_timeout" and time.monotonic() < deadline:
                    continue
                raise
            if value.get("method") == method and "params" in value:
                return value["params"]
            self.pending.append(value)
            if len(self.pending) > 256:
                raise ProtocolError()

    def close(self) -> None:
        with contextlib.suppress(Exception):
            self.selector.close()


def start_codex(profile: Path) -> tuple[subprocess.Popen[bytes], JSONRPC]:
    try:
        process = subprocess.Popen(
            ["codex", "app-server", "--listen", "stdio://"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=sanitized_environment("codex", profile),
            start_new_session=True,
        )
    except OSError:
        raise HelperError("provider_unavailable") from None
    rpc = JSONRPC(process)
    try:
        initialized = rpc.request(
            "initialize",
            {
                "clientInfo": {"name": "vibedock-auth-helper", "title": "VibeDock Auth Helper", "version": "1"},
                "capabilities": {
                    "experimentalApi": False,
                    "requestAttestation": False,
                    "optOutNotificationMethods": [],
                    "mcpServerOpenaiFormElicitation": False,
                },
            },
            time.monotonic() + 15,
        )
        if not isinstance(initialized, dict) or not isinstance(initialized.get("codexHome"), str):
            raise ProtocolError()
        if Path(initialized["codexHome"]) != profile:
            raise ProtocolError()
        rpc.notification("initialized")
        return process, rpc
    except Exception:
        terminate_process(process)
        rpc.close()
        raise


def terminate_process(process: subprocess.Popen[Any], grace: float = 2.0) -> None:
    if process.poll() is not None:
        return
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        with contextlib.suppress(subprocess.TimeoutExpired):
            process.wait(timeout=1)


def validate_account_result(value: Any) -> bool:
    if not isinstance(value, dict) or not isinstance(value.get("requiresOpenaiAuth"), bool):
        raise ProtocolError()
    account = value.get("account")
    if account is None:
        return False
    if not isinstance(account, dict) or account.get("type") not in {"chatgpt", "apiKey"}:
        raise ProtocolError()
    return True


def codex_account(profile: Path) -> bool:
    process, rpc = start_codex(profile)
    try:
        result = rpc.request("account/read", {"refreshToken": False}, time.monotonic() + 15)
        return validate_account_result(result)
    finally:
        rpc.close()
        terminate_process(process)


def _write_marker(root: Path, marker: Path, phase: str, provider: str) -> None:
    _atomic_json(root, marker, {"version": 1, "phase": phase, "provider": provider})


def recover_commit(root: Path, item: dict[str, Path], provider: str) -> None:
    marker = item["marker"]
    try:
        value = _read_json(root, marker)
    except HelperError as error:
        if error.code == "state_not_found":
            return
        raise
    if value.get("version") != 1 or value.get("provider") != provider:
        raise HelperError("invalid_transaction")
    phase = value.get("phase")
    stage, stable, previous = item["stage"], item["stable"], item["previous"]
    if phase == "prepared":
        if previous.exists() and not stable.exists():
            _mkdir_secure(root, stable.parent)
            os.replace(previous, stable)
    elif phase == "backed_up":
        if previous.exists() and not stable.exists():
            _mkdir_secure(root, stable.parent)
            os.replace(previous, stable)
    elif phase == "installing":
        if stage.exists() and not stable.exists():
            os.replace(stage, stable)
        if stable.exists() and previous.exists():
            _remove_tree(root, previous)
    elif phase == "installed":
        if not stable.exists():
            if stage.exists():
                os.replace(stage, stable)
            elif previous.exists():
                os.replace(previous, stable)
        if stable.exists() and previous.exists():
            _remove_tree(root, previous)
    else:
        raise HelperError("invalid_transaction")
    with contextlib.suppress(FileNotFoundError):
        os.unlink(marker)


def commit_staging(root: Path, item: dict[str, Path], provider: str) -> None:
    with attempt_lock(root, item["lock"]):
        recover_commit(root, item, provider)
        if item["cancel"].exists():
            raise HelperError("cancelled")
        _assert_tree_safe(root, item["stage"])
        _mkdir_secure(root, item["stable_parent"])
        if item["previous"].exists():
            _remove_tree(root, item["previous"])
        _write_marker(root, item["marker"], "prepared", provider)
        if item["stable"].exists() or item["stable"].is_symlink():
            _assert_tree_safe(root, item["stable"])
            os.replace(item["stable"], item["previous"])
        _write_marker(root, item["marker"], "backed_up", provider)
        _write_marker(root, item["marker"], "installing", provider)
        os.replace(item["stage"], item["stable"])
        _write_marker(root, item["marker"], "installed", provider)
        if item["previous"].exists():
            _remove_tree(root, item["previous"])
        os.unlink(item["marker"])


def _prepare_attempt(root: Path, item: dict[str, Path]) -> None:
    _mkdir_secure(root, item["base"])
    with attempt_lock(root, item["lock"]):
        recover_commit(root, item, item["stage"].name)
        if item["stage"].exists() or item["stage"].is_symlink():
            _remove_tree(root, item["stage"])
        _mkdir_secure(root, item["stage"])
        for temporary in (item["cancel"], item["claim"]):
            with contextlib.suppress(FileNotFoundError):
                os.unlink(temporary)


def _signal_event() -> threading.Event:
    event = threading.Event()

    def handle_signal(_number: int, _frame: Any) -> None:
        event.set()

    signal.signal(signal.SIGTERM, handle_signal)
    signal.signal(signal.SIGINT, handle_signal)
    return event


def login_codex(root: Path, account: str, attempt: str, expiry: int) -> int:
    item = paths(root, account, attempt, "codex")
    _prepare_attempt(root, item)
    state = base_state(account, attempt, "codex", expiry)
    _atomic_json(root, item["state"], state)
    cancelled = _signal_event()
    process: subprocess.Popen[bytes] | None = None
    rpc: JSONRPC | None = None
    login_id = ""
    try:
        process, rpc = start_codex(item["stage"])
        result = rpc.request("account/login/start", {"type": "chatgptDeviceCode"}, time.monotonic() + 30)
        if not isinstance(result, dict) or set(("type", "loginId", "userCode", "verificationUrl")) - set(result):
            raise ProtocolError()
        if result.get("type") != "chatgptDeviceCode" or not isinstance(result.get("loginId"), str):
            raise ProtocolError()
        login_id = result["loginId"]
        if not (1 <= len(login_id) <= 128):
            raise ProtocolError()
        code = result.get("userCode")
        if not isinstance(code, str) or not USER_CODE_RE.fullmatch(code):
            raise ProtocolError()
        verification_url = validate_codex_url(result.get("verificationUrl"))
        update_state(
            root,
            item["state"],
            state,
            "awaiting_user",
            verification_url=verification_url,
            user_code=code,
            poll_after_ms=2000,
        )
        deadline = time.monotonic() + max(0, expiry - int(time.time()))
        try:
            completed = rpc.wait_notification("account/login/completed", deadline, cancelled)
        except HelperError as error:
            if error.code in {"cancelled", "provider_timeout"}:
                with contextlib.suppress(HelperError):
                    response = rpc.request("account/login/cancel", {"loginId": login_id}, time.monotonic() + 5)
                    if not isinstance(response, dict) or response.get("status") not in {"canceled", "notFound"}:
                        raise ProtocolError()
                terminal = "cancelled" if cancelled.is_set() else "expired"
                update_state(root, item["state"], state, terminal, error_code="")
                return 0
            raise
        if (
            not isinstance(completed, dict)
            or completed.get("loginId") != login_id
            or not isinstance(completed.get("success"), bool)
        ):
            raise ProtocolError()
        if not completed["success"]:
            raise HelperError("provider_login_failed")
        update_state(root, item["state"], state, "verifying")
        verified = rpc.request("account/read", {"refreshToken": False}, time.monotonic() + 15)
        if not validate_account_result(verified):
            raise HelperError("provider_verification_failed")
        commit_staging(root, item, "codex")
        update_state(root, item["state"], state, "completed", error_code="")
        return 0
    except HelperError as error:
        status = "cancelled" if error.code == "cancelled" or cancelled.is_set() else "failed"
        update_state(root, item["state"], state, status, error_code="" if status == "cancelled" else error.code)
        return 0
    except Exception:
        update_state(root, item["state"], state, "failed", error_code="helper_internal_error")
        return 0
    finally:
        if rpc is not None:
            rpc.close()
        if process is not None:
            terminate_process(process)
        if state.get("state") != "completed" and item["stage"].exists():
            with contextlib.suppress(HelperError):
                _remove_tree(root, item["stage"])


def validate_claude_status(value: Any) -> bool:
    if not isinstance(value, dict) or not isinstance(value.get("loggedIn"), bool):
        raise ProtocolError()
    if value["loggedIn"]:
        if not isinstance(value.get("authMethod"), str) or not value["authMethod"]:
            raise ProtocolError()
        provider = value.get("apiProvider")
        if provider is not None and not isinstance(provider, str):
            raise ProtocolError()
    return value["loggedIn"]


def claude_status(profile: Path) -> bool:
    try:
        completed = subprocess.run(
            ["claude", "auth", "status", "--json"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=20,
            check=False,
            env=sanitized_environment("claude", profile),
        )
    except (OSError, subprocess.SubprocessError):
        raise HelperError("provider_unavailable") from None
    if len(completed.stdout) > MAX_STATE_BYTES:
        raise ProtocolError()
    try:
        value = json.loads(completed.stdout)
    except (UnicodeError, json.JSONDecodeError):
        raise ProtocolError() from None
    # Claude may use a non-zero status when reporting loggedOut; the structured
    # value, not the process status or terminal text, is authoritative.
    return validate_claude_status(value)


def claude_login_child(root: Path, account: str, attempt: str, expiry: int) -> int:
    item = paths(root, account, attempt, "claude")
    state = _read_json(root, item["state"])
    try:
        descriptor = os.open(item["claim"], os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
        os.close(descriptor)
    except FileExistsError:
        print("This login terminal is already active.")
        return 1
    try:
        if int(time.time()) >= expiry or item["cancel"].exists():
            return 1
        completed = subprocess.run(
            ["claude", "auth", "login", "--claudeai"],
            stdin=None,
            stdout=None,
            stderr=None,
            timeout=max(1, expiry - int(time.time())),
            check=False,
            env=sanitized_environment("claude", item["stage"]),
        )
        if completed.returncode != 0 or item["cancel"].exists():
            raise HelperError("provider_login_failed")
        update_state(root, item["state"], state, "verifying")
        if not claude_status(item["stage"]):
            raise HelperError("provider_verification_failed")
        if item["cancel"].exists():
            raise HelperError("cancelled")
        commit_staging(root, item, "claude")
        update_state(root, item["state"], state, "completed", error_code="")
        print("Official Claude Code login verified. You may close this terminal.")
        return 0
    except subprocess.TimeoutExpired:
        update_state(root, item["state"], state, "expired", error_code="")
        return 1
    except HelperError as error:
        status = "cancelled" if error.code == "cancelled" else "failed"
        update_state(root, item["state"], state, status, error_code="" if status == "cancelled" else error.code)
        print("Claude Code login was not verified. You may close this terminal.")
        return 1
    except Exception:
        update_state(root, item["state"], state, "failed", error_code="helper_internal_error")
        return 1


def terminal_secret() -> int:
    value = sys.stdin.buffer.readline(256)
    if len(value) > 129:
        raise HelperError("invalid_terminal_credential")
    password = value.decode("ascii", errors="strict").strip()
    if not (32 <= len(password) <= 128) or not re.fullmatch(r"[A-Za-z0-9_-]+", password):
        raise HelperError("invalid_terminal_credential")
    path = Path("/tmp/vibedock-auth-terminal-password")
    descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="ascii", closefd=True) as output:
            output.write(password + "\n")
            output.flush()
            os.fsync(output.fileno())
    except Exception:
        with contextlib.suppress(FileNotFoundError):
            path.unlink()
        raise
    return 0


def login_claude(root: Path, account: str, attempt: str, expiry: int) -> int:
    item = paths(root, account, attempt, "claude")
    _prepare_attempt(root, item)
    state = base_state(account, attempt, "claude", expiry)
    update_state(root, item["state"], state, "starting", terminal_available=False)
    secret_path = Path("/tmp/vibedock-auth-terminal-password")
    deadline = min(expiry, int(time.time()) + 30)
    while not secret_path.exists() and int(time.time()) < deadline:
        time.sleep(0.05)
    try:
        info = os.lstat(secret_path)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 129:
            raise HelperError("invalid_terminal_credential")
        password = secret_path.read_text(encoding="ascii").strip()
    except (OSError, UnicodeError, HelperError):
        update_state(root, item["state"], state, "failed", error_code="invalid_terminal_credential")
        return 0
    finally:
        with contextlib.suppress(FileNotFoundError):
            secret_path.unlink()
    if not (32 <= len(password) <= 128) or not re.fullmatch(r"[A-Za-z0-9_-]+", password):
        update_state(root, item["state"], state, "failed", error_code="invalid_terminal_credential")
        return 0
    update_state(root, item["state"], state, "awaiting_user", terminal_available=True)
    command = [
        TTYD_PATH,
        "-p", str(TTYD_PORT),
        "-W",
        "-o",
        "-c", "vibe:" + password,
        "-t", "titleFixed=VibeDock Claude Login",
        HELPER_PATH,
        "claude-login-child",
        "--account", account,
        "--attempt", attempt,
        "--expires-at", str(expiry),
    ]
    cancelled = _signal_event()
    try:
        process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=sanitized_environment("claude", item["stage"]),
            start_new_session=True,
        )
    except OSError:
        update_state(root, item["state"], state, "failed", error_code="provider_unavailable")
        return 0
    try:
        while True:
            try:
                current = _read_json(root, item["state"])
                state = current
            except HelperError:
                update_state(root, item["state"], state, "failed", error_code="invalid_state")
                return 0
            if state.get("state") in TERMINAL_STATES:
                return 0
            if cancelled.is_set():
                item["cancel"].touch(mode=0o600, exist_ok=True)
                terminate_process(process)
                update_state(root, item["state"], state, "cancelled", error_code="")
                return 0
            if int(time.time()) >= expiry:
                item["cancel"].touch(mode=0o600, exist_ok=True)
                terminate_process(process)
                update_state(root, item["state"], state, "expired", error_code="")
                return 0
            if process.poll() is not None:
                # A successful child writes a terminal state before ttyd exits.
                time.sleep(0.1)
                state = _read_json(root, item["state"])
                if state.get("state") not in TERMINAL_STATES:
                    update_state(root, item["state"], state, "failed", error_code="provider_login_failed")
                return 0
            time.sleep(0.25)
    finally:
        terminate_process(process)
        if state.get("state") != "completed" and item["stage"].exists():
            with contextlib.suppress(HelperError):
                _remove_tree(root, item["stage"])


def inspect_attempt(root: Path, account: str, attempt: str, provider: str) -> dict[str, Any]:
    item = paths(root, account, attempt, provider)
    value = _read_json(root, item["state"])
    required = {
        "protocol": int,
        "account_id": str,
        "attempt_id": str,
        "provider": str,
        "state": str,
        "expires_at": int,
        "poll_after_ms": int,
        "verification_url": str,
        "user_code": str,
        "error_code": str,
        "message": str,
        "terminal_available": bool,
    }
    if any(not isinstance(value.get(key), expected) for key, expected in required.items()):
        raise HelperError("invalid_state")
    if (
        value["protocol"] != PROTOCOL_VERSION
        or value["account_id"] != account
        or value["attempt_id"] != attempt
        or value["provider"] != provider
        or value["state"] not in GENERIC_MESSAGES
        or value["poll_after_ms"] < 1000
        or value["poll_after_ms"] > 30000
    ):
        raise HelperError("invalid_state")
    if value["state"] == "awaiting_user" and provider == "codex":
        validate_codex_url(value["verification_url"])
        if not USER_CODE_RE.fullmatch(value["user_code"]):
            raise HelperError("invalid_state")
    if value["state"] in TERMINAL_STATES and (
        value["verification_url"] or value["user_code"] or value["terminal_available"]
    ):
        raise HelperError("invalid_state")
    return value


def provider_status(root: Path, account: str, provider: str) -> dict[str, Any]:
    validate_account(account)
    validate_provider(provider)
    root = _ensure_root(root)
    stable = root / account / provider
    _assert_components(root, stable, allow_leaf_symlink=True)
    checked_at = int(time.time())
    if not stable.exists() or stable.is_symlink():
        return {"protocol": PROTOCOL_VERSION, "status": "unauthenticated", "checked_at": checked_at, "message": "No local official CLI credentials"}
    _assert_tree_safe(root, stable)
    try:
        authenticated = codex_account(stable) if provider == "codex" else claude_status(stable)
    except HelperError:
        return {"protocol": PROTOCOL_VERSION, "status": "unavailable", "checked_at": checked_at, "message": "Official CLI status unavailable"}
    return {
        "protocol": PROTOCOL_VERSION,
        "status": "authenticated" if authenticated else "unauthenticated",
        "checked_at": checked_at,
        "message": "Official CLI status checked",
    }


def provider_logout(root: Path, account: str, provider: str) -> dict[str, Any]:
    validate_account(account)
    validate_provider(provider)
    root = _ensure_root(root)
    stable = root / account / provider
    _assert_components(root, stable, allow_leaf_symlink=True)
    confirmed = False
    if stable.exists() and not stable.is_symlink():
        try:
            if provider == "codex":
                process, rpc = start_codex(stable)
                try:
                    response = rpc.request("account/logout", None, time.monotonic() + 30)
                    confirmed = isinstance(response, dict)
                finally:
                    rpc.close()
                    terminate_process(process)
            else:
                completed = subprocess.run(
                    ["claude", "auth", "logout"],
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    timeout=30,
                    check=False,
                    env=sanitized_environment("claude", stable),
                )
                confirmed = completed.returncode == 0
        except (HelperError, OSError, subprocess.SubprocessError):
            confirmed = False
    removed = _remove_tree(root, stable)
    account_path = root / account
    _assert_components(root, account_path)
    with contextlib.suppress(OSError):
        account_path.rmdir()
    return {
        "protocol": PROTOCOL_VERSION,
        "local_credentials_removed": removed or not stable.exists(),
        "provider_logout_confirmed": confirmed,
        "message": "Local credentials removed" if confirmed else "Local credentials removed; provider logout unconfirmed",
    }


def cleanup_paths(root: Path, account: str, cleanup: str) -> dict[str, Path]:
    validate_account(account)
    validate_cleanup(cleanup)
    root = _ensure_root(root)
    base = root / ".auth-cleanups" / cleanup
    result = {
        "root": root,
        "base": base,
        "marker": base / "cleanup.json",
        "quarantine": base / "credentials",
        "stable": root / account,
        "lock": root / ".auth-cleanups" / "operation.lock",
    }
    for item in result.values():
        _assert_components(root, item)
    return result


def _cleanup_marker(root: Path, item: dict[str, Path], account: str, cleanup: str, phase: str, created_at: int) -> None:
    _atomic_json(root, item["marker"], {
        "version": 1,
        "account_id": account,
        "cleanup_id": cleanup,
        "phase": phase,
        "created_at": created_at,
    })


def prepare_cleanup(root: Path, account: str, cleanup: str, now: int) -> dict[str, Any]:
    item = cleanup_paths(root, account, cleanup)
    _mkdir_secure(root, item["base"])
    with attempt_lock(root, item["lock"]):
        if item["marker"].exists() or item["quarantine"].exists():
            raise HelperError("cleanup_exists")
        _cleanup_marker(root, item, account, cleanup, "preparing", now)
        moved = False
        try:
            if item["stable"].exists() or item["stable"].is_symlink():
                _assert_tree_safe(root, item["stable"])
                os.replace(item["stable"], item["quarantine"])
                moved = True
            _cleanup_marker(root, item, account, cleanup, "prepared", now)
        except Exception:
            if moved and item["quarantine"].exists() and not item["stable"].exists():
                os.replace(item["quarantine"], item["stable"])
            with contextlib.suppress(HelperError):
                _remove_tree(root, item["base"])
            raise
    return {"protocol": PROTOCOL_VERSION, "prepared": True}


def finish_cleanup(root: Path, account: str, cleanup: str, action: str) -> dict[str, Any]:
    if action not in {"commit", "rollback"}:
        raise HelperError("invalid_cleanup_action")
    item = cleanup_paths(root, account, cleanup)
    with attempt_lock(root, item["lock"]):
        marker = _read_json(root, item["marker"])
        if (
            marker.get("version") != 1
            or marker.get("account_id") != account
            or marker.get("cleanup_id") != cleanup
            or marker.get("phase") not in {"preparing", "prepared"}
        ):
            raise HelperError("invalid_transaction")
        if action == "commit":
            if item["quarantine"].exists() or item["quarantine"].is_symlink():
                _remove_tree(root, item["quarantine"])
        else:
            if item["quarantine"].exists() or item["quarantine"].is_symlink():
                _assert_tree_safe(root, item["quarantine"])
                if item["stable"].exists() or item["stable"].is_symlink():
                    raise HelperError("cleanup_restore_conflict")
                os.replace(item["quarantine"], item["stable"])
        _remove_tree(root, item["base"])
    return {"protocol": PROTOCOL_VERSION, "completed": True}


def _reconcile_cleanups(root: Path, now: int) -> int:
    cleanups_root = root / ".auth-cleanups"
    _assert_components(root, cleanups_root, allow_leaf_symlink=True)
    if not cleanups_root.exists():
        return 0
    if cleanups_root.is_symlink() or not cleanups_root.is_dir():
        raise HelperError("unsafe_credentials_path")
    recovered = 0
    with attempt_lock(root, cleanups_root / "operation.lock"):
        for cleanup_path in list(cleanups_root.iterdir()):
            if cleanup_path.name == "operation.lock":
                continue
            if not CLEANUP_RE.fullmatch(cleanup_path.name):
                raise HelperError("unsafe_credentials_path")
            _assert_components(root, cleanup_path)
            marker_path = cleanup_path / "cleanup.json"
            try:
                marker = _read_json(root, marker_path)
                account = validate_account(marker.get("account_id", ""))
                cleanup = validate_cleanup(marker.get("cleanup_id", ""))
                created_at = marker.get("created_at")
                if marker.get("version") != 1 or cleanup != cleanup_path.name or not isinstance(created_at, int):
                    raise HelperError("invalid_transaction")
                item = cleanup_paths(root, account, cleanup)
                if marker.get("phase") not in {"preparing", "prepared"}:
                    raise HelperError("invalid_transaction")
                # Prepare transactions are rolled back on startup. This never
                # silently commits a Store mutation whose result is unknown.
                if item["quarantine"].exists():
                    _assert_tree_safe(root, item["quarantine"])
                    if item["stable"].exists() or item["stable"].is_symlink():
                        # Preserve both trees for explicit operator recovery.
                        continue
                    os.replace(item["quarantine"], item["stable"])
                _remove_tree(root, item["base"])
                recovered += 1
            except HelperError:
                continue
    with contextlib.suppress(OSError):
        cleanups_root.rmdir()
    return recovered


def reconcile(root: Path, now: int) -> dict[str, Any]:
    root = _ensure_root(root)
    attempts_root = root / ".auth-attempts"
    _assert_components(root, attempts_root, allow_leaf_symlink=True)
    removed = 0
    if not attempts_root.exists():
        recovered = _reconcile_cleanups(root, now)
        return {"protocol": PROTOCOL_VERSION, "removed": 0, "recovered": recovered}
    if attempts_root.is_symlink() or not attempts_root.is_dir():
        raise HelperError("unsafe_credentials_path")
    for account_path in list(attempts_root.iterdir()):
        if not ACCOUNT_RE.fullmatch(account_path.name):
            raise HelperError("unsafe_credentials_path")
        _assert_components(root, account_path)
        if not account_path.is_dir():
            raise HelperError("unsafe_credentials_path")
        for attempt_path in list(account_path.iterdir()):
            if not ATTEMPT_RE.fullmatch(attempt_path.name):
                raise HelperError("unsafe_credentials_path")
            _assert_components(root, attempt_path)
            if not attempt_path.is_dir():
                raise HelperError("unsafe_credentials_path")
            state_path = attempt_path / "state.json"
            try:
                state = _read_json(root, state_path)
                provider = validate_provider(state.get("provider", ""))
                item = paths(root, account_path.name, attempt_path.name, provider)
                with attempt_lock(root, item["lock"]):
                    recover_commit(root, item, provider)
                expires_at = state.get("expires_at")
                terminal = state.get("state") in TERMINAL_STATES
                stale = isinstance(expires_at, int) and expires_at <= now
            except HelperError:
                # Unknown trees are never recursively deleted by reconciliation.
                continue
            if terminal or stale:
                _remove_tree(root, attempt_path)
                removed += 1
        with contextlib.suppress(OSError):
            account_path.rmdir()
    with contextlib.suppress(OSError):
        attempts_root.rmdir()
    recovered = _reconcile_cleanups(root, now)
    return {"protocol": PROTOCOL_VERSION, "removed": removed, "recovered": recovered}


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(add_help=False, allow_abbrev=False)
    subparsers = parser.add_subparsers(dest="operation", required=True)
    subparsers.add_parser("protocol", add_help=False)
    subparsers.add_parser("terminal-secret", add_help=False)
    for operation in ("login", "inspect"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
        child.add_argument("--account", required=True)
        child.add_argument("--attempt", required=True)
        if operation == "login":
            child.add_argument("--expires-at", required=True, type=int)
    child = subparsers.add_parser("claude-login-child", add_help=False, allow_abbrev=False)
    child.add_argument("--account", required=True)
    child.add_argument("--attempt", required=True)
    child.add_argument("--expires-at", required=True, type=int)
    for operation in ("status", "logout"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
        child.add_argument("--account", required=True)
    for operation in ("cleanup-prepare", "cleanup-commit", "cleanup-rollback"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--account", required=True)
        child.add_argument("--cleanup", required=True)
        if operation == "cleanup-prepare":
            child.add_argument("--now", required=True, type=int)
    child = subparsers.add_parser("reconcile", add_help=False, allow_abbrev=False)
    child.add_argument("--now", required=True, type=int)
    return parser


def main(argv: list[str] | None = None) -> int:
    try:
        args = _parser().parse_args(argv)
        root = CREDENTIALS_ROOT
        if args.operation == "protocol":
            result = compatibility_manifest(True)
        elif args.operation == "terminal-secret":
            return terminal_secret()
        elif args.operation == "login":
            compatibility_manifest(True)
            account = validate_account(args.account)
            attempt = validate_attempt(args.attempt)
            expiry = validate_expiry(args.expires_at)
            if args.provider == "codex":
                return login_codex(root, account, attempt, expiry)
            return login_claude(root, account, attempt, expiry)
        elif args.operation == "claude-login-child":
            account = validate_account(args.account)
            attempt = validate_attempt(args.attempt)
            expiry = validate_expiry(args.expires_at)
            return claude_login_child(root, account, attempt, expiry)
        elif args.operation == "inspect":
            result = inspect_attempt(root, args.account, args.attempt, args.provider)
        elif args.operation == "status":
            compatibility_manifest(True)
            result = provider_status(root, args.account, args.provider)
        elif args.operation == "logout":
            compatibility_manifest(True)
            result = provider_logout(root, args.account, args.provider)
        elif args.operation == "cleanup-prepare":
            result = prepare_cleanup(root, args.account, args.cleanup, args.now)
        elif args.operation == "cleanup-commit":
            result = finish_cleanup(root, args.account, args.cleanup, "commit")
        elif args.operation == "cleanup-rollback":
            result = finish_cleanup(root, args.account, args.cleanup, "rollback")
        elif args.operation == "reconcile":
            result = reconcile(root, args.now)
        else:
            raise HelperError("unsupported_operation")
        sys.stdout.write(json.dumps(result, separators=(",", ":"), sort_keys=True) + "\n")
        return 0
    except SystemExit:
        raise
    except HelperError as error:
        sys.stderr.write("auth-helper error: " + error.code + "\n")
        return 2
    except Exception:
        sys.stderr.write("auth-helper error: helper_internal_error\n")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
