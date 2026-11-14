// The shell: session, routing, live data, and the frame around every view.
import { h, mount } from './dom.js';
import { get, post, connectEvents, setSignedOutHandler } from './api.js';
import { state, setRedraw, redraw, record, alerts, toast, closeDialog } from './state.js';
import { icon, brandMark, dot, button } from './ui.js';
import { duration } from './fmt.js';
import { overview } from './views/overview.js';
import { mesh } from './views/mesh.js';
import { internet } from './views/internet.js';
import { devices } from './views/devices.js';
import { radios } from './views/radios.js';
import { multicast } from './views/multicast.js';
import { alertsView } from './views/alerts.js';
import { settings } from './views/settings.js';
import { gate } from './views/gate.js';
import { refreshConfig } from './config.js';

const ROUTES = [
  { id: 'overview', label: 'Overview', icon: 'overview', view: overview, tab: true },
  { id: 'mesh', label: 'Mesh', icon: 'mesh', view: mesh, tab: true },
  { id: 'internet', label: 'Internet', icon: 'internet', view: internet, tab: true },
  { id: 'devices', label: 'Devices', icon: 'devices', view: devices, tab: true },
  { id: 'radios', label: 'Radios', icon: 'radios', view: radios },
  { id: 'multicast', label: 'Multicast', icon: 'multicast', view: multicast },
  { id: 'alerts', label: 'Alerts', icon: 'alerts', view: alertsView },
  { id: 'settings', label: 'Settings', icon: 'settings', view: settings },
];
const THEMES = ['dusk', 'day', 'night'];
const THEME_NAMES = { dusk: 'Dusk', day: 'Day', night: 'Night (red)' };

function setTheme(t) {
  state.theme = t;
  document.documentElement.setAttribute('data-theme', t);
  try { localStorage.setItem('trellis.theme', t); } catch (e) { /* private mode */ }
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', bg);
  redraw();
}
const nextTheme = () => setTheme(THEMES[(THEMES.indexOf(state.theme) + 1) % THEMES.length]);

function readRoute() {
  const id = (location.hash.replace(/^#\/?/, '').split('/')[0]) || 'overview';
  const [, sub] = location.hash.replace(/^#\/?/, '').split('/');
  state.route = ROUTES.some((r) => r.id === id) || id === 'more' ? id : 'overview';
  state.sub = sub || '';
}

// Live data -------------------------------------------------------------------

let stopEvents = null;
let topoTimer = null;

function onStatus(st) {
  state.status = st;
  state.statusAt = Date.now();
  state.problem = null;
  state.live = 'on';
  let total = 0;
  for (const n of st.batman.neighbors || []) {
    record('nbr:' + n.mac, n.throughput_mbps);
    total += n.throughput_mbps;
  }
  for (const l of [st.mesh_link, ...(st.more_links || [])]) for (const p of l.peers || []) record('sig:' + p.mac, p.signal_dbm);
  record('tput', total);
  record('rtt', st.gateway.wan_healthy ? st.gateway.rtt_ms || 0 : 0);
  record('mcast', st.batman.multicast.counters.tx + st.batman.multicast.counters.rx);
  record('clients', (st.ap.clients || []).length);
  if (state.session && st.setup.needed !== state.session.setup) refreshSession();
  redraw();
}

async function pollTopology() {
  clearTimeout(topoTimer);
  if (!state.session || !state.session.authenticated) return;
  const wanted = (state.route === 'overview' || state.route === 'mesh') && !document.hidden;
  let wait = 4000;
  if (wanted) {
    try {
      state.topology = await get('/topology');
      state.topologyAt = Date.now();
      wait = Math.max(4000, state.topology.interval_ms || 5000);
      redraw();
    } catch (e) { /* the next round tries again */ }
  }
  topoTimer = setTimeout(pollTopology, wait);
}

function startLive() {
  if (stopEvents) stopEvents();
  stopEvents = connectEvents({
    status: onStatus,
    problem: (msg) => { state.problem = msg; state.live = 'off'; redraw(); },
    signedOut: () => { signedOut(); },
    state: (up) => { if (!up) { state.live = 'off'; redraw(); } },
  });
  pollTopology();
}

function stopLive() {
  if (stopEvents) stopEvents();
  stopEvents = null;
  clearTimeout(topoTimer);
}

export async function refreshSession() {
  try {
    state.session = await get('/session');
  } catch (e) {
    state.session = state.session || { authenticated: false, admin_set: true, setup: false, callsign: '', unreachable: true };
    state.session.unreachable = true;
  }
  if (state.session.authenticated && !stopEvents) startLive();
  if (!state.session.authenticated) stopLive();
  redraw();
}

function signedOut() {
  stopLive();
  state.status = null;
  state.config = null;
  if (state.session) state.session.authenticated = false;
  redraw();
}

async function signOut() {
  try { await post('/logout'); } catch (e) { /* signed out either way */ }
  signedOut();
  toast('Signed out.');
}

// Frame -----------------------------------------------------------------------

function liveLine() {
  const age = state.statusAt ? (Date.now() - state.statusAt) / 1000 : null;
  let cls = state.live, text = 'live';
  if (state.live === 'on' && age != null && age > 7) { cls = 'stale'; text = `last report ${duration(age)} ago`; }
  if (state.live === 'off') text = state.problem ? 'node not reporting' : age != null ? `reconnecting · ${duration(age)} old` : 'connecting…';
  return h('div.live', { class: cls, role: 'status' }, h('i.pulse'), text);
}

function navLink(r, cls = '') {
  const n = r.id === 'alerts' ? alerts() : [];
  const bad = n.some((w) => w.level === 'error');
  return h('a', { href: '#/' + r.id, 'aria-current': state.route === r.id ? 'page' : null, class: cls },
    icon(r.icon), h('span', r.label), n.length ? h('span.count', { class: bad ? 'bad' : '' }, n.length) : null);
}

function rail() {
  const st = state.status;
  return h('aside.rail',
    h('div.brand', brandMark(), h('div', h('div.brand-name', 'TRELLIS'), h('div.brand-sub', 'mesh node'))),
    h('div.rail-node',
      h('div.callsign', st ? st.node.callsign : state.session.callsign || '…'),
      h('div.where', st ? `#${st.node.node_id} · ${st.node.address.split('/')[0]}` : ' ')),
    h('nav.nav', { 'aria-label': 'Sections' },
      ROUTES.slice(0, 6).map((r) => navLink(r)), h('div.nav-sep'), ROUTES.slice(6).map((r) => navLink(r))),
    h('div.rail-foot',
      liveLine(),
      h('div.rail-actions',
        h('button.btn.small', { onclick: nextTheme, title: 'Change theme: dusk, day, night' }, icon(state.theme), THEME_NAMES[state.theme]),
        h('button.btn.small.ghost', { onclick: signOut, title: 'Sign out' }, icon('out')))));
}

function topbar() {
  const st = state.status;
  return h('header.topbar',
    brandMark(),
    h('div.callsign', st ? st.node.callsign : state.session.callsign || 'Trellis'),
    liveLine(),
    h('button.btn.icon.ghost', { onclick: nextTheme, title: 'Change theme', 'aria-label': 'Change theme' }, icon(state.theme)));
}

function tabbar() {
  const n = alerts().length;
  const moreIds = ROUTES.filter((r) => !r.tab).map((r) => r.id);
  return h('nav.tabbar', { 'aria-label': 'Sections' },
    ROUTES.filter((r) => r.tab).map((r) => h('a', { href: '#/' + r.id, 'aria-current': state.route === r.id ? 'page' : null }, icon(r.icon), r.label)),
    h('a', { href: '#/more', 'aria-current': state.route === 'more' || moreIds.includes(state.route) ? 'page' : null },
      icon('more'), 'More', n ? h('span.count', n) : null));
}

// The "More" page on a phone: what does not fit the tab bar.
function more() {
  return h('div.page',
    h('header.page-head', h('div', h('h1', 'More'))),
    h('section.panel', h('div.panel-body.flush', h('nav.nav', ROUTES.filter((r) => !r.tab).map((r) => navLink(r))))),
    h('div.row', button(THEME_NAMES[state.theme] + ' theme', nextTheme, '', { icon: state.theme }), button('Sign out', signOut, '', { icon: 'out' })));
}

function banners() {
  const st = state.status;
  const out = [];
  if (state.problem) out.push(h('div.banner.bad', dot('bad'), h('span', h('b', 'No status: '), state.problem, '.')));
  if (st && st.regulatory.active_mode === 'unrestricted') {
    out.push(h('div.banner', dot('warn'), h('span', h('b', 'Unrestricted regulatory mode. '),
      'Radios use every channel and width they have, at their own full power, beyond licence-exempt limits; that needs frequency authorization. ',
      h('a', { href: '#/settings/profile' }, 'Switch to country mode'))));
  } else if (st && st.regulatory.requested_mode === 'unrestricted') {
    out.push(h('div.banner', dot('warn'), h('span', h('b', 'Unrestricted mode is not in force. '),
      `The radios keep ${st.regulatory.country}’s limits: ${st.regulatory.note || 'see Alerts'}. `,
      h('a', { href: '#/settings/profile' }, 'Mesh profile'))));
  }
  return out;
}

function dialog() {
  const d = state.dialog;
  if (!d) return null;
  return h('div.scrim', { onpointerdown: (e) => { if (e.target === e.currentTarget && !d.modal) closeDialog(); } },
    h('div.dialog', { role: 'dialog', 'aria-modal': 'true', 'aria-label': d.title },
      h('div.dialog-head', h('h2', d.title)),
      h('div.dialog-body', d.body ? d.body() : null),
      h('div.dialog-foot', (d.actions || []).map((a) => button(a.label, a.run, a.kind || '', { disabled: a.disabled && a.disabled() })))));
}

function toasts() {
  return h('div.toasts', { 'aria-live': 'polite' }, state.toasts.map((t) => h('div.toast', { class: t.tone, key: 't' + t.id }, t.message)));
}

function waiting() {
  return h('div.page', h('div.empty', state.problem ? 'The node is not reporting its status.' : 'Waiting for the node to report…'));
}

function app() {
  const s = state.session;
  let body;
  if (!s) {
    body = h('div.boot', 'Trellis');
  } else if (!s.authenticated || s.setup || (state.ui.setup && state.ui.setup.done)) {
    // The wizard's last page stays until the operator leaves it: it holds
    // the Wi-Fi details needed to come back to the node.

    body = gate({ refreshSession });
  } else {
    const r = ROUTES.find((x) => x.id === state.route);
    const needsStatus = state.route !== 'more';
    body = h('div.shell',
      rail(),
      h('div', topbar(), banners(),
        h('main.main', { key: state.route }, needsStatus && !state.status ? waiting() : state.route === 'more' ? more() : r.view()),
        tabbar()));
  }
  return h('div', { id: 'app-root' }, body, dialog(), toasts());
}

// Boot ------------------------------------------------------------------------

state.theme = document.documentElement.getAttribute('data-theme') || 'dusk';
readRoute();
setSignedOutHandler(signedOut);
setRedraw(mount(document.getElementById('app'), app));
window.addEventListener('hashchange', () => {
  readRoute();
  refreshConfig();
  window.scrollTo(0, 0);
  pollTopology();
  redraw();
});
document.addEventListener('visibilitychange', () => { if (!document.hidden) pollTopology(); });
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && state.dialog && !state.dialog.modal) closeDialog();
});
// Ages ("last report 9 s ago") move on even when nothing arrives.
setInterval(() => { if (state.session && state.session.authenticated) redraw(); }, 1000);
refreshSession();
