import base64
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest import mock
from urllib.parse import parse_qs, urlsplit


MODULE_PATH = Path(__file__).with_name("auth-helper.py")
spec = importlib.util.spec_from_file_location("vibedock_auth_helper", MODULE_PATH)
helper = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(helper)

ACCOUNT = "account12345"
ATTEMPT = "attempt12345678901234567"
CLEANUP = "cleanup1234567890123456789012345"


def jwt(claims):
    def encoded(value):
        raw = json.dumps(value, separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(raw).decode().rstrip("=")

    return encoded({"alg": "none"}) + "." + encoded(claims) + ".signature"


class FakeResponse:
    def __init__(self, value):
        self.body = json.dumps(value).encode()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self, limit):
        return self.body[:limit]


class RootCase(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()

    def tearDown(self):
        self.temporary.cleanup()


class ValidationTest(RootCase):
    def test_manifest_is_static_and_does_not_probe_clis(self):
        self.assertEqual(
            helper.compatibility_manifest(True),
            {
                "protocol": 1,
                "codex_version": "0.144.6",
                "claude_version": "2.1.215",
                "ttyd_version": "1.7.7",
            },
        )

    def test_authorization_urls_use_exact_provider_contracts(self):
        for provider, host, path, redirect in (
            ("codex", "auth.openai.com", "/oauth/authorize", helper.CODEX_OAUTH_REDIRECT_URI),
            ("claude", "claude.com", "/cai/oauth/authorize", helper.CLAUDE_OAUTH_REDIRECT_URI),
        ):
            with self.subTest(provider=provider):
                url = helper.build_authorization_url(provider, "state-value", "challenge-value")
                self.assertEqual(helper.validate_authorization_url(url, provider), url)
                parsed = urlsplit(url)
                query = parse_qs(parsed.query)
                self.assertEqual(parsed.hostname, host)
                self.assertEqual(parsed.path, path)
                self.assertEqual(query["redirect_uri"], [redirect])
                self.assertEqual(query["code_challenge_method"], ["S256"])

        valid = helper.build_authorization_url("codex", "state-value", "challenge-value")
        for value in (
            valid.replace("https://", "http://", 1),
            valid.replace("auth.openai.com", "auth.openai.com.evil.example", 1),
            valid.replace("redirect_uri=", "redirect_uri=https%3A%2F%2Fevil.example%2F&ignored="),
            valid + "#fragment",
        ):
            with self.subTest(value=value), self.assertRaises(helper.ProtocolError):
                helper.validate_authorization_url(value, "codex")

    def test_authorization_response_requires_exact_callback_and_state(self):
        state = "state-value-1234"
        code = "authorization-code"
        codex = f"http://localhost:1455/auth/callback?code={code}&state={state}"
        self.assertEqual(helper.parse_authorization_response("codex", codex, state), code)
        self.assertEqual(helper.parse_authorization_response("claude", code + "#" + state, state), code)
        for provider, value in (
            ("codex", f"https://localhost:1455/auth/callback?code={code}&state={state}"),
            ("codex", f"http://127.0.0.1:1455/auth/callback?code={code}&state={state}"),
            ("codex", "?code=" + code + "&state=" + state),
            ("claude", code + "#wrong-state"),
        ):
            with self.subTest(provider=provider, value=value), self.assertRaises(helper.HelperError):
                helper.parse_authorization_response(provider, value, state)

    def test_ids_and_symlink_escapes_are_rejected(self):
        for value in ("../account123", "ACCOUNT12345", "short", "account12345/"):
            with self.subTest(value=value), self.assertRaises(helper.HelperError):
                helper.validate_account(value)
        outside = self.root.parent / "outside-auth-test"
        outside.mkdir(exist_ok=True)
        (self.root / ACCOUNT).symlink_to(outside, target_is_directory=True)
        try:
            with self.assertRaises(helper.HelperError):
                helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        finally:
            (self.root / ACCOUNT).unlink()
            outside.rmdir()

    def test_config_files_are_allowlisted_and_protect_secrets(self):
        codex_default = helper._read_config(self.root, ACCOUNT, "codex", "config.toml")
        claude_default = helper._read_config(self.root, ACCOUNT, "claude", "settings.json")
        self.assertFalse(codex_default["exists"])
        self.assertEqual(codex_default["content"], "")
        self.assertIn("Optional Codex settings", codex_default["default_content"])
        self.assertFalse(claude_default["exists"])
        self.assertEqual(claude_default["content"], "")
        self.assertEqual(json.loads(claude_default["default_content"]), {})

        codex_root = self.root / ACCOUNT / "codex"
        claude_root = self.root / ACCOUNT / "claude"
        helper._mkdir_secure(self.root, codex_root)
        helper._mkdir_secure(self.root, claude_root)
        (codex_root / "auth.json").write_text('{"access_token":"must-not-leak"}', encoding="utf-8")
        (claude_root / ".credentials.json").write_text('{"oauth":"must-not-leak"}', encoding="utf-8")
        codex_listing = helper.list_config(self.root, ACCOUNT, "codex")
        claude_listing = helper.list_config(self.root, ACCOUNT, "claude")
        self.assertEqual([item["path"] for item in codex_listing["files"]], ["config.toml"])
        self.assertEqual([item["path"] for item in claude_listing["files"]], ["settings.json"])
        self.assertNotIn("must-not-leak", json.dumps((codex_listing, claude_listing)))

        saved = helper._write_config(self.root, ACCOUNT, "codex", "config.toml", 'model = "gpt-test"\n')
        self.assertTrue(saved["exists"])
        with self.assertRaises(helper.HelperError):
            helper._read_config(self.root, ACCOUNT, "codex", "auth.json")
        with self.assertRaises(helper.HelperError):
            helper._write_config(self.root, ACCOUNT, "claude", "settings.json", '{"apiKey":"secret"}')


class OAuthFlowTest(RootCase):
    def start(self, provider, attempt=ATTEMPT):
        return helper.start_oauth(self.root, ACCOUNT, attempt, provider, int(time.time()) + 600)

    def test_start_keeps_verifier_private_and_uses_provider_pkce_sizes(self):
        for provider, minimum_verifier_length in (("codex", 80), ("claude", 40)):
            attempt = ATTEMPT[:-1] + ("c" if provider == "codex" else "a")
            state = self.start(provider, attempt)
            item = helper.paths(self.root, ACCOUNT, attempt, provider)
            private = helper._read_json(self.root, item["oauth"])
            public_text = item["state"].read_text(encoding="utf-8")
            self.assertEqual(state["state"], "awaiting_user")
            self.assertGreaterEqual(len(private["code_verifier"]), minimum_verifier_length)
            self.assertNotIn(private["code_verifier"], public_text)
            self.assertEqual(os.stat(item["oauth"]).st_mode & 0o777, 0o600)

    def test_codex_completion_writes_canonical_auth_json_and_preserves_config(self):
        helper._write_config(self.root, ACCOUNT, "codex", "config.toml", 'model = "gpt-test"\n')
        stable = self.root / ACCOUNT / "codex"
        (stable / "state.sqlite").write_text("runtime-state", encoding="utf-8")
        runtime_dir = stable / "tmp" / "arg0"
        helper._mkdir_secure(self.root, runtime_dir)
        outside = self.root.parent / "outside-relogin-test"
        outside.write_text("provider-binary", encoding="utf-8")
        self.addCleanup(outside.unlink, missing_ok=True)
        (runtime_dir / "codex").symlink_to(outside)
        self.start("codex")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        private = helper._read_json(self.root, item["oauth"])
        id_token = jwt({"https://api.openai.com/auth": {"chatgpt_account_id": "chatgpt-account"}})
        response = {
            "id_token": id_token,
            "access_token": "codex-access-token",
            "refresh_token": "codex-refresh-token",
        }
        callback = "http://localhost:1455/auth/callback?code=authorization-code&state=" + private["state"]
        with mock.patch.object(helper.TOKEN_OPENER, "open", return_value=FakeResponse(response)) as opened:
            result = helper.complete_oauth(self.root, ACCOUNT, ATTEMPT, "codex", callback)
        self.assertEqual(result["state"], "completed")
        request = opened.call_args.args[0]
        self.assertEqual(request.full_url, helper.CODEX_OAUTH_TOKEN_URL)
        self.assertNotIn("authorization-code", json.dumps(result))
        credentials = json.loads((item["stable"] / "auth.json").read_text())
        self.assertEqual(credentials["auth_mode"], "chatgpt")
        self.assertIsNone(credentials["OPENAI_API_KEY"])
        self.assertEqual(credentials["tokens"]["account_id"], "chatgpt-account")
        self.assertEqual((item["stable"] / "config.toml").read_text(), 'model = "gpt-test"\n')
        self.assertEqual((item["stable"] / "state.sqlite").read_text(), "runtime-state")
        self.assertTrue((item["stable"] / "tmp" / "arg0" / "codex").is_symlink())
        self.assertEqual(outside.read_text(), "provider-binary")
        self.assertEqual(os.stat(item["stable"] / "auth.json").st_mode & 0o777, 0o600)
        self.assertFalse(item["oauth"].exists())

    def test_codex_completion_allows_official_null_account_id(self):
        self.start("codex")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        private = helper._read_json(self.root, item["oauth"])
        response = {
            "id_token": jwt({"sub": "chatgpt-user"}),
            "access_token": "codex-access-token",
            "refresh_token": "codex-refresh-token",
        }
        callback = "http://localhost:1455/auth/callback?code=authorization-code&state=" + private["state"]
        with mock.patch.object(helper.TOKEN_OPENER, "open", return_value=FakeResponse(response)):
            result = helper.complete_oauth(self.root, ACCOUNT, ATTEMPT, "codex", callback)
        self.assertEqual(result["state"], "completed")
        credentials = json.loads((item["stable"] / "auth.json").read_text())
        self.assertIsNone(credentials["tokens"]["account_id"])
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "codex")["status"], "authenticated")

    def test_claude_completion_writes_credentials_and_sends_state(self):
        self.start("claude")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "claude")
        private = helper._read_json(self.root, item["oauth"])
        response = {
            "access_token": "claude-access-token",
            "refresh_token": "claude-refresh-token",
            "expires_in": 3600,
            "scope": "user:profile user:inference user:sessions:claude_code",
        }
        pasted = "authorization-code#" + private["state"]
        with mock.patch.object(helper.TOKEN_OPENER, "open", return_value=FakeResponse(response)) as opened:
            result = helper.complete_oauth(self.root, ACCOUNT, ATTEMPT, "claude", pasted)
        self.assertEqual(result["state"], "completed")
        request_body = json.loads(opened.call_args.args[0].data)
        self.assertEqual(request_body["state"], private["state"])
        credentials = json.loads((item["stable"] / ".credentials.json").read_text())
        oauth = credentials["claudeAiOauth"]
        self.assertEqual(oauth["refreshToken"], "claude-refresh-token")
        self.assertIn("user:inference", oauth["scopes"])
        self.assertGreater(oauth["expiresAt"], int(time.time() * 1000))

    def test_state_mismatch_fails_without_replacing_existing_credentials(self):
        stable = self.root / ACCOUNT / "codex"
        helper._mkdir_secure(self.root, stable)
        (stable / "auth.json").write_text("old-credentials", encoding="utf-8")
        self.start("codex")
        callback = "http://localhost:1455/auth/callback?code=authorization-code&state=wrong-state"
        with mock.patch.object(helper.TOKEN_OPENER, "open") as opened:
            result = helper.complete_oauth(self.root, ACCOUNT, ATTEMPT, "codex", callback)
        self.assertEqual(result["state"], "failed")
        self.assertEqual(result["error_code"], "authorization_state_mismatch")
        self.assertEqual((stable / "auth.json").read_text(), "old-credentials")
        opened.assert_not_called()
        self.assertFalse(helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")["oauth"].exists())

    def test_inspect_expires_only_an_unfinished_attempt(self):
        state = self.start("claude")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "claude")
        state["expires_at"] = int(time.time()) - 1
        helper._atomic_json(self.root, item["state"], state)
        loaded = helper.inspect_attempt(self.root, ACCOUNT, ATTEMPT, "claude")
        self.assertEqual(loaded["state"], "expired")
        self.assertEqual(loaded["verification_url"], "")
        self.assertFalse(item["oauth"].exists())
        self.assertFalse(item["stage"].exists())

    def test_inspect_expires_a_stuck_verification_after_exchange_deadline(self):
        state = self.start("claude")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "claude")
        oauth = helper._read_json(self.root, item["oauth"])
        oauth["verification_deadline"] = int(time.time()) - 1
        helper._atomic_json(self.root, item["oauth"], oauth)
        helper.update_state(self.root, item["state"], state, "verifying")
        loaded = helper.inspect_attempt(self.root, ACCOUNT, ATTEMPT, "claude")
        self.assertEqual(loaded["state"], "expired")
        self.assertFalse(item["oauth"].exists())

    def test_reconcile_retains_recent_terminal_attempt_for_restart_recovery(self):
        state = self.start("codex")
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        helper.update_state(self.root, item["state"], state, "completed", error_code="")
        item["oauth"].unlink()
        now = int(time.time())
        self.assertEqual(helper.reconcile(self.root, now)["removed"], 0)
        self.assertEqual(helper.inspect_attempt(self.root, ACCOUNT, ATTEMPT, "codex")["state"], "completed")
        old = now - helper.TERMINAL_RETENTION_SECONDS - 1
        os.utime(item["state"], (old, old))
        self.assertEqual(helper.reconcile(self.root, now)["removed"], 1)
        self.assertFalse(item["base"].exists())


class StateAndTransactionTest(RootCase):
    def create_stage(self):
        item = helper.paths(self.root, ACCOUNT, ATTEMPT, "codex")
        helper._mkdir_secure(self.root, item["stage"])
        (item["stage"] / "auth.json").write_text("new", encoding="utf-8")
        return item

    def test_commit_replaces_stable_atomically(self):
        item = self.create_stage()
        helper._mkdir_secure(self.root, item["stable"])
        (item["stable"] / "auth.json").write_text("old", encoding="utf-8")
        helper.commit_staging(self.root, item, "codex")
        self.assertEqual((item["stable"] / "auth.json").read_text(), "new")
        self.assertFalse(item["stage"].exists())
        self.assertFalse(item["marker"].exists())

    def test_remove_tree_unlinks_provider_symlinks_without_following(self):
        item = self.create_stage()
        temporary = item["stage"] / "tmp" / "arg0"
        helper._mkdir_secure(self.root, temporary)
        outside = self.root.parent / "outside-auth-remove-test"
        outside.write_text("outside", encoding="utf-8")
        (temporary / "codex").symlink_to(outside)
        try:
            self.assertTrue(helper._remove_tree(self.root, item["stage"] / "tmp"))
            self.assertEqual(outside.read_text(), "outside")
        finally:
            outside.unlink(missing_ok=True)

    def test_cleanup_rollback_restores_profile(self):
        stable = self.root / ACCOUNT
        helper._mkdir_secure(self.root, stable / "codex")
        (stable / "codex" / "auth.json").write_text("secret", encoding="utf-8")
        helper.prepare_cleanup(self.root, ACCOUNT, CLEANUP, 100)
        helper.finish_cleanup(self.root, ACCOUNT, CLEANUP, "rollback")
        self.assertEqual((stable / "codex" / "auth.json").read_text(), "secret")

    def test_cleanup_accepts_provider_runtime_symlinks_without_following_them(self):
        stable = self.root / ACCOUNT
        runtime_dir = stable / "codex" / "tmp" / "arg0"
        helper._mkdir_secure(self.root, runtime_dir)
        outside = self.root.parent / "outside-cleanup-test"
        outside.write_text("outside", encoding="utf-8")
        (runtime_dir / "codex").symlink_to(outside)
        try:
            helper.prepare_cleanup(self.root, ACCOUNT, CLEANUP, 100)
            helper.finish_cleanup(self.root, ACCOUNT, CLEANUP, "commit")
            self.assertFalse(stable.exists())
            self.assertEqual(outside.read_text(), "outside")
        finally:
            outside.unlink(missing_ok=True)


class ProviderStatusTest(RootCase):
    def test_status_reads_canonical_files_and_ignores_codex_runtime_symlinks(self):
        stable = self.root / ACCOUNT / "codex"
        helper._mkdir_secure(self.root, stable / "tmp" / "arg0")
        outside = self.root.parent / "outside-status-test"
        outside.write_text("binary", encoding="utf-8")
        (stable / "tmp" / "arg0" / "codex").symlink_to(outside)
        credentials = {
            "auth_mode": "chatgpt",
            "OPENAI_API_KEY": None,
            "tokens": {
                "id_token": "id-token-value",
                "access_token": "access-token-value",
                "refresh_token": "refresh-token-value",
                "account_id": "account-value",
            },
        }
        helper._atomic_json(self.root, stable / "auth.json", credentials)
        try:
            result = helper.provider_status(self.root, ACCOUNT, "codex")
            self.assertEqual(result["status"], "authenticated")
        finally:
            helper._remove_tree(self.root, stable)
            outside.unlink(missing_ok=True)

    def test_missing_and_malformed_credentials_have_distinct_status(self):
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "claude")["status"], "unauthenticated")
        stable = self.root / ACCOUNT / "claude"
        helper._mkdir_secure(self.root, stable)
        (stable / ".credentials.json").write_text("{}", encoding="utf-8")
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "claude")["status"], "unauthenticated")
        (stable / ".credentials.json").write_text("not-json", encoding="utf-8")
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "claude")["status"], "unavailable")

    def test_config_only_profile_is_unauthenticated(self):
        helper._write_config(self.root, ACCOUNT, "codex", "config.toml", 'model = "gpt-test"\n')
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "codex")["status"], "unauthenticated")

    def test_logout_removes_only_credentials_without_invoking_a_cli(self):
        stable = self.root / ACCOUNT / "claude"
        helper._mkdir_secure(self.root, stable)
        (stable / ".credentials.json").write_text("secret", encoding="utf-8")
        (stable / "settings.json").write_text('{"theme":"dark"}', encoding="utf-8")
        result = helper.provider_logout(self.root, ACCOUNT, "claude")
        self.assertTrue(result["local_credentials_removed"])
        self.assertFalse(result["provider_logout_confirmed"])
        self.assertFalse((stable / ".credentials.json").exists())
        self.assertEqual((stable / "settings.json").read_text(), '{"theme":"dark"}')
        self.assertEqual(helper.provider_status(self.root, ACCOUNT, "claude")["status"], "unauthenticated")


if __name__ == "__main__":
    unittest.main()
