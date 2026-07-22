'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const auth = require('./account-auth.js');

test('normalizes active status and server polling interval', () => {
  const result = auth.normalizeAuthStatus({
    state: 'unauthenticated',
    attempt: {
      id: 'abcdefghijklmnopqrstuvwx',
      account_id: 'acct12345678',
      provider: 'codex',
      state: 'awaiting_user',
      poll_after_ms: 2400,
      verification_url: 'https://auth.openai.com/oauth/authorize?client_id=client&redirect_uri=redirect&state=state&code_challenge=challenge',
    },
  });
  assert.equal(result.status, 'login_in_progress');
  assert.equal(result.attemptID, 'abcdefghijklmnopqrstuvwx');
  assert.equal(result.attempt.pollAfterMs, 2400);
});

test('clears transient data for terminal attempts', () => {
  const attempt = auth.normalizeAttempt({
    id: 'abcdefghijklmnopqrstuvwx',
    account_id: 'acct12345678',
    provider: 'codex',
    state: 'completed',
    verification_url: 'https://auth.openai.com/oauth/authorize?client_id=client&redirect_uri=redirect&state=state&code_challenge=challenge',
  });
  assert.equal(attempt.verificationURL, '');
  assert.equal(auth.isTerminalAttempt(attempt.state), true);
});

test('official URLs are restricted to the provider authorization endpoints', () => {
  const state = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ';
  const challenge = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ';
  const codexQuery = new URLSearchParams({
    client_id:'app_EMoamEEZ73f0CkXaXp7hrann',
    redirect_uri:'http://localhost:1455/auth/callback',
    response_type:'code',
    scope:'openid profile email offline_access api.connectors.read api.connectors.invoke',
    state,
    code_challenge:challenge,
    code_challenge_method:'S256',
    id_token_add_organizations:'true',
    codex_cli_simplified_flow:'true',
    originator:'codex_cli_rs',
  });
  const claudeQuery = new URLSearchParams({
    code:'true',
    client_id:'9d1c250a-e61b-44d9-88ed-5944d1962f5e',
    response_type:'code',
    redirect_uri:'https://platform.claude.com/oauth/code/callback',
    scope:'org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload',
    code_challenge:challenge,
    code_challenge_method:'S256',
    state,
  });
  const codexURL = `https://auth.openai.com/oauth/authorize?${codexQuery}`;
  const claudeURL = `https://claude.com/cai/oauth/authorize?${claudeQuery}`;
  assert.equal(auth.safeOfficialURL(codexURL, 'codex'), codexURL);
  assert.equal(auth.safeOfficialURL(claudeURL, 'claude'), claudeURL);
  assert.equal(auth.safeOfficialURL(codexURL, 'claude'), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('https:', 'http:')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('https://', 'https://user:secret@')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('auth.openai.com', 'auth.openai.com:8443')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('/oauth/authorize', '/other')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('auth.openai.com', 'example.com')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('app_EMoamEEZ73f0CkXaXp7hrann', 'wrong-client')), '');
  assert.equal(auth.safeOfficialURL(codexURL.replace('code_challenge_method=S256', 'code_challenge_method=plain')), '');
});

test('validates pasted Codex callback URLs without rewriting them', () => {
  const callback = 'http://localhost:1455/auth/callback?code=secret-code&state=expected-state';
  assert.equal(auth.normalizeAuthorizationResponse('codex', `  ${callback}\n`), callback);
  assert.equal(auth.normalizeAuthorizationResponse('codex', 'https://localhost:1455/auth/callback?code=c&state=s'), '');
  assert.equal(auth.normalizeAuthorizationResponse('codex', 'http://localhost:1455/auth/callback?code=c'), '');
  assert.equal(auth.normalizeAuthorizationResponse('codex', 'http://localhost:1455/auth/callback?code=c&state=s&state=other'), '');
  assert.equal(auth.normalizeAuthorizationResponse('codex', 'http://localhost.evil.test:1455/auth/callback?code=c&state=s'), '');
});

test('accepts Claude code-state responses and its exact official callback URL', () => {
  const compact = 'authorization-code#expected-state';
  const callback = 'https://platform.claude.com/oauth/code/callback?code=authorization-code&state=expected-state';
  assert.equal(auth.normalizeAuthorizationResponse('claude', compact), compact);
  assert.equal(auth.normalizeAuthorizationResponse('claude', callback), callback);
  assert.equal(auth.normalizeAuthorizationResponse('claude', 'authorization-code'), '');
  assert.equal(auth.normalizeAuthorizationResponse('claude', 'authorization code#state'), '');
  assert.equal(auth.normalizeAuthorizationResponse('claude', 'https://example.com/callback?code=c&state=s#state'), '');
});

test('polling pauses for a hidden page or closed dialog', () => {
  assert.equal(auth.shouldPoll({dialogOpen: true, documentHidden: false, state: 'awaiting_user'}), true);
  assert.equal(auth.shouldPoll({dialogOpen: true, documentHidden: true, state: 'awaiting_user'}), false);
  assert.equal(auth.shouldPoll({dialogOpen: false, documentHidden: false, state: 'awaiting_user'}), false);
  assert.equal(auth.shouldPoll({dialogOpen: true, documentHidden: false, state: 'completed'}), false);
});

test('countdown is clamped and formatted', () => {
  assert.equal(auth.secondsRemaining('2026-01-01T00:00:01Z', Date.parse('2026-01-01T00:00:00Z')), 1);
  assert.equal(auth.secondsRemaining('2025-01-01T00:00:00Z', Date.parse('2026-01-01T00:00:00Z')), 0);
  assert.equal(auth.formatCountdown(125), '02:05');
});
