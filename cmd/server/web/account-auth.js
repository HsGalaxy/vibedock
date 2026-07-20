(function exposeAccountAuth(root, factory) {
  const accountAuth = factory();
  if (typeof module === 'object' && module.exports) module.exports = accountAuth;
  if (root) root.VibeDockAccountAuth = accountAuth;
}(typeof globalThis !== 'undefined' ? globalThis : this, function createAccountAuth() {
  'use strict';

  const ACTIVE_ATTEMPT_STATES = new Set(['starting', 'awaiting_user', 'verifying', 'cancelling']);
  const TERMINAL_ATTEMPT_STATES = new Set(['completed', 'cancelled', 'expired', 'failed']);
  const ATTEMPT_STATE_ALIASES = {
    pending: 'starting',
    waiting: 'awaiting_user',
    awaiting: 'awaiting_user',
    awaiting_user_action: 'awaiting_user',
    authorizing: 'awaiting_user',
    success: 'completed',
    succeeded: 'completed',
    canceled: 'cancelled',
    error: 'failed',
  };
  const AUTH_STATUS_ALIASES = {
    logged_in: 'authenticated',
    ready: 'authenticated',
    logged_out: 'unauthenticated',
    not_authenticated: 'unauthenticated',
    in_progress: 'login_in_progress',
    pending: 'login_in_progress',
    error: 'unknown',
  };

  function cleanString(value) {
    return typeof value === 'string' ? value.trim() : '';
  }

  function normalizeProvider(value) {
    const provider = cleanString(value).toLowerCase();
    if (provider === 'claude-code' || provider === 'anthropic') return 'claude';
    if (provider === 'openai') return 'codex';
    return provider === 'codex' || provider === 'claude' ? provider : '';
  }

  function normalizeAttemptState(value) {
    const state = cleanString(value).toLowerCase();
    return ATTEMPT_STATE_ALIASES[state] || state || 'starting';
  }

  function normalizeAuthState(value, authenticated) {
    if (authenticated === true) return 'authenticated';
    if (authenticated === false && !value) return 'unauthenticated';
    const state = cleanString(value).toLowerCase();
    const normalized = AUTH_STATUS_ALIASES[state] || state;
    return ['authenticated', 'unauthenticated', 'login_in_progress', 'unknown', 'unavailable'].includes(normalized)
      ? normalized
      : 'unknown';
  }

  function pollDelayMs(source) {
    const milliseconds = Number(
      source?.poll_after_ms ?? source?.poll_interval_ms ?? source?.retry_after_ms ?? source?.next_poll_ms,
    );
    const seconds = Number(
      source?.poll_after_seconds ?? source?.poll_interval_seconds ?? source?.retry_after_seconds ?? source?.poll_after,
    );
    const requested = Number.isFinite(milliseconds) && milliseconds > 0
      ? milliseconds
      : Number.isFinite(seconds) && seconds > 0
        ? seconds * 1000
        : 2000;
    return Math.min(15000, Math.max(1000, Math.round(requested)));
  }

  function normalizeAttempt(payload, fallback = {}) {
    const source = payload?.attempt || payload?.login_attempt || payload || {};
    const state = normalizeAttemptState(source.state || source.status || fallback.state);
    const terminal = TERMINAL_ATTEMPT_STATES.has(state);
    const error = typeof source.error === 'object' && source.error !== null ? source.error : {};
    const normalized = {
      id: cleanString(source.id || source.attempt_id || fallback.id),
      accountID: cleanString(source.account_id || fallback.accountID),
      provider: normalizeProvider(source.provider || source.agent || fallback.provider),
      state,
      expiresAt: cleanString(source.expires_at || source.expiry || fallback.expiresAt),
      pollAfterMs: pollDelayMs(source),
      message: cleanString(source.message || source.status_message),
      errorCode: cleanString(source.error_code || error.code),
      errorMessage: cleanString(source.error_message || error.message || (typeof source.error === 'string' ? source.error : '')),
      verificationURL: cleanString(source.verification_url || source.verification_uri || source.verification_uri_complete),
      userCode: cleanString(source.user_code || source.device_code),
    };
    if (terminal) {
      normalized.verificationURL = '';
      normalized.userCode = '';
    }
    return normalized;
  }

  function normalizeAuthStatus(payload) {
    const source = payload || {};
    const activeSource = source.active_attempt || source.login_attempt || source.attempt || null;
    const activeAttempt = activeSource && typeof activeSource === 'object' ? normalizeAttempt(activeSource) : null;
    const status = normalizeAuthState(source.status || source.auth_status || source.state, source.authenticated);
    return {
      status: activeAttempt && ACTIVE_ATTEMPT_STATES.has(activeAttempt.state) ? 'login_in_progress' : status,
      attemptID: cleanString(source.active_attempt_id || source.attempt_id || activeAttempt?.id),
      attemptState: activeAttempt?.state || (cleanString(source.attempt_state) ? normalizeAttemptState(source.attempt_state) : ''),
      message: cleanString(source.message || source.status_message),
      provider: normalizeProvider(source.provider || source.agent || activeAttempt?.provider),
      attempt: activeAttempt,
    };
  }

  function authStatusPresentation(status) {
    const value = normalizeAuthState(status);
    return ({
      authenticated: {label: '已登录', tone: 'authenticated'},
      unauthenticated: {label: '未登录', tone: 'unauthenticated'},
      login_in_progress: {label: '登录中', tone: 'in-progress'},
      unavailable: {label: '暂不可用', tone: 'unavailable'},
      unknown: {label: '状态未知', tone: 'unknown'},
    })[value];
  }

  function attemptStatePresentation(state) {
    return ({
      starting: '正在启动官方登录…',
      awaiting_user: '等待你在官方页面完成登录',
      verifying: '正在通过官方 CLI 验证登录状态…',
      cancelling: '正在取消登录…',
      completed: '登录完成，官方 CLI 已确认账号状态',
      cancelled: '登录已取消',
      expired: '登录请求已过期',
      failed: '登录未完成',
    })[normalizeAttemptState(state)] || '正在检查登录状态…';
  }

  function isActiveAttempt(state) {
    return ACTIVE_ATTEMPT_STATES.has(normalizeAttemptState(state));
  }

  function isTerminalAttempt(state) {
    return TERMINAL_ATTEMPT_STATES.has(normalizeAttemptState(state));
  }

  function safeOfficialURL(value) {
    const raw = cleanString(value);
    if (!raw || raw.length > 2048) return '';
    try {
      const parsed = new URL(raw);
      if (parsed.protocol !== 'https:' || parsed.username || parsed.password) return '';
      if (parsed.port && parsed.port !== '443') return '';
      return parsed.href;
    } catch (_) {
      return '';
    }
  }

  function secondsRemaining(expiresAt, now = Date.now()) {
    const expiry = Date.parse(cleanString(expiresAt));
    if (!Number.isFinite(expiry)) return null;
    return Math.max(0, Math.ceil((expiry - now) / 1000));
  }

  function formatCountdown(seconds) {
    if (!Number.isFinite(seconds)) return '等待服务返回有效期';
    const minutes = Math.floor(seconds / 60);
    const remainder = Math.max(0, seconds % 60);
    return `${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}`;
  }

  function shouldPoll({dialogOpen, documentHidden, state}) {
    return Boolean(dialogOpen && !documentHidden && isActiveAttempt(state));
  }

  return Object.freeze({
    ACTIVE_ATTEMPT_STATES,
    TERMINAL_ATTEMPT_STATES,
    normalizeProvider,
    normalizeAttemptState,
    normalizeAttempt,
    normalizeAuthStatus,
    authStatusPresentation,
    attemptStatePresentation,
    isActiveAttempt,
    isTerminalAttempt,
    pollDelayMs,
    safeOfficialURL,
    secondsRemaining,
    formatCountdown,
    shouldPoll,
  });
}));
