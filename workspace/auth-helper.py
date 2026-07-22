#!/usr/bin/env python3
"""Fixed-command authentication helper for VibeDock.

The public control plane selects one of the fixed operations below. Provider
output is never copied to this process' stderr or to the Docker log stream.
"""

from __future__ import annotations

import argparse
import base64
import contextlib
import datetime as dt
import fcntl
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import time
from typing import Any, Iterator
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

try:
    import tomllib
except ModuleNotFoundError:  # pragma: no cover - the pinned image has tomllib
    tomllib = None

PROTOCOL_VERSION = 1
CODEX_VERSION = "0.144.6"
CLAUDE_VERSION = "2.1.215"
TTYD_VERSION = "1.7.7"
CREDENTIALS_ROOT = Path("/vibedock/credentials")
MAX_STATE_BYTES = 256 << 10
ACCOUNT_RE = re.compile(r"^[a-z0-9]{12}$")
ATTEMPT_RE = re.compile(r"^[a-z0-9]{24}$")
CLEANUP_RE = re.compile(r"^[a-z0-9]{32}$")
PROVIDERS = {"codex", "claude"}
CONFIG_FILES = {"codex": ("config.toml",), "claude": ("settings.json",)}
DEFAULT_CONFIG_CONTENT = {
    "codex": "# Optional Codex settings. Add overrides below, then save.\n",
    "claude": "{}\n",
}
MAX_CONFIG_BYTES = 256 << 10
MAX_AUTH_RESPONSE_BYTES = 8192
TERMINAL_STATES = {"completed", "cancelled", "expired", "failed"}
VERIFYING_TTL_SECONDS = 75
TERMINAL_RETENTION_SECONDS = 10 * 60
CODEX_OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
CODEX_OAUTH_AUTHORIZE_URL = "https://auth.openai.com/oauth/authorize"
CODEX_OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
CODEX_OAUTH_REDIRECT_URI = "http://localhost:1455/auth/callback"
CODEX_OAUTH_SCOPES = "openid profile email offline_access api.connectors.read api.connectors.invoke"
CLAUDE_OAUTH_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
CLAUDE_OAUTH_AUTHORIZE_URL = "https://claude.com/cai/oauth/authorize"
CLAUDE_OAUTH_TOKEN_URL = "https://platform.claude.com/v1/oauth/token"
CLAUDE_OAUTH_REDIRECT_URI = "https://platform.claude.com/oauth/code/callback"
CLAUDE_OAUTH_SCOPES = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
AUTHORIZATION_URLS = {
    "codex": ("auth.openai.com", "/oauth/authorize"),
    "claude": ("claude.com", "/cai/oauth/authorize"),
}


class _RejectRedirects(HTTPRedirectHandler):
    def redirect_request(self, _request, _file, _code, _message, _headers, _new_url):
        return None


TOKEN_OPENER = build_opener(_RejectRedirects)
GENERIC_MESSAGES = {
    "starting": "Preparing official authorization",
    "awaiting_user": "Waiting for the official authorization response",
    "verifying": "Exchanging the authorization response",
    "completed": "Official authorization completed",
    "cancelled": "Login cancelled",
    "expired": "Login expired",
    "failed": "Official authorization failed",
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


def _assert_directory_entry(root: Path, path: Path) -> None:
    """Validate the directory itself without following provider-owned children."""
    _assert_components(root, path, allow_leaf_symlink=True)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise HelperError("credentials_missing") from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise HelperError("unsafe_credentials_path")


def _remove_entry_at(parent: int, name: str) -> None:
    """Remove an entry without following symlinks, anchored to its parent fd."""
    info = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if stat.S_ISDIR(info.st_mode):
        flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
        child = os.open(name, flags, dir_fd=parent)
        try:
            for child_name in os.listdir(child):
                _remove_entry_at(child, child_name)
        finally:
            os.close(child)
        os.rmdir(name, dir_fd=parent)
    else:
        os.unlink(name, dir_fd=parent)


def _remove_tree(root: Path, path: Path) -> bool:
    """Remove a tree safely, unlinking provider-created symlinks in place."""
    _assert_components(root, path, allow_leaf_symlink=True)
    relative = path.relative_to(root)
    if not relative.parts:
        raise HelperError("unsafe_credentials_path")
    root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    parent_fd = root_fd
    opened: list[int] = []
    try:
        for component in relative.parts[:-1]:
            child_fd = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
            opened.append(child_fd)
            parent_fd = child_fd
        leaf = relative.parts[-1]
        try:
            os.stat(leaf, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError:
            return False
        _remove_entry_at(parent_fd, leaf)
        return True
    finally:
        for descriptor in reversed(opened):
            os.close(descriptor)
        os.close(root_fd)


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


def _config_path(root: Path, account: str, provider: str, name: str) -> Path:
    validate_account(account)
    validate_provider(provider)
    if name not in CONFIG_FILES[provider]:
        raise HelperError("config_path_not_allowed")
    root = _ensure_root(root)
    base = root / account / provider
    _assert_components(root, base)
    return base / name


def _config_content_is_safe(provider: str, content: str) -> None:
    if len(content.encode("utf-8")) > MAX_CONFIG_BYTES:
        raise HelperError("config_too_large")
    try:
        if provider == "codex":
            if tomllib is None:
                raise HelperError("config_parser_unavailable")
            value = tomllib.loads(content)
        else:
            value = json.loads(content)
    except (UnicodeError, ValueError, json.JSONDecodeError, tomllib.TOMLDecodeError if tomllib else ValueError):
        raise HelperError("config_invalid") from None
    if not isinstance(value, dict):
        raise HelperError("config_invalid")

    def walk(item: Any) -> None:
        if isinstance(item, dict):
            for key, child in item.items():
                lowered = str(key).lower()
                if any(word in lowered for word in ("apikey", "api_key", "token", "password", "secret", "oauth")):
                    raise HelperError("config_protected")
                walk(child)
        elif isinstance(item, list):
            for child in item:
                walk(child)

    walk(value)


def _read_config(root: Path, account: str, provider: str, name: str) -> dict[str, Any]:
    path = _config_path(root, account, provider, name)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return {
            "protocol": PROTOCOL_VERSION,
            "path": name,
            "exists": False,
            "content": "",
            "default_content": DEFAULT_CONFIG_CONTENT[provider],
        }
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_size > MAX_CONFIG_BYTES:
        raise HelperError("config_invalid")
    try:
        content = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        raise HelperError("config_invalid") from None
    _config_content_is_safe(provider, content)
    return {"protocol": PROTOCOL_VERSION, "path": name, "exists": True, "content": content}


def _write_config(root: Path, account: str, provider: str, name: str, content: str) -> dict[str, Any]:
    path = _config_path(root, account, provider, name)
    _config_content_is_safe(provider, content)
    _mkdir_secure(root, path.parent)
    descriptor, temporary = tempfile.mkstemp(prefix=".config-", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8", closefd=True) as output:
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)
    return _read_config(root, account, provider, name)


def list_config(root: Path, account: str, provider: str) -> dict[str, Any]:
    files = []
    for name in CONFIG_FILES[provider]:
        path = _config_path(root, account, provider, name)
        exists = False
        size = 0
        try:
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise HelperError("config_invalid")
            exists, size = True, info.st_size
        except FileNotFoundError:
            pass
        files.append({"path": name, "exists": exists, "editable": True, "size": size})
    return {"protocol": PROTOCOL_VERSION, "files": files}


def reset_config(root: Path, account: str, provider: str, name: str) -> dict[str, Any]:
    path = _config_path(root, account, provider, name)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return {"protocol": PROTOCOL_VERSION, "removed": False}
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise HelperError("config_invalid")
    os.unlink(path)
    return {"protocol": PROTOCOL_VERSION, "removed": True}


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
        "oauth": base / "oauth.json",
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
    if status != "awaiting_user":
        state["verification_url"] = ""
        state["user_code"] = ""
    if status in TERMINAL_STATES:
        state["terminal_available"] = False
    _atomic_json(root, path, state)


def compatibility_manifest(check_commands: bool = True) -> dict[str, Any]:
    # OAuth is implemented by this helper. The provider CLIs are consumers of
    # the generated credentials and are deliberately not invoked here.
    del check_commands
    return {
        "protocol": PROTOCOL_VERSION,
        "codex_version": CODEX_VERSION,
        "claude_version": CLAUDE_VERSION,
        "ttyd_version": TTYD_VERSION,
    }


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


def _base64url(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).decode("ascii").rstrip("=")


def _pkce_values(provider: str) -> tuple[str, str, str]:
    verifier_bytes = 64 if provider == "codex" else 32
    verifier = _base64url(os.urandom(verifier_bytes))
    challenge = _base64url(hashlib.sha256(verifier.encode("ascii")).digest())
    state = _base64url(os.urandom(32))
    return state, verifier, challenge


def build_authorization_url(provider: str, state: str, challenge: str) -> str:
    validate_provider(provider)
    if provider == "codex":
        values = {
            "response_type": "code",
            "client_id": CODEX_OAUTH_CLIENT_ID,
            "redirect_uri": CODEX_OAUTH_REDIRECT_URI,
            "scope": CODEX_OAUTH_SCOPES,
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
            "id_token_add_organizations": "true",
            "codex_cli_simplified_flow": "true",
            "originator": "codex_cli_rs",
        }
        return CODEX_OAUTH_AUTHORIZE_URL + "?" + urlencode(values)
    values = {
        "code": "true",
        "client_id": CLAUDE_OAUTH_CLIENT_ID,
        "response_type": "code",
        "redirect_uri": CLAUDE_OAUTH_REDIRECT_URI,
        "scope": CLAUDE_OAUTH_SCOPES,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
        "state": state,
    }
    return CLAUDE_OAUTH_AUTHORIZE_URL + "?" + urlencode(values)


def validate_authorization_url(value: str, provider: str) -> str:
    if not isinstance(value, str) or len(value) < 32 or len(value) > 4096:
        raise ProtocolError()
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        raise ProtocolError() from None
    expected_host, expected_path = AUTHORIZATION_URLS[provider]
    if (
        parsed.scheme != "https"
        or parsed.username is not None
        or parsed.password is not None
        or port not in (None, 443)
        or parsed.fragment
        or (parsed.hostname or "").lower() != expected_host
        or parsed.path != expected_path
    ):
        raise ProtocolError()
    query = parse_qs(parsed.query, keep_blank_values=True)
    expected_client = CODEX_OAUTH_CLIENT_ID if provider == "codex" else CLAUDE_OAUTH_CLIENT_ID
    expected_redirect = CODEX_OAUTH_REDIRECT_URI if provider == "codex" else CLAUDE_OAUTH_REDIRECT_URI
    required = {
        "client_id": expected_client,
        "redirect_uri": expected_redirect,
        "response_type": "code",
        "code_challenge_method": "S256",
    }
    if any(query.get(name) != [expected] for name, expected in required.items()):
        raise ProtocolError()
    if len(query.get("state", [])) != 1 or len(query.get("code_challenge", [])) != 1:
        raise ProtocolError()
    return value


def install_staged_credentials(root: Path, item: dict[str, Path], provider: str) -> None:
    """Atomically replace only the provider credential, preserving profile data."""
    name = "auth.json" if provider == "codex" else ".credentials.json"
    source = item["stage"] / name
    _assert_tree_safe(root, item["stage"])
    if item["stable"].exists() or item["stable"].is_symlink():
        _assert_directory_entry(root, item["stable"])
    else:
        _mkdir_secure(root, item["stable"])
    target = item["stable"] / name
    _assert_components(root, target)
    os.replace(source, target)
    directory = os.open(item["stable"], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
    _remove_tree(root, item["stage"])


def start_oauth(root: Path, account: str, attempt: str, provider: str, expiry: int) -> dict[str, Any]:
    item = paths(root, account, attempt, provider)
    _prepare_attempt(root, item)
    state_value = base_state(account, attempt, provider, expiry)
    state, verifier, challenge = _pkce_values(provider)
    authorization_url = build_authorization_url(provider, state, challenge)
    validate_authorization_url(authorization_url, provider)
    redirect_uri = CODEX_OAUTH_REDIRECT_URI if provider == "codex" else CLAUDE_OAUTH_REDIRECT_URI
    _atomic_json(root, item["oauth"], {
        "version": 1,
        "provider": provider,
        "state": state,
        "code_verifier": verifier,
        "redirect_uri": redirect_uri,
        "expires_at": expiry,
    })
    update_state(
        root,
        item["state"],
        state_value,
        "awaiting_user",
        verification_url=authorization_url,
        user_code="",
        poll_after_ms=2000,
    )
    return state_value


def _valid_oauth_value(value: Any, name: str, required: bool = True) -> str:
    if value is None and not required:
        return ""
    if not isinstance(value, str) or len(value) < 8 or len(value) > 65536 or any(ord(character) < 0x20 for character in value):
        raise HelperError("provider_token_invalid", name)
    return value


def parse_authorization_response(provider: str, response: str, expected_state: str) -> str:
    value = response.strip()
    if not value or len(value.encode("utf-8")) > MAX_AUTH_RESPONSE_BYTES or "\x00" in value:
        raise HelperError("authorization_response_invalid")
    code = ""
    returned_state = ""
    if "://" in value:
        try:
            parsed = urlsplit(value)
            port = parsed.port
        except ValueError:
            raise HelperError("authorization_response_invalid") from None
        if provider == "codex":
            valid_redirect = (
                parsed.scheme == "http"
                and parsed.hostname == "localhost"
                and port == 1455
                and parsed.path == "/auth/callback"
            )
        else:
            valid_redirect = (
                parsed.scheme == "https"
                and parsed.hostname == "platform.claude.com"
                and port in (None, 443)
                and parsed.path == "/oauth/code/callback"
            )
        if (
            not valid_redirect
            or parsed.username is not None
            or parsed.password is not None
            or parsed.fragment
            or any(len(values) != 1 for values in parse_qs(parsed.query, keep_blank_values=True).values())
        ):
            raise HelperError("authorization_response_invalid")
        query = parse_qs(parsed.query, keep_blank_values=True)
        if query.get("error"):
            raise HelperError("provider_authorization_rejected")
        code = query.get("code", [""])[0]
        returned_state = query.get("state", [""])[0]
    elif provider == "claude" and value.count("#") == 1:
        code, returned_state = value.split("#", 1)
    else:
        raise HelperError("authorization_response_invalid")
    code = _valid_oauth_value(code, "code")
    returned_state = _valid_oauth_value(returned_state, "state")
    if not hmac.compare_digest(returned_state, expected_state):
        raise HelperError("authorization_state_mismatch")
    return code


def _request_token(provider: str, code: str, verifier: str, redirect_uri: str, state: str) -> dict[str, Any]:
    if provider == "codex":
        body = urlencode({
            "grant_type": "authorization_code",
            "client_id": CODEX_OAUTH_CLIENT_ID,
            "code": code,
            "redirect_uri": redirect_uri,
            "code_verifier": verifier,
        }).encode("ascii")
        request = Request(CODEX_OAUTH_TOKEN_URL, data=body, headers={
            "Accept": "application/json",
            "Content-Type": "application/x-www-form-urlencoded",
            "User-Agent": "codex-cli/" + CODEX_VERSION,
        }, method="POST")
    else:
        body = json.dumps({
            "grant_type": "authorization_code",
            "client_id": CLAUDE_OAUTH_CLIENT_ID,
            "code": code,
            "redirect_uri": redirect_uri,
            "code_verifier": verifier,
            "state": state,
        }, separators=(",", ":")).encode("utf-8")
        request = Request(CLAUDE_OAUTH_TOKEN_URL, data=body, headers={
            "Accept": "application/json",
            "Content-Type": "application/json",
            "User-Agent": "claude-code/" + CLAUDE_VERSION,
        }, method="POST")
    try:
        with TOKEN_OPENER.open(request, timeout=60) as response:
            encoded = response.read(MAX_STATE_BYTES + 1)
    except HTTPError as error:
        error.close()
        raise HelperError("provider_exchange_rejected") from None
    except (URLError, TimeoutError, OSError):
        raise HelperError("provider_unavailable") from None
    if len(encoded) > MAX_STATE_BYTES:
        raise HelperError("provider_response_too_large")
    try:
        value = json.loads(encoded.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError):
        raise HelperError("provider_response_invalid") from None
    if not isinstance(value, dict):
        raise HelperError("provider_response_invalid")
    return value


def _jwt_claims(token: str) -> dict[str, Any]:
    parts = token.split(".")
    if len(parts) != 3:
        return {}
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        value = json.loads(base64.urlsafe_b64decode(payload).decode("utf-8"))
    except (ValueError, UnicodeError, json.JSONDecodeError):
        return {}
    return value if isinstance(value, dict) else {}


def _codex_account_id(id_token: str, access_token: str) -> str | None:
    claim_name = "https://api.openai.com/auth"
    for token in (id_token, access_token):
        claims = _jwt_claims(token)
        auth = claims.get(claim_name)
        if isinstance(auth, dict):
            account_id = auth.get("chatgpt_account_id")
            if isinstance(account_id, str) and 1 <= len(account_id) <= 256:
                return account_id
    return None


def _write_provider_credentials(root: Path, item: dict[str, Path], provider: str, token: dict[str, Any]) -> None:
    access_token = _valid_oauth_value(token.get("access_token"), "access_token")
    refresh_token = _valid_oauth_value(token.get("refresh_token"), "refresh_token")
    if provider == "codex":
        id_token = _valid_oauth_value(token.get("id_token"), "id_token")
        credentials = {
            "auth_mode": "chatgpt",
            "OPENAI_API_KEY": None,
            "tokens": {
                "id_token": id_token,
                "access_token": access_token,
                "refresh_token": refresh_token,
                "account_id": _codex_account_id(id_token, access_token),
            },
            "last_refresh": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z"),
        }
        _atomic_json(root, item["stage"] / "auth.json", credentials)
        return
    expires_in = token.get("expires_in")
    if not isinstance(expires_in, (int, float)) or isinstance(expires_in, bool) or expires_in <= 0 or expires_in > 366 * 24 * 3600:
        raise HelperError("provider_token_invalid")
    raw_scopes = token.get("scope")
    if isinstance(raw_scopes, str):
        scopes = [scope for scope in raw_scopes.split() if scope]
    elif isinstance(raw_scopes, list) and all(isinstance(scope, str) and scope for scope in raw_scopes):
        scopes = raw_scopes
    else:
        scopes = CLAUDE_OAUTH_SCOPES.split()
    if not scopes:
        raise HelperError("provider_token_invalid")
    if "user:inference" not in scopes:
        raise HelperError("provider_token_invalid")
    credentials = {"claudeAiOauth": {
        "accessToken": access_token,
        "refreshToken": refresh_token,
        "expiresAt": int(time.time() * 1000 + float(expires_in) * 1000),
        "scopes": scopes,
    }}
    _atomic_json(root, item["stage"] / ".credentials.json", credentials)


def complete_oauth(root: Path, account: str, attempt: str, provider: str, response: str) -> dict[str, Any]:
    item = paths(root, account, attempt, provider)
    state_value = inspect_attempt(root, account, attempt, provider)
    if state_value["state"] in TERMINAL_STATES:
        return state_value
    if int(time.time()) >= state_value["expires_at"]:
        update_state(root, item["state"], state_value, "expired", error_code="")
        return state_value
    try:
        oauth = _read_json(root, item["oauth"])
        if (
            oauth.get("version") != 1
            or oauth.get("provider") != provider
            or oauth.get("expires_at") != state_value["expires_at"]
            or not isinstance(oauth.get("state"), str)
            or not isinstance(oauth.get("code_verifier"), str)
            or not isinstance(oauth.get("redirect_uri"), str)
        ):
            raise HelperError("oauth_session_invalid")
        code = parse_authorization_response(provider, response, oauth["state"])
        oauth["verification_deadline"] = int(time.time()) + VERIFYING_TTL_SECONDS
        _atomic_json(root, item["oauth"], oauth)
        update_state(root, item["state"], state_value, "verifying")
        token = _request_token(provider, code, oauth["code_verifier"], oauth["redirect_uri"], oauth["state"])
        _write_provider_credentials(root, item, provider, token)
        install_staged_credentials(root, item, provider)
        update_state(root, item["state"], state_value, "completed", error_code="")
        with contextlib.suppress(FileNotFoundError):
            os.unlink(item["oauth"])
    except HelperError as error:
        update_state(root, item["state"], state_value, "failed", error_code=error.code)
        if item["stage"].exists() or item["stage"].is_symlink():
            with contextlib.suppress(HelperError):
                _remove_tree(root, item["stage"])
        with contextlib.suppress(FileNotFoundError):
            os.unlink(item["oauth"])
    return state_value


def cancel_oauth(root: Path, account: str, attempt: str, provider: str) -> dict[str, Any]:
    item = paths(root, account, attempt, provider)
    state_value = inspect_attempt(root, account, attempt, provider)
    if state_value["state"] == "completed":
        return state_value
    if state_value["state"] not in TERMINAL_STATES:
        update_state(root, item["state"], state_value, "cancelled", error_code="")
    for path in (item["oauth"], item["cancel"], item["claim"]):
        with contextlib.suppress(FileNotFoundError):
            os.unlink(path)
    if item["stage"].exists() or item["stage"].is_symlink():
        _remove_tree(root, item["stage"])
    return state_value


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
    if value["state"] == "awaiting_user":
        validate_authorization_url(value["verification_url"], provider)
        if value["user_code"] or value["terminal_available"]:
            raise HelperError("invalid_state")
        if int(time.time()) >= value["expires_at"]:
            update_state(root, item["state"], value, "expired", error_code="")
            with contextlib.suppress(FileNotFoundError):
                os.unlink(item["oauth"])
            if item["stage"].exists() or item["stage"].is_symlink():
                _remove_tree(root, item["stage"])
    elif value["state"] == "verifying":
        try:
            oauth = _read_json(root, item["oauth"])
            verification_deadline = oauth.get("verification_deadline")
            if not isinstance(verification_deadline, int):
                raise HelperError("oauth_session_invalid")
        except HelperError as error:
            update_state(root, item["state"], value, "failed", error_code=error.code)
            with contextlib.suppress(FileNotFoundError):
                os.unlink(item["oauth"])
            if item["stage"].exists() or item["stage"].is_symlink():
                _remove_tree(root, item["stage"])
        else:
            if int(time.time()) >= verification_deadline:
                update_state(root, item["state"], value, "expired", error_code="")
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(item["oauth"])
                if item["stage"].exists() or item["stage"].is_symlink():
                    _remove_tree(root, item["stage"])
    elif (
        value["verification_url"] or value["user_code"] or value["terminal_available"]
    ):
        raise HelperError("invalid_state")
    return value


def _read_provider_credentials(root: Path, path: Path) -> dict[str, Any]:
    _assert_components(root, path)
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        raise HelperError("credentials_missing") from None
    except OSError:
        raise HelperError("credentials_invalid") from None
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_size <= 0 or info.st_size > MAX_STATE_BYTES:
            raise HelperError("credentials_invalid")
        chunks = []
        remaining = MAX_STATE_BYTES + 1
        while remaining:
            chunk = os.read(descriptor, remaining)
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        encoded = b"".join(chunks)
    finally:
        os.close(descriptor)
    if len(encoded) > MAX_STATE_BYTES:
        raise HelperError("credentials_invalid")
    try:
        value = json.loads(encoded.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError):
        raise HelperError("credentials_invalid") from None
    if not isinstance(value, dict):
        raise HelperError("credentials_invalid")
    return value


def _credentials_are_valid(provider: str, value: dict[str, Any]) -> bool:
    if provider == "codex":
        tokens = value.get("tokens")
        if value.get("auth_mode") != "chatgpt" or not isinstance(tokens, dict):
            return False
        required_tokens_valid = all(
            isinstance(tokens.get(name), str) and 1 <= len(tokens[name]) <= (128 << 10)
            for name in ("id_token", "access_token", "refresh_token")
        )
        account_id = tokens.get("account_id")
        return required_tokens_valid and (
            account_id is None
            or (isinstance(account_id, str) and 1 <= len(account_id) <= 256)
        )
    oauth = value.get("claudeAiOauth")
    if not isinstance(oauth, dict):
        return False
    scopes = oauth.get("scopes")
    return (
        all(isinstance(oauth.get(name), str) and 1 <= len(oauth[name]) <= (128 << 10) for name in ("accessToken", "refreshToken"))
        and isinstance(oauth.get("expiresAt"), (int, float))
        and not isinstance(oauth.get("expiresAt"), bool)
        and isinstance(scopes, list)
        and bool(scopes)
        and all(isinstance(scope, str) and bool(scope) for scope in scopes)
    )


def provider_status(root: Path, account: str, provider: str) -> dict[str, Any]:
    validate_account(account)
    validate_provider(provider)
    root = _ensure_root(root)
    stable = root / account / provider
    _assert_components(root, stable, allow_leaf_symlink=True)
    checked_at = int(time.time())
    if not stable.exists() or stable.is_symlink():
        return {"protocol": PROTOCOL_VERSION, "status": "unauthenticated", "checked_at": checked_at, "message": "No managed credentials"}
    credential_path = stable / ("auth.json" if provider == "codex" else ".credentials.json")
    try:
        authenticated = _credentials_are_valid(provider, _read_provider_credentials(root, credential_path))
    except HelperError as error:
        if error.code == "credentials_missing":
            return {"protocol": PROTOCOL_VERSION, "status": "unauthenticated", "checked_at": checked_at, "message": "No managed credentials"}
        return {"protocol": PROTOCOL_VERSION, "status": "unavailable", "checked_at": checked_at, "message": "Managed credentials are unavailable"}
    return {
        "protocol": PROTOCOL_VERSION,
        "status": "authenticated" if authenticated else "unauthenticated",
        "checked_at": checked_at,
        "message": "Managed credentials checked",
    }


def provider_logout(root: Path, account: str, provider: str) -> dict[str, Any]:
    validate_account(account)
    validate_provider(provider)
    root = _ensure_root(root)
    stable = root / account / provider
    _assert_components(root, stable, allow_leaf_symlink=True)
    removed = False
    if stable.exists() and not stable.is_symlink():
        _assert_directory_entry(root, stable)
        credential_path = stable / ("auth.json" if provider == "codex" else ".credentials.json")
        _assert_components(root, credential_path, allow_leaf_symlink=True)
        try:
            info = os.lstat(credential_path)
        except FileNotFoundError:
            pass
        else:
            if stat.S_ISDIR(info.st_mode):
                raise HelperError("credentials_invalid")
            os.unlink(credential_path)
            removed = True
    elif stable.is_symlink():
        raise HelperError("unsafe_credentials_path")
    return {
        "protocol": PROTOCOL_VERSION,
        "local_credentials_removed": removed or not (stable / ("auth.json" if provider == "codex" else ".credentials.json")).exists(),
        "provider_logout_confirmed": False,
        "message": "Local credentials removed; provider token revocation was not requested",
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
                _assert_directory_entry(root, item["stable"])
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
                _assert_directory_entry(root, item["quarantine"])
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
                    _assert_directory_entry(root, item["quarantine"])
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
                state = inspect_attempt(root, account_path.name, attempt_path.name, provider)
                terminal = state.get("state") in TERMINAL_STATES
                state_mtime = int(os.lstat(state_path).st_mtime)
                stale = terminal and state_mtime + TERMINAL_RETENTION_SECONDS <= now
            except HelperError:
                # Unknown trees are never recursively deleted by reconciliation.
                continue
            if stale:
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
    for operation in ("login", "complete", "cancel", "inspect"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
        child.add_argument("--account", required=True)
        child.add_argument("--attempt", required=True)
        if operation == "login":
            child.add_argument("--expires-at", required=True, type=int)
    for operation in ("status", "logout"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
        child.add_argument("--account", required=True)
    child = subparsers.add_parser("config-list", add_help=False, allow_abbrev=False)
    child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
    child.add_argument("--account", required=True)
    for operation in ("config-read", "config-write", "config-reset"):
        child = subparsers.add_parser(operation, add_help=False, allow_abbrev=False)
        child.add_argument("--provider", required=True, choices=sorted(PROVIDERS))
        child.add_argument("--account", required=True)
        child.add_argument("--path", required=True)
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
            result = compatibility_manifest(False)
        elif args.operation == "login":
            account = validate_account(args.account)
            attempt = validate_attempt(args.attempt)
            expiry = validate_expiry(args.expires_at)
            result = start_oauth(root, account, attempt, args.provider, expiry)
        elif args.operation == "complete":
            encoded = sys.stdin.buffer.read(MAX_AUTH_RESPONSE_BYTES + 1)
            if len(encoded) > MAX_AUTH_RESPONSE_BYTES:
                raise HelperError("authorization_response_invalid")
            try:
                response = encoded.decode("utf-8")
            except UnicodeDecodeError:
                raise HelperError("authorization_response_invalid") from None
            result = complete_oauth(root, args.account, args.attempt, args.provider, response)
        elif args.operation == "cancel":
            result = cancel_oauth(root, args.account, args.attempt, args.provider)
        elif args.operation == "inspect":
            result = inspect_attempt(root, args.account, args.attempt, args.provider)
        elif args.operation == "status":
            result = provider_status(root, args.account, args.provider)
        elif args.operation == "logout":
            result = provider_logout(root, args.account, args.provider)
        elif args.operation == "config-list":
            result = list_config(root, args.account, args.provider)
        elif args.operation == "config-read":
            result = _read_config(root, args.account, args.provider, args.path)
        elif args.operation == "config-write":
            content_bytes = sys.stdin.buffer.read(MAX_CONFIG_BYTES + 1)
            if len(content_bytes) > MAX_CONFIG_BYTES:
                raise HelperError("config_too_large")
            try:
                content = content_bytes.decode("utf-8")
            except UnicodeDecodeError:
                raise HelperError("config_invalid") from None
            result = _write_config(root, args.account, args.provider, args.path, content)
        elif args.operation == "config-reset":
            result = reset_config(root, args.account, args.provider, args.path)
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
