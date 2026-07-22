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
  const OFFICIAL_AUTHORIZATION_ENDPOINTS = Object.freeze({
    codex: {
      hostname: 'auth.openai.com',
      pathname: '/oauth/authorize',
      clientID: 'app_EMoamEEZ73f0CkXaXp7hrann',
      redirectURI: 'http://localhost:1455/auth/callback',
      scope: 'openid profile email offline_access api.connectors.read api.connectors.invoke',
      extra: {id_token_add_organizations:'true', codex_cli_simplified_flow:'true', originator:'codex_cli_rs'},
    },
    claude: {
      hostname: 'claude.com',
      pathname: '/cai/oauth/authorize',
      clientID: '9d1c250a-e61b-44d9-88ed-5944d1962f5e',
      redirectURI: 'https://platform.claude.com/oauth/code/callback',
      scope: 'org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload',
      extra: {code:'true'},
    },
  });

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
      verificationURL: cleanString(source.authorization_url || source.verification_url || source.verification_uri || source.verification_uri_complete),
    };
    if (terminal) {
      normalized.verificationURL = '';
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
      starting: '正在生成官方授权请求…',
      awaiting_user: '等待你在官方页面授权并提交返回结果',
      verifying: '正在校验授权响应并保存凭据…',
      cancelling: '正在取消登录…',
      completed: '登录完成，凭据已保存',
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

  function safeOfficialURL(value, provider = '') {
    const raw = cleanString(value);
    if (!raw || raw.length > 2048) return '';
    try {
      const parsed = new URL(raw);
      if (parsed.protocol !== 'https:' || parsed.username || parsed.password) return '';
      if (parsed.port && parsed.port !== '443') return '';
      if (parsed.hash) return '';
      const normalizedProvider = normalizeProvider(provider);
      const allowed = normalizedProvider
        ? [OFFICIAL_AUTHORIZATION_ENDPOINTS[normalizedProvider]]
        : Object.values(OFFICIAL_AUTHORIZATION_ENDPOINTS);
      const endpoint = allowed.find(candidate => candidate.hostname === parsed.hostname && candidate.pathname === parsed.pathname);
      if (!endpoint) return '';
      const required = {
        client_id:endpoint.clientID,
        redirect_uri:endpoint.redirectURI,
        response_type:'code',
        code_challenge_method:'S256',
        scope:endpoint.scope,
        ...endpoint.extra,
      };
      if (Object.entries(required).some(([key, expected]) => parsed.searchParams.getAll(key).length !== 1 || parsed.searchParams.get(key) !== expected)) return '';
      const state = parsed.searchParams.getAll('state');
      const challenge = parsed.searchParams.getAll('code_challenge');
      if (state.length !== 1 || !/^[A-Za-z0-9._~-]{20,256}$/.test(state[0])) return '';
      if (challenge.length !== 1 || !/^[A-Za-z0-9._~-]{43}$/.test(challenge[0])) return '';
      return parsed.href;
    } catch (_) {
      return '';
    }
  }

  function hasSingleQueryValue(parsed, key) {
    return parsed.searchParams.getAll(key).length === 1 && Boolean(parsed.searchParams.get(key));
  }

  function normalizeAuthorizationResponse(provider, value) {
    const normalizedProvider = normalizeProvider(provider);
    const raw = cleanString(value);
    if (!normalizedProvider || !raw || raw.length > 8192 || raw.includes('\0')) return '';
    if (normalizedProvider === 'codex') {
      try {
        const parsed = new URL(raw);
        if (parsed.protocol !== 'http:' || parsed.hostname !== 'localhost' || parsed.port !== '1455') return '';
        if (parsed.username || parsed.password || parsed.pathname !== '/auth/callback' || parsed.hash) return '';
        if (!hasSingleQueryValue(parsed, 'code') || !hasSingleQueryValue(parsed, 'state')) return '';
        return raw;
      } catch (_) {
        return '';
      }
    }
    try {
      const parsed = new URL(raw);
      if (parsed.protocol === 'https:' && parsed.hostname === 'platform.claude.com' && !parsed.port
          && !parsed.username && !parsed.password && parsed.pathname === '/oauth/code/callback' && !parsed.hash
          && hasSingleQueryValue(parsed, 'code') && hasSingleQueryValue(parsed, 'state')) return raw;
    } catch (_) {
      // Claude also returns a compact authorizationCode#state value.
    }
    if (/^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(raw)) return '';
    const separator = raw.indexOf('#');
    if (separator < 1 || separator !== raw.lastIndexOf('#')) return '';
    const code = raw.slice(0, separator);
    const state = raw.slice(separator + 1);
    if (!code || !state || /\s/.test(code) || /\s/.test(state)) return '';
    return raw;
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
    normalizeAuthorizationResponse,
    secondsRemaining,
    formatCountdown,
    shouldPoll,
  });
}));
