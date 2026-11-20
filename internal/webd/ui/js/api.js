// The node's API (docs/api.md), as the UI uses it.

const BASE = '/api/v1';
export const apiBase = BASE;

export class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || `HTTP ${status}`);
    this.status = status;
    this.fields = (body && body.fields) || [];
    this.retryAfter = (body && body.retry_after_s) || 0;
  }
}

// onSignedOut is called when a request finds the session gone.
let onSignedOut = () => {};
export function setSignedOutHandler(fn) { onSignedOut = fn; }

export async function api(method, path, body) {
  const init = { method, headers: {}, credentials: 'same-origin', cache: 'no-store' };
  if (method !== 'GET') init.headers['X-Trellis-Request'] = '1';
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(BASE + path, init);
  } catch (e) {
    throw new ApiError(0, { error: 'No response from node. Check the connection.' });
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* no body */ }
  if (!res.ok) {
    if (res.status === 401 && path !== '/login' && path !== '/password') onSignedOut();
    throw new ApiError(res.status, data);
  }
  return data;
}

// upload sends a file as the request body, as it is (a release package).
export async function upload(path, file) {
  let res;
  try {
    res = await fetch(BASE + path, {
      method: 'POST', credentials: 'same-origin', cache: 'no-store', body: file,
      headers: { 'X-Trellis-Request': '1', 'Content-Type': 'application/vnd.debian.binary-package' },
    });
  } catch (e) {
    throw new ApiError(0, { error: 'No response from node. Check the connection.' });
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* no body */ }
  if (!res.ok) {
    if (res.status === 401) onSignedOut();
    throw new ApiError(res.status, data);
  }
  return data;
}

export const get = (path) => api('GET', path);
export const put = (path, body) => api('PUT', path, body);
export const post = (path, body) => api('POST', path, body ?? {});

// Live status. handlers: {status(doc), problem(message), signedOut(), state(up)}
export function connectEvents(handlers) {
  let es = null;
  let closed = false;
  let retry = null;
  const open = () => {
    if (closed) return;
    es = new EventSource(BASE + '/events');
    es.addEventListener('open', () => handlers.state(true));
    es.addEventListener('status', (e) => {
      try { handlers.status(JSON.parse(e.data)); } catch (err) { /* a truncated event: the next one follows */ }
    });
    es.addEventListener('problem', (e) => {
      try { handlers.problem(JSON.parse(e.data).error); } catch (err) { /* ignore */ }
    });
    es.addEventListener('signed-out', () => { close(); handlers.signedOut(); });
    es.addEventListener('error', () => {
      handlers.state(false);
      // The browser retries by itself while the stream is merely
      // interrupted. A closed one (401 after a restart) needs a look.
      if (es.readyState === EventSource.CLOSED) {
        es.close();
        clearTimeout(retry);
        retry = setTimeout(async () => {
          try {
            const s = await get('/session');
            if (!s.authenticated) { close(); handlers.signedOut(); return; }
          } catch (err) { /* still away */ }
          open();
        }, 2500);
      }
    });
  };
  const close = () => {
    closed = true;
    clearTimeout(retry);
    if (es) es.close();
  };
  open();
  return close;
}
