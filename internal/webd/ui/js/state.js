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
// The keyboard goes into a dialog when it opens (Tab then walks its
// controls, Escape closes it) and back to where it was when it closes.
let focusBefore = null;
export function openDialog(d) {
  if (!state.dialog) focusBefore = document.activeElement;
  state.dialog = d;
  redraw();
  requestAnimationFrame(() => { const el = document.querySelector('.dialog'); if (el && !el.contains(document.activeElement)) el.focus(); });
}
export function closeDialog() {
  const d = state.dialog;
  state.dialog = null;
  if (d && d.onclose) d.onclose();
  redraw();
  if (!state.dialog && focusBefore && document.contains(focusBefore)) { try { focusBefore.focus(); } catch (e) { /* gone */ } }
  if (!state.dialog) focusBefore = null;
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

// Alerts the operator chose to ignore, in this browser, for this node (the
// storage is per origin, and every node is its own origin). They still
// show on the Alerts page; everywhere else they are left out. An alert is
// known by its level, source and code, so the same alert with a changing
// figure in its message stays ignored, and one that turns more serious
// shows again. One that has been gone for an hour is forgotten: when it
// comes back, it shows.
const IGNORED_KEY = 'trellis-ignored-alerts';
const IGNORED_FORGET = 3600e3;
let ignoredAlerts = (() => {
  try { return JSON.parse(localStorage.getItem(IGNORED_KEY) || '{}') || {}; } catch (e) { return {}; }
})();
function saveIgnored() {
  try { localStorage.setItem(IGNORED_KEY, JSON.stringify(ignoredAlerts)); } catch (e) { /* private mode */ }
}
export const alertKey = (w) => [w.level, w.source || '', w.code || w.message].join('|');
export const isIgnored = (w) => Object.prototype.hasOwnProperty.call(ignoredAlerts, alertKey(w));
export function ignoreAlert(w, on) {
  if (on) ignoredAlerts[alertKey(w)] = Date.now();
  else delete ignoredAlerts[alertKey(w)];
  saveIgnored();
  redraw();
}
// noteAlerts keeps ignored alerts that are still there, and forgets those
// gone for an hour. Called with every status.
export function noteAlerts(warnings) {
  const now = Date.now();
  const present = new Set((warnings || []).map(alertKey));
  let changed = false;
  for (const k of Object.keys(ignoredAlerts)) {
    if (present.has(k)) {
      if (now - ignoredAlerts[k] > 60e3) { ignoredAlerts[k] = now; changed = true; }
    } else if (now - ignoredAlerts[k] > IGNORED_FORGET) {
      delete ignoredAlerts[k]; changed = true;
    }
  }
  if (changed) saveIgnored();
}

// shownWarnings are the status's warnings less the ignored ones: what every
// page but Alerts goes by.
export function shownWarnings(st = state.status) {
  return (st && st.warnings || []).filter((w) => !isIgnored(w));
}

// warnings worth a badge: everything but "info", and not ignored.
export function alerts(st = state.status) {
  return shownWarnings(st).filter((w) => w.level !== 'info');
}
