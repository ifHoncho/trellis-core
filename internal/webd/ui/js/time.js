// Time as the UI shows it: in the zone of the device looking at it when the
// browser knows one, else in the zone chosen in Settings → Appearance,
// else Zulu (UTC). The choice is kept in this browser.
import { state } from './state.js';

const KEY = 'trellis-timezone';
export const ZULU = 'UTC';

// deviceZone is the zone the browser reports for this device ("" when it
// reports none, or one it cannot format in).
export function deviceZone() {
  try {
    const z = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (!z) return '';
    new Intl.DateTimeFormat('en-GB', { timeZone: z });
    return z;
  } catch (e) { return ''; }
}

// zoneChoice is what the operator chose: '' (follow the device), 'UTC'
// (Zulu), or a zone's name.
export function zoneChoice() {
  try { return localStorage.getItem(KEY) || ''; } catch (e) { return ''; }
}
export function setZoneChoice(z) {
  try { if (z) localStorage.setItem(KEY, z); else localStorage.removeItem(KEY); } catch (e) { /* private mode */ }
  fmtCache.clear();
}

// zone is the zone in use.
export function zone() {
  const c = zoneChoice();
  if (c) {
    try { new Intl.DateTimeFormat('en-GB', { timeZone: c }); return c; } catch (e) { /* a zone this browser lost */ }
  }
  return deviceZone() || ZULU;
}
export const isZulu = (z = zone()) => z === 'UTC' || z === 'Etc/UTC' || z === 'GMT' || z === 'Etc/GMT';

// zones lists the zones this browser can show, for the setting.
export function zones() {
  try { return Intl.supportedValuesOf('timeZone'); } catch (e) { return []; }
}

const fmtCache = new Map();
function fmt(z, opts) {
  const k = z + JSON.stringify(opts);
  let f = fmtCache.get(k);
  if (!f) { f = new Intl.DateTimeFormat('en-GB', { timeZone: z, hourCycle: 'h23', ...opts }); fmtCache.set(k, f); }
  return f;
}
const valid = (d) => d instanceof Date && !Number.isNaN(d.getTime());

// zoneLabel is the zone's short name at that moment: "MST", "GMT+2", "Z".
export function zoneLabel(d = new Date(), z = zone()) {
  if (isZulu(z)) return 'Z';
  try {
    // US English has the short names people know ("CST"); elsewhere the
    // same call gives an offset ("GMT-6").
    const p = new Intl.DateTimeFormat('en-US', { timeZone: z, timeZoneName: 'short', hour: '2-digit' })
      .formatToParts(valid(d) ? d : new Date()).find((x) => x.type === 'timeZoneName');
    return p ? p.value : z;
  } catch (e) { return z; }
}

// clock is a time of day, 24-hour: "14:32:07", or "14:32:07Z" in Zulu.
// opts.seconds false leaves the seconds out; opts.zone true appends the
// zone's short name.
export function clock(d, opts = {}) {
  d = d instanceof Date ? d : new Date(d);
  if (!valid(d)) return '–';
  const z = opts.in || zone();
  const t = fmt(z, { hour: '2-digit', minute: '2-digit', ...(opts.seconds === false ? {} : { second: '2-digit' }) }).format(d);
  if (isZulu(z)) return t + 'Z';
  return opts.zone ? `${t} ${zoneLabel(d, z)}` : t;
}

// day is a date: "Thu 19 Nov 2026".
export function day(d, opts = {}) {
  d = d instanceof Date ? d : new Date(d);
  if (!valid(d)) return '–';
  return fmt(opts.in || zone(), { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' }).format(d).replace(',', '');
}

// stamp is a moment with its date when it is not today: "14:32:07", or
// "18 Nov 14:32".
export function stamp(d) {
  d = d instanceof Date ? d : new Date(d);
  if (!valid(d)) return '–';
  const z = zone();
  const ymd = (x) => fmt(z, { year: 'numeric', month: '2-digit', day: '2-digit' }).format(x);
  if (ymd(d) === ymd(nodeNow() || new Date())) return clock(d);
  return `${fmt(z, { day: 'numeric', month: 'short' }).format(d)} ${clock(d, { seconds: false })}`;
}

// nodeNow is the node's clock now: its time when it last reported, plus
// what has passed here since. Null before the first report.
export function nodeNow() {
  const st = state.status;
  const t = st && Date.parse(st.time);
  if (!t) return null;
  return new Date(t + (state.statusAt ? Date.now() - state.statusAt : 0));
}

// untilNextSecond is how long, in ms, until the node's clock starts its
// next second (the browser's before the first report).
export function untilNextSecond() {
  const n = nodeNow();
  const t = n ? n.getTime() : Date.now();
  return 1000 - (((t % 1000) + 1000) % 1000);
}
