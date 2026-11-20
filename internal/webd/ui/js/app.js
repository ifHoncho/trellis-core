// The shell: session, routing, live data, and the frame around every view.
import { SLOTS, SLOT_ICONS, themeName, slotOf, slotTheme, nextTheme } from './themes.js';
import { h, mount } from './dom.js';
import { get, post, connectEvents, setSignedOutHandler } from './api.js';
import { state, setRedraw, redraw, record, alerts, toast, closeDialog, noteAlerts } from './state.js';
import { icon, brandMark, dot, button } from './ui.js';
import { duration } from './fmt.js';
import { overview } from './views/overview.js';
import { recordHost } from './views/hardware.js';
import { mesh } from './views/mesh.js';
import { internet } from './views/internet.js';
import { devices } from './views/devices.js';
import { radios } from './views/radios.js';
import { multicast } from './views/multicast.js';
import { voice } from './views/voice.js';
import { alertsView } from './views/alerts.js';
import { settings } from './views/settings.js';
import { gate } from './views/gate.js';
import { refreshConfig, followProfile, unsavedOn, pendingChange, replacing, startReplacing, stopReplacing, cancelPending } from './config.js';
import { clock, untilNextSecond } from './time.js';

const ROUTES = [
  { id: 'overview', label: 'Overview', icon: 'overview', view: overview, tab: true },
  { id: 'mesh', label: 'Mesh', icon: 'mesh', view: mesh, tab: true },
  { id: 'internet', label: 'Internet', icon: 'internet', view: internet, tab: true },
  { id: 'devices', label: 'Devices', icon: 'devices', view: devices, tab: true },
  { id: 'radios', label: 'Radios', icon: 'radios', view: radios },
  { id: 'multicast', label: 'Multicast', icon: 'multicast', view: multicast },
  { id: 'voice', label: 'Voice', icon: 'voice', view: voice },
  { id: 'alerts', label: 'Alerts', icon: 'alerts', view: alertsView },
  { id: 'settings', label: 'Settings', icon: 'settings', view: settings },
];
const themeIcon = () => SLOT_ICONS[slotOf(state.theme)];
const switchTitle = () => `Theme: ${SLOTS.map((s) => themeName(slotTheme(s))).join(' / ')} (Settings → Appearance)`;

function readRoute() {
  const id = (location.hash.replace(/^#\/?/, '').split('/')[0]) || 'overview';
  const [, sub] = location.hash.replace(/^#\/?/, '').split('/');
  state.route = ROUTES.some((r) => r.id === id) || id === 'more' ? id : 'overview';
  state.sub = sub || '';
}

// Live data -------------------------------------------------------------------

let stopEvents = null;
let topoTimer = null;
let voiceTimer = null;

// pollVoice asks for the voice state every second while the Voice page is
// open: who is talking changes faster than the status stream.
async function pollVoice() {
  clearTimeout(voiceTimer);
  if (!state.session || !state.session.authenticated) return;
  if (state.route === 'voice' && !document.hidden) {
    try { state.voice = await get('/voice'); redraw(); } catch (e) { /* tried again */ }
  }
  voiceTimer = setTimeout(pollVoice, state.route === 'voice' ? 1000 : 3000);
}

function onStatus(st) {
  state.status = st;
  followProfile(st);
  noteAlerts(st.warnings);
  if (st.regulatory.active_mode !== 'unrestricted' && regDismissed(st)) setRegDismissed(st, false);
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
  recordHost(st, record);
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
  pollVoice();
}

function stopLive() {
  if (stopEvents) stopEvents();
  stopEvents = null;
  clearTimeout(topoTimer);
  clearTimeout(voiceTimer);
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
  if (state.live === 'off') text = state.problem ? 'Node not reporting' : age != null ? `Reconnecting · data ${duration(age)} old` : 'connecting…';
  return h('div.live', { class: cls, role: 'status' }, h('i.pulse'), text);
}

function navLink(r, cls = '') {
  const n = r.id === 'alerts' ? alerts() : [];
  const bad = n.some((w) => w.level === 'error');
  return h('a', { href: '#/' + r.id, 'aria-current': state.route === r.id ? 'page' : null, class: cls },
    icon(r.icon), h('span', r.label), n.length ? h('span.count', { class: bad ? 'bad' : '' }, n.length) : null,
    // Edits on that page that are not saved or sent yet.
    unsavedOn(r.id) ? h('span.unsaved', { title: 'Unsaved changes', role: 'img', 'aria-label': 'unsaved changes' }) : null);
}

// setTitle names the browser tab after the node and the page, with the
// alerts that need attention in front: several nodes in several tabs stay
// apart.
function setTitle() {
  const st = state.status;
  const who = st ? st.node.callsign : (state.session && state.session.callsign) || '';
  const r = ROUTES.find((x) => x.id === state.route);
  const n = st && state.session && state.session.authenticated ? alerts().length : 0;
  const t = [who, r ? r.label : '', 'Trellis'].filter(Boolean).join(' · ');
  const title = (n ? `(${n}) ` : '') + t;
  if (document.title !== title) document.title = title;
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
        h('button.btn.small.theme-switch', { onclick: nextTheme, title: switchTitle() }, icon(themeIcon()), h('span', themeName(state.theme))),
        h('button.btn.small.ghost', { onclick: signOut, title: 'Sign out' }, icon('out')))));
}

function topbar() {
  const st = state.status;
  return h('header.topbar',
    brandMark(),
    h('div.callsign', st ? st.node.callsign : state.session.callsign || 'Trellis'),
    liveLine(),
    regMini(),
    h('button.btn.icon.ghost', { onclick: nextTheme, title: switchTitle(), 'aria-label': 'Change theme' }, icon(themeIcon())));
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
    h('div.row', button(themeName(state.theme) + ' theme', nextTheme, '', { icon: themeIcon() }), button('Sign out', signOut, '', { icon: 'out' })));
}

// The unrestricted-mode banner can be dismissed, in this browser, for this
// mesh: it shrinks to a triangle marked U in the corner, which shows the
// warning on hover or a tap. It comes back when the node leaves
// unrestricted mode and later enters it again.
const REG_KEY = 'trellis-unrestricted-dismissed';
const REG_TEXT = 'All channels and widths at full hardware power, without licence-exempt limits. Requires frequency authorization. ';
function regDismissed(st) {
  try { return localStorage.getItem(REG_KEY) === st.profile.name; } catch (e) { return false; }
}
function setRegDismissed(st, on) {
  try {
    if (on) localStorage.setItem(REG_KEY, st.profile.name);
    else localStorage.removeItem(REG_KEY);
  } catch (e) { /* private mode: it shows */ }
  state.ui.regPop = false;
  redraw();
}

// regMini is the dismissed banner: the marked triangle, and the warning
// beside it while the pointer is on it, it has focus, or it was tapped.
function regMini() {
  const st = state.status;
  if (!st || st.regulatory.active_mode !== 'unrestricted' || !regDismissed(st)) return null;
  const open = !!state.ui.regPop;
  return h('div.reg-mini', { class: open ? 'open' : '' },
    h('button.reg-tri', {
      type: 'button', 'aria-expanded': String(open), 'aria-label': 'Unrestricted regulatory mode: show banner',
      onclick: () => { state.ui.regPop = !open; redraw(); },
    }, h('span.tri', h('span', 'U'))),
    h('div.reg-pop', { role: 'note' },
      h('b', 'Unrestricted regulatory mode. '), REG_TEXT,
      h('div.reg-pop-foot',
        h('a', { href: '#/settings/profile' }, 'Switch to country mode'),
        button('Show banner', () => setRegDismissed(st, false), 'small ghost'))));
}

function banners() {
  const st = state.status;
  const out = [];
  if (state.problem) out.push(h('div.banner.bad', dot('bad'), h('span', h('b', 'No status: '), state.problem, '.')));
  // A mesh-wide change on its way shows on every page: until the switch,
  // no other change is sent to every node (it would replace this one).
  const mc = pendingChange(st);
  if (mc) {
    const secs = Math.max(0, Math.round((Date.parse(mc.apply_at) - Date.now()) / 1000));
    const cancelled = state.ui.sent && state.ui.sent.what === 'cancel' && state.ui.sent.revision === mc.revision && mc.sender;
    out.push(h('div.banner', dot('warn'), h('span', h('b', cancelled ? 'Mesh change cancelled. ' : 'Mesh change pending. '),
      cancelled
        ? `All nodes keep the current profile as rev ${mc.revision} at ${clock(mc.apply_at)} (in ${duration(secs)}). `
        : `Profile rev ${mc.revision} from ${mc.sender ? 'this node' : mc.from}: switchover at ${clock(mc.apply_at)} (in ${duration(secs)}). `,
      replacing(st)
        ? h('span', 'Replacing: the editors show the pending profile. Edit and send to supersede it. ')
        : h('a', { href: '#/settings/profile' }, 'Status')),
    // A wrong change can be put right before it lands: replaced by a
    // corrected one, or cancelled.
    cancelled ? null : replacing(st)
      ? h('span.banner-actions', h('button.btn.small.ghost', { type: 'button', onclick: stopReplacing, title: 'Keep the pending change' }, 'Keep'))
      : h('span.banner-actions',
        h('button.btn.small.ghost', { type: 'button', onclick: startReplacing, title: 'Edit the pending profile and send a replacement' }, 'Replace…'),
        h('button.btn.small.ghost', { type: 'button', onclick: cancelPending, title: 'All nodes keep the current profile' }, 'Cancel…'))));
  }
  if (st && st.regulatory.active_mode === 'unrestricted') {
    if (!regDismissed(st)) {
      out.push(h('div.banner', dot('warn'), h('span', h('b', 'Unrestricted regulatory mode. '), REG_TEXT,
        h('a', { href: '#/settings/profile' }, 'Switch to country mode')),
      h('button.btn.small.ghost.dismiss', { type: 'button', onclick: () => setRegDismissed(st, true), title: 'Minimize to corner marker (this browser)' }, 'Dismiss')));
    }
  } else if (st && st.regulatory.requested_mode === 'unrestricted') {
    out.push(h('div.banner', dot('warn'), h('span', h('b', 'Unrestricted mode not active. '),
      `Radios limited to ${st.regulatory.country} rules: ${st.regulatory.note || 'see Alerts'}. `,
      h('a', { href: '#/settings/profile' }, 'Mesh profile'))));
  }
  return out;
}

function dialog() {
  const d = state.dialog;
  if (!d) return null;
  return h('div.scrim', { onpointerdown: (e) => { if (e.target === e.currentTarget && !d.modal) closeDialog(); } },
    h('div.dialog', { role: 'dialog', 'aria-modal': 'true', 'aria-label': d.title, tabindex: '-1' },
      h('div.dialog-head', h('h2', d.title)),
      h('div.dialog-body', d.body ? d.body() : null),
      h('div.dialog-foot', (d.actions || []).map((a) => button(a.label, a.run, a.kind || '', { disabled: a.disabled && a.disabled() })))));
}

function toasts() {
  return h('div.toasts', { 'aria-live': 'polite' }, state.toasts.map((t) => h('div.toast', { class: t.tone, key: 't' + t.id }, t.message)));
}

function waiting() {
  return h('div.page', h('div.empty', state.problem ? 'Node not reporting status.' : 'Waiting for node status…'));
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
      h('div', topbar(), banners(), h('div.reg-corner', regMini()),
        h('main.main', { key: state.route }, needsStatus && !state.status ? waiting() : state.route === 'more' ? more() : r.view()),
        tabbar()));
  }
  setTitle();
  return h('div', { id: 'app-root' }, body, dialog(), toasts());
}

// Boot ------------------------------------------------------------------------

state.theme = document.documentElement.getAttribute('data-theme') || 'dusk';
readRoute();
setSignedOutHandler(signedOut);
setRedraw(mount(document.getElementById('app'), app));
// The unrestricted-mode warning opened by a tap closes on a tap anywhere
// else, and on going to another page.
document.addEventListener('pointerdown', (e) => {
  if (state.ui.regPop && !(e.target.closest && e.target.closest('.reg-mini'))) { state.ui.regPop = false; redraw(); }
});
window.addEventListener('hashchange', () => {
  state.ui.regPop = false;
  readRoute();
  refreshConfig();
  window.scrollTo(0, 0);
  pollTopology();
  pollVoice();
  redraw();
});
document.addEventListener('visibilitychange', () => { if (!document.hidden) pollTopology(); });
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && state.dialog && !state.dialog.modal) closeDialog();
  // Tab stays inside an open dialog.
  if (e.key === 'Tab' && state.dialog) {
    const box = document.querySelector('.dialog');
    if (!box) return;
    const els = [...box.querySelectorAll('button, a[href], input, select, textarea')].filter((x) => !x.disabled && x.offsetParent !== null);
    if (!els.length) { e.preventDefault(); return; }
    const first = els[0], last = els[els.length - 1];
    if (!box.contains(document.activeElement) || document.activeElement === box) { e.preventDefault(); (e.shiftKey ? last : first).focus(); }
    else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
});
// Leaving the page with changes not saved or not sent: the browser asks.
window.addEventListener('beforeunload', (e) => {
  if (unsavedOn('settings') || unsavedOn('internet') || unsavedOn('voice')) { e.preventDefault(); e.returnValue = ''; }
});
// Once a second, just after the node's clock starts a new one: the
// clock on Overview and Appearance moves on with it, whatever else
// redraws in between, and ages ("last report 9 s ago") move on even when
// nothing arrives. Aimed again each time, so a status that moves the
// node's clock moves the timer with it.
function tick() {
  if (state.session && state.session.authenticated) redraw();
  setTimeout(tick, untilNextSecond() + 8);
}
setTimeout(tick, untilNextSecond() + 8);
refreshSession();
