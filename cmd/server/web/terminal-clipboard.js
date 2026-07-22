(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.VibeDockTerminalClipboard = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict';

  function isBrowserPasteShortcut(event) {
    if (!event || event.altKey) return false;
    const key = String(event.key || '').toLowerCase();
    return key === 'v' && (event.ctrlKey || event.metaKey);
  }

  function attach(frame, schedule = setTimeout) {
    let stopped = false;
    let attempts = 0;

    const tryAttach = () => {
      if (stopped || !frame?.isConnected) return;
      try {
        const terminal = frame.contentWindow?.term;
        if (terminal && typeof terminal.attachCustomKeyEventHandler === 'function') {
          terminal.attachCustomKeyEventHandler(event => !isBrowserPasteShortcut(event));
          frame.dataset.clipboardBridge = 'ready';
          return;
        }
      } catch (_) {
        // Session terminals are served from the same origin. Stop cleanly if a
        // deployment puts them on another origin instead of polling forever.
        return;
      }
      attempts += 1;
      if (attempts < 200) schedule(tryAttach, 50);
    };

    frame.addEventListener('load', tryAttach, {once: true});
    return () => { stopped = true; };
  }

  return {attach, isBrowserPasteShortcut};
});
