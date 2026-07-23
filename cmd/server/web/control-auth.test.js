'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const auth = require('./control-auth.js');

test('normalizes authentication status without retaining unauthenticated secrets', () => {
  assert.deepEqual(auth.normalizeStatus({
    initialized:true,
    authenticated:true,
    username:' admin ',
    csrf_token:' token ',
  }), {
    initialized:true,
    authenticated:true,
    username:'admin',
    csrfToken:'token',
  });
  assert.deepEqual(auth.normalizeStatus({
    initialized:true,
    authenticated:false,
    username:'stale',
    csrf_token:'stale',
  }), {
    initialized:true,
    authenticated:false,
    username:'',
    csrfToken:'',
  });
});

test('selects first-run registration, login, and application modes', () => {
  assert.equal(auth.modeFor({initialized:false, authenticated:false}), 'register');
  assert.equal(auth.modeFor({initialized:true, authenticated:false}), 'login');
  assert.equal(auth.modeFor({initialized:true, authenticated:true}), 'application');
});

test('identifies methods that need CSRF protection', () => {
  assert.equal(auth.isReadMethod('GET'), true);
  assert.equal(auth.isReadMethod('head'), true);
  assert.equal(auth.isReadMethod('POST'), false);
  assert.equal(auth.isReadMethod('DELETE'), false);
});

test('validates first-run credentials', () => {
  assert.equal(auth.validateCredentials('register', 'ad', 'long-enough-password', 'long-enough-password', 'setup-code'), '用户名至少需要 3 个字符');
  assert.equal(auth.validateCredentials('register', 'admin', 'short', 'short', 'setup-code'), '密码至少需要 12 个字符');
  assert.equal(auth.validateCredentials('register', 'admin', 'long-enough-password', 'different-password', 'setup-code'), '两次输入的密码不一致');
  assert.equal(auth.validateCredentials('register', 'admin', 'long-enough-password', 'long-enough-password'), '请输入初始化代码');
  assert.equal(auth.validateCredentials('register', 'admin', 'long-enough-password', 'long-enough-password', ' setup-code '), '');
  assert.equal(auth.validateCredentials('login', 'admin', 'short'), '');
});

test('resolves previews to an absolute dedicated origin', () => {
  const current = 'https://vibedock.example.test:8787/workspace/abc';
  assert.equal(
    auth.resolvePreviewOrigin(current, {preview_origin:'https://vibedock.example.test:9443', preview_port:9999}),
    'https://vibedock.example.test:9443',
  );
  assert.equal(auth.resolvePreviewOrigin(current, {preview_port:8788}), 'https://vibedock.example.test:8788');
  assert.equal(auth.resolvePreviewOrigin(current, {}), 'https://vibedock.example.test:8788');
  assert.equal(auth.resolvePreviewOrigin(current, {preview_origin:'/relative-is-not-an-origin'}), 'https://vibedock.example.test:8788');
  assert.equal(auth.resolvePreviewOrigin(current, {preview_origin:'javascript:alert(1)'}), 'https://vibedock.example.test:8788');
  assert.equal(auth.resolvePreviewOrigin(current, {preview_origin:'https://preview.example.test:9443'}), 'https://vibedock.example.test:8788');
  assert.equal(auth.resolvePreviewOrigin(current, {preview_origin:'http://vibedock.example.test:9443'}), 'https://vibedock.example.test:8788');
  assert.throws(
    () => auth.resolvePreviewOrigin(current, {preview_origin:'https://vibedock.example.test:8787'}),
    /must differ/,
  );
  assert.throws(() => auth.resolvePreviewOrigin('https://vibedock.example.test:8788/', {}), /must differ/);
});
