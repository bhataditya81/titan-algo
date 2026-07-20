// api.js — REST + WebSocket transport. Every function fails soft: network
// errors, non-2xx responses, and bad JSON all resolve to {ok:false, error}
// rather than throwing, so callers never need try/catch.

const Api = (() => {
  const TOKEN_KEY = 'titan_api_token';
  const BASE_URL_KEY = 'titan_base_url';
  const DEFAULT_BASE_URL = 'http://127.0.0.1:8080';

  function getToken() {
    return localStorage.getItem(TOKEN_KEY) || '';
  }
  function setToken(token) {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  }
  function getBaseUrl() {
    return localStorage.getItem(BASE_URL_KEY) || DEFAULT_BASE_URL;
  }
  function setBaseUrl(url) {
    localStorage.setItem(BASE_URL_KEY, url || DEFAULT_BASE_URL);
  }

  async function request(method, path, body) {
    const url = getBaseUrl().replace(/\/$/, '') + path;
    const headers = { 'X-API-Key': getToken() };
    if (body !== undefined) headers['Content-Type'] = 'application/json';

    let res;
    try {
      res = await fetch(url, {
        method,
        headers,
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
    } catch (err) {
      return { ok: false, error: 'network error: ' + err.message };
    }

    let data = null;
    try {
      const text = await res.text();
      data = text ? JSON.parse(text) : null;
    } catch (_) {
      // non-JSON body; leave data null, fall through to status check
    }

    if (!res.ok) {
      const msg = (data && data.error) || `HTTP ${res.status}`;
      return { ok: false, status: res.status, error: msg, data };
    }
    return { ok: true, status: res.status, data };
  }

  const get = (path) => request('GET', path);
  const post = (path, body) => request('POST', path, body === undefined ? {} : body);

  // --- WebSocket -----------------------------------------------------------
  // Wraps ws/live with auto no-retry-loop semantics: caller decides when to
  // reconnect (manual button per spec), we just expose connect/close/onmessage.
  function connectWs({ onOpen, onMessage, onClose, onError }) {
    const httpBase = getBaseUrl().replace(/\/$/, '');
    const wsBase = httpBase.replace(/^http/, 'ws');
    const token = encodeURIComponent(getToken());
    let socket;
    try {
      socket = new WebSocket(`${wsBase}/ws/live?token=${token}`);
    } catch (err) {
      onError && onError(err);
      return null;
    }

    socket.addEventListener('open', () => onOpen && onOpen());
    socket.addEventListener('close', (ev) => onClose && onClose(ev));
    socket.addEventListener('error', (ev) => onError && onError(ev));
    socket.addEventListener('message', (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (_) {
        return; // ignore malformed frames, never throw
      }
      onMessage && onMessage(msg);
    });

    return socket;
  }

  return {
    DEFAULT_BASE_URL,
    getToken, setToken,
    getBaseUrl, setBaseUrl,
    get, post,
    connectWs,
  };
})();
