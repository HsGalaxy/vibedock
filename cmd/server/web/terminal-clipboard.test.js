'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const clipboard = require('./terminal-clipboard.js');

test('recognizes browser text paste shortcuts without swallowing Codex image paste variants', () => {
  assert.equal(clipboard.isBrowserPasteShortcut({key:'v', ctrlKey:true}), true);
  assert.equal(clipboard.isBrowserPasteShortcut({key:'V', ctrlKey:true, shiftKey:true}), true);
  assert.equal(clipboard.isBrowserPasteShortcut({key:'v', metaKey:true}), true);
  assert.equal(clipboard.isBrowserPasteShortcut({key:'v', ctrlKey:true, altKey:true}), false);
  assert.equal(clipboard.isBrowserPasteShortcut({key:'c', ctrlKey:true}), false);
});

test('installs an xterm key handler that leaves browser paste to the paste event', () => {
  let handler;
  const frame = {
    isConnected: true,
    dataset: {},
    contentWindow: {term: {attachCustomKeyEventHandler(value) { handler = value; }}},
    addEventListener(_name, listener) { listener(); },
  };

  clipboard.attach(frame, () => assert.fail('an initialized terminal must not be polled'));

  assert.equal(frame.dataset.clipboardBridge, 'ready');
  assert.equal(handler({key:'v', ctrlKey:true}), false);
  assert.equal(handler({key:'c', ctrlKey:true}), true);
});

test('waits for ttyd to publish its xterm instance', () => {
  let handler;
  let retry;
  const frame = {
    isConnected: true,
    dataset: {},
    contentWindow: {},
    addEventListener(_name, listener) { listener(); },
  };

  clipboard.attach(frame, callback => { retry = callback; });
  assert.equal(typeof retry, 'function');
  frame.contentWindow.term = {attachCustomKeyEventHandler(value) { handler = value; }};
  retry();

  assert.equal(frame.dataset.clipboardBridge, 'ready');
  assert.equal(handler({key:'v', metaKey:true}), false);
});

test('can be registered before the iframe is inserted into the view layer', () => {
  let onLoad;
  let handler;
  const frame = {
    isConnected: false,
    dataset: {},
    contentWindow: {},
    addEventListener(_name, listener) { onLoad = listener; },
  };

  clipboard.attach(frame, () => assert.fail('the loaded terminal must attach immediately'));
  frame.isConnected = true;
  frame.contentWindow.term = {attachCustomKeyEventHandler(value) { handler = value; }};
  onLoad();

  assert.equal(frame.dataset.clipboardBridge, 'ready');
  assert.equal(handler({key:'v', ctrlKey:true}), false);
});
