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
      verification_url: 'https://auth.openai.com/codex/device',
      user_code: 'ABCD-EFGH',
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
    verification_url: 'https://auth.openai.com/codex/device',
    user_code: 'ABCD-EFGH',
  });
  assert.equal(attempt.verificationURL, '');
  assert.equal(attempt.userCode, '');
  assert.equal(auth.isTerminalAttempt(attempt.state), true);
});

test('official URLs must be credential-free HTTPS', () => {
  assert.equal(auth.safeOfficialURL('http://auth.openai.com/codex/device'), '');
  assert.equal(auth.safeOfficialURL('https://user:secret@auth.openai.com/codex/device'), '');
  assert.equal(auth.safeOfficialURL('https://auth.openai.com:8443/codex/device'), '');
  assert.equal(auth.safeOfficialURL('https://auth.openai.com/codex/device'), 'https://auth.openai.com/codex/device');
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
