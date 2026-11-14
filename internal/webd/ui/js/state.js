// What the UI knows, in one place. Views read it; redraw() repaints.

export const state = {
  session: null,     // /session: authenticated, admin_set, setup, callsign
  status: null,      // the node's last status document
  statusAt: 0,       // when it arrived (ms)
  live: 'off',       // on, stale or off: the event stream
  problem: null,     // why there is no status ("meshd is not running…")
  topology: null,
  topologyAt: 0,
  config: null,      // /config, fetched when a settings view opens
  route: 'overview',
  theme: 'dusk',
  dialog: null,
  toasts: [],
  ui: {},            // per-view state: form drafts, selections
  hist: {},          // short histories for sparklines
};

let redrawFn = () => {};
export function setRedraw(fn) { redrawFn = fn; }
export function redraw() { redrawFn(); }

let toastId = 0;
export function toast(message, tone = 'ok', ms = 4500) {
  const t = { id: ++toastId, message, tone };
  state.toasts.push(t);
  redraw();
  setTimeout(() => {
    state.toasts = state.toasts.filter((x) => x !== t);
    redraw();
  }, ms);
}

// dialog: {title, body: () => Node[], actions: [{label, kind, run}]}
export function openDialog(d) { state.dialog = d; redraw(); }
export function closeDialog() {
  const d = state.dialog;
  state.dialog = null;
  if (d && d.onclose) d.onclose();
  redraw();
}

const HIST = 90; // three minutes at one sample every two seconds
export function record(key, value) {
  const h = (state.hist[key] ||= []);
  h.push(value);
  if (h.length > HIST) h.shift();
}
export const history = (key) => state.hist[key] || [];

// names maps every address a node goes by (identity MAC, radio MACs) to
// its callsign, from gossip.
export function names(st = state.status) {
  const m = new Map();
  if (!st) return m;
  for (const n of st.nodes || []) {
    m.set(n.mac, n.callsign);
    for (const o of n.originators || []) m.set(o, n.callsign);
  }
  return m;
}
export function nodeByAddr(mac, st = state.status) {
  if (!st) return null;
  return (st.nodes || []).find((n) => n.mac === mac || (n.originators || []).includes(mac)) || null;
}
export const shortMac = (mac) => (mac ? mac.slice(-8) : '–');
export function nameOf(mac, st = state.status) {
  return names(st).get(mac) || shortMac(mac);
}

// warnings worth a badge: everything but "info".
export function alerts(st = state.status) {
  return (st && st.warnings || []).filter((w) => w.level !== 'info');
}
