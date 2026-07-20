import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest import mock


MODULE_PATH = Path(__file__).with_name("auth-helper.py")
spec = importlib.util.spec_from_file_location("vibedock_auth_helper", MODULE_PATH)
helper = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(helper)

ACCOUNT = "account12345"
ATTEMPT = "attempt12345678901234567"
CLEANUP = "cleanup1234567890123456789012345"


class RootCase(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()

    def tearDown(self):
        self.temporary.cleanup()


class ValidationTest(RootCase):
    def test_manifest_is_pinned(self):
        self.assertEqual(
            helper.compatibility_manifest(False),
            {
                "protocol": 1,
                "codex_version": "0.144.6",
                "claude_version": "2.1.215",
                "ttyd_version": "1.7.7",
            },
        )

    def test_codex_url_allowlist_is_exact(self):
        self.assertEqual(helper.validate_codex_url("https://auth.openai.com/codex/device"), "https://auth.openai.com/codex/device")
        for value in (
            "http://auth.openai.com/codex/device",
            "https://evil.example/codex/device",
            "https://auth.openai.com.evil.example/codex/device",
            "https://user@auth.openai.com/codex/device",
            "https://auth.openai.com:444/codex/device",
            "https://auth.openai.com/codex/device?code=secret",
            "https://auth.openai.com/codex/device/",
        ):
            with self.subTest(value=value), self.assertRaises(helper.ProtocolError):
                helper.validate_codex_url(value)

    def test_ids_reject_path_traversal(self):
        for value in ("../account123", "ACCOUNT12345", "short", "account12345/"):
            with self.subTest(value=value), self.assertRaises(helper.HelperError):
                helper.validate_account(value)
        with self.assertRaises(helper.HelperError):
            helper.validate_attempt("../" + ATTEMPT)

    def test_symlink_escape_is_rejected(self):
        outside = self.root.parent / "outside-auth-test"
        outside.mkdir(exist_ok=True)
        (self.root / "account12345").symlink_to(outside, target_is_directory=True)
        try:
            with self.assertRaises(helper.HelperError):
                helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        finally:
            (self.root / "account12345").unlink()
            outside.rmdir()

    def test_config_files_are_allowlisted_and_validated(self):
        listed = helper.list_config(self.root, ACCOUNT, "codex")
        self.assertEqual([item["path"] for item in listed["files"]], ["config.toml"])
        saved = helper._write_config(self.root, ACCOUNT, "codex", "config.toml", 'model = "gpt-test"\n')
        self.assertTrue(saved["exists"])
        self.assertEqual(helper._read_config(self.root, ACCOUNT, "codex", "config.toml")["content"], 'model = "gpt-test"\n')
        with self.assertRaises(helper.HelperError):
            helper._read_config(self.root, ACCOUNT, "codex", "auth.json")
        with self.assertRaises(helper.HelperError):
            helper._write_config(self.root, ACCOUNT, "claude", "settings.json", '{"apiKey":"secret"}')

    def test_config_symlink_is_rejected(self):
        profile = self.root / ACCOUNT / "claude"
        helper._mkdir_secure(self.root, profile)
        outside = self.root.parent / "outside-config-test.json"
        outside.write_text("{}", encoding="utf-8")
        (profile / "settings.json").symlink_to(outside)
        try:
            with self.assertRaises(helper.HelperError):
                helper._read_config(self.root, ACCOUNT, "claude", "settings.json")
        finally:
            (profile / "settings.json").unlink()
            outside.unlink()


class JSONRPCTest(unittest.TestCase):
    class FakeProcess:
        def __init__(self, messages):
            self.stdin = io.BytesIO()
            reader, writer = os.pipe()
            os.write(writer, b"".join((json.dumps(value) + "\n").encode() for value in messages))
            os.close(writer)
            self.stdout = os.fdopen(reader, "rb", buffering=0)

    def test_request_keeps_interleaved_notification(self):
        process = self.FakeProcess([
            {"method": "account/login/completed", "params": {"success": True}},
            {"id": 1, "result": {"ok": True}},
        ])
        rpc = helper.JSONRPC(process)
        rpc.selector = mock.Mock()
        rpc.selector.select.return_value = [(1, 1)]
        self.assertEqual(rpc.request("initialize", {}, time.monotonic() + 1), {"ok": True})
        self.assertEqual(
            rpc.wait_notification("account/login/completed", time.monotonic() + 1, threading.Event()),
            {"success": True},
        )

    def test_receive_rejects_invalid_explicit_header(self):
        process = self.FakeProcess([{"jsonrpc": "1.0", "id": 1, "result": {"ok": True}}])
        rpc = helper.JSONRPC(process)
        rpc.selector = mock.Mock()
        rpc.selector.select.return_value = [(1, 1)]
        with self.assertRaises(helper.ProtocolError):
            rpc.receive(time.monotonic() + 1)

    def test_send_uses_codex_jsonl_wire_format(self):
        process = self.FakeProcess([])
        rpc = helper.JSONRPC(process)
        rpc.send({"jsonrpc": "2.0", "id": 7, "method": "initialize", "params": {}})
        self.assertEqual(process.stdin.getvalue(), b'{"id":7,"method":"initialize","params":{}}\n')

    def test_malformed_message_fails_closed(self):
        process = self.FakeProcess([])
        process.stdout.close()
        reader, writer = os.pipe()
        os.write(writer, b"not-json\n")
        os.close(writer)
        process.stdout = os.fdopen(reader, "rb", buffering=0)
        rpc = helper.JSONRPC(process)
        rpc.selector = mock.Mock()
        rpc.selector.select.return_value = [(1, 1)]
        with self.assertRaises(helper.ProtocolError):
            rpc.receive(time.monotonic() + 1)


class StateAndTransactionTest(RootCase):
    def _create_stage(self):
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        helper._mkdir_secure(self.root, item["stage"])
        (item["stage"] / "auth.json").write_text("new", encoding="utf-8")
        return item

    def test_commit_replaces_stable_only_after_stage_ready(self):
        item = self._create_stage()
        helper._mkdir_secure(self.root, item["stable"])
        (item["stable"] / "auth.json").write_text("old", encoding="utf-8")
        helper.commit_staging(self.root, item, "codex")
        self.assertEqual((item["stable"] / "auth.json").read_text(), "new")
        self.assertFalse(item["stage"].exists())
        self.assertFalse(item["marker"].exists())
        self.assertFalse(item["previous"].exists())

    def test_remove_tree_unlinks_nested_symlinks_without_following(self):
        item = self._create_stage()
        temporary = item["stage"] / "tmp" / "arg0"
        helper._mkdir_secure(self.root, temporary)
        outside = self.root.parent / "outside-auth-remove-test"
        outside.write_text("outside", encoding="utf-8")
        (temporary / "codex").symlink_to(outside)
        try:
            self.assertTrue(helper._remove_tree(self.root, item["stage"] / "tmp"))
            self.assertFalse((item["stage"] / "tmp").exists())
            self.assertEqual(outside.read_text(encoding="utf-8"), "outside")
        finally:
            outside.unlink(missing_ok=True)

    def test_recover_restores_old_profile_before_install(self):
        item = self._create_stage()
        helper._mkdir_secure(self.root, item["previous"])
        (item["previous"] / "auth.json").write_text("old", encoding="utf-8")
        helper._write_marker(self.root, item["marker"], "backed_up", "codex")
        helper.recover_commit(self.root, item, "codex")
        self.assertEqual((item["stable"] / "auth.json").read_text(), "old")
        self.assertTrue(item["stage"].exists())
        self.assertFalse(item["marker"].exists())

    def test_terminal_state_clears_transient_values(self):
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        state = helper.base_state(ACCOUNT, ATTEMPT, "codex", int(time.time()) + 60)
        state.update(verification_url="https://auth.openai.com/codex/device", user_code="CODE-123", terminal_available=True)
        helper.update_state(self.root, item["state"], state, "failed", error_code="provider_login_failed")
        loaded = helper.inspect_attempt(self.root, ACCOUNT, ATTEMPT, "codex")
        self.assertEqual(loaded["verification_url"], "")
        self.assertEqual(loaded["user_code"], "")
        self.assertFalse(loaded["terminal_available"])

    def test_cleanup_rollback_restores_profile(self):
        stable = self.root / ACCOUNT
        helper._mkdir_secure(self.root, stable / "codex")
        (stable / "codex" / "auth.json").write_text("secret", encoding="utf-8")
        helper.prepare_cleanup(self.root, ACCOUNT, CLEANUP, 100)
        self.assertFalse(stable.exists())
        helper.finish_cleanup(self.root, ACCOUNT, CLEANUP, "rollback")
        self.assertEqual((stable / "codex" / "auth.json").read_text(), "secret")

    def test_cleanup_commit_removes_only_quarantined_account(self):
        stable = self.root / ACCOUNT
        other = self.root / "another12345"
        helper._mkdir_secure(self.root, stable / "claude")
        helper._mkdir_secure(self.root, other / "codex")
        (stable / "claude" / "config.json").write_text("secret", encoding="utf-8")
        (other / "codex" / "auth.json").write_text("other", encoding="utf-8")
        helper.prepare_cleanup(self.root, ACCOUNT, CLEANUP, 100)
        helper.finish_cleanup(self.root, ACCOUNT, CLEANUP, "commit")
        self.assertFalse(stable.exists())
        self.assertEqual((other / "codex" / "auth.json").read_text(), "other")

    def test_reconcile_rolls_back_interrupted_cleanup(self):
        stable = self.root / ACCOUNT
        helper._mkdir_secure(self.root, stable / "codex")
        (stable / "codex" / "auth.json").write_text("secret", encoding="utf-8")
        helper.prepare_cleanup(self.root, ACCOUNT, CLEANUP, 100)
        result = helper.reconcile(self.root, 200)
        self.assertEqual(result["recovered"], 1)
        self.assertEqual((stable / "codex" / "auth.json").read_text(), "secret")


class ProviderNormalizationTest(RootCase):
    def test_claude_status_is_structured(self):
        self.assertTrue(helper.validate_claude_status({"loggedIn": True, "authMethod": "oauth_token", "apiProvider": "firstParty"}))
        self.assertFalse(helper.validate_claude_status({"loggedIn": False}))
        for value in ({}, {"loggedIn": "true"}, {"loggedIn": True, "authMethod": ""}):
            with self.assertRaises(helper.ProtocolError):
                helper.validate_claude_status(value)

    def test_codex_account_result_is_structured(self):
        self.assertTrue(helper.validate_account_result({"requiresOpenaiAuth": True, "account": {"type": "chatgpt"}}))
        self.assertFalse(helper.validate_account_result({"requiresOpenaiAuth": True, "account": None}))
        with self.assertRaises(helper.ProtocolError):
            helper.validate_account_result({"account": {"type": "chatgpt"}})

    def test_logout_removes_local_profile_when_provider_fails(self):
        stable = self.root / ACCOUNT / "claude"
        helper._mkdir_secure(self.root, stable)
        (stable / "config.json").write_text("secret", encoding="utf-8")
        failed = subprocess.CompletedProcess([], 1, b"", b"provider-secret")
        with mock.patch.object(helper.subprocess, "run", return_value=failed):
            result = helper.provider_logout(self.root, ACCOUNT, "claude")
        self.assertTrue(result["local_credentials_removed"])
        self.assertFalse(result["provider_logout_confirmed"])
        self.assertFalse(stable.exists())
        self.assertNotIn("provider-secret", json.dumps(result))

    def test_status_without_profile_does_not_invoke_provider(self):
        with mock.patch.object(helper.subprocess, "run") as run:
            result = helper.provider_status(self.root, ACCOUNT, "claude")
        self.assertEqual(result["status"], "unauthenticated")
        run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
