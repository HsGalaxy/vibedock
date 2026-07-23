(function exposeControlAuth(root, factory) {
  const controlAuth = factory();
  if (typeof module === 'object' && module.exports) module.exports = controlAuth;
  if (root) root.VibeDockControlAuth = controlAuth;
}(typeof globalThis !== 'undefined' ? globalThis : this, function createControlAuth() {
  'use strict';

  function cleanString(value) {
    return typeof value === 'string' ? value.trim() : '';
  }

  function normalizeStatus(payload = {}) {
    const initialized = payload.initialized === true;
    const authenticated = initialized && payload.authenticated === true;
    return {
      initialized,
      authenticated,
      username: authenticated ? cleanString(payload.username) : '',
      csrfToken: authenticated ? cleanString(payload.csrf_token) : '',
    };
  }

  function modeFor(status) {
    if (status?.authenticated) return 'application';
    return status?.initialized ? 'login' : 'register';
  }

  function isReadMethod(method) {
    const value = cleanString(method || 'GET').toUpperCase();
    return value === 'GET' || value === 'HEAD';
  }

  function validateCredentials(mode, username, password, confirmation = '', setupToken = '') {
    const normalizedUsername = cleanString(username);
    const usernameLength = Array.from(normalizedUsername).length;
    if (usernameLength < 3) return '用户名至少需要 3 个字符';
    if (usernameLength > 64) return '用户名不能超过 64 个字符';
    if (!password) return '请输入密码';
    if (mode === 'register' && Array.from(password).length < 12) return '密码至少需要 12 个字符';
    if (mode === 'register' && password !== confirmation) return '两次输入的密码不一致';
    if (mode === 'register' && !cleanString(setupToken)) return '请输入初始化代码';
    return '';
  }

  function resolvePreviewOrigin(currentURL, config = {}) {
    const current = new URL(currentURL);
    const controlOrigin = current.origin;
    const configuredOrigin = cleanString(config.preview_origin);
    let resolved;
    if (configuredOrigin) {
      try {
        const configured = new URL(configuredOrigin);
        const isOriginOnly = configured.username === '' && configured.password === ''
          && (configured.pathname === '' || configured.pathname === '/')
          && configured.search === '' && configured.hash === '';
        if ((configured.protocol === 'http:' || configured.protocol === 'https:') && isOriginOnly) {
          if (configured.protocol !== current.protocol || configured.hostname !== current.hostname) {
            throw new Error('Preview origin must use the control-plane protocol and hostname');
          }
          resolved = configured.origin;
        }
      } catch (_) { /* use the dedicated-port fallback below */ }
    }
    if (!resolved) {
      const configuredPort = Number(config.preview_port);
      const port = Number.isInteger(configuredPort) && configuredPort >= 1 && configuredPort <= 65535 ? configuredPort : 8788;
      current.port = String(port);
      resolved = current.origin;
    }
    if (resolved === controlOrigin) throw new Error('Preview origin must differ from the control-plane origin');
    return resolved;
  }

  return {isReadMethod, modeFor, normalizeStatus, resolvePreviewOrigin, validateCredentials};
}));
