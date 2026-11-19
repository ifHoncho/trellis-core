// Editing configuration: a draft of the node config, and applying a
// change in two steps. Every change is tried first (dry run); when the
// node says it interrupts something, the operator sees exactly what
// before it is applied.
import { h } from './dom.js';
import { get, put, post } from './api.js';
import { state, redraw, toast, openDialog, closeDialog } from './state.js';
import { notice, qr } from './ui.js';
import { clock } from './time.js';

const clone = (v) => JSON.parse(JSON.stringify(v));

export function cfg() {
  return (state.ui.cfg ||= { loading: false, error: null, draft: null, base: '', fields: {}, busy: false });
}

export async function loadConfig() {
  const c = cfg();
  c.loading = true;
  try {
    state.config = await get('/config');
    c.draft = clone(state.config.node);
    c.base = canon(c.draft);
    c.fields = {};
    c.error = null;
    c.stale = false;
  } catch (e) {
    c.error = e.message;
  }
  c.loading = false;
  redraw();
}

// ensureConfig starts a load when there is nothing yet; views call it on
// every draw and show a placeholder until draft exists.
export function ensureConfig() {
  const c = cfg();
  if (!c.draft && !c.loading && !c.error) loadConfig();
  return c;
}

// refreshConfig reloads the configuration when a page opens, unless there
// are edits not saved yet: what is shown is then what the node has, also
// after a change made elsewhere (meshctl, another browser).
export function refreshConfig() {
  const c = cfg();
  if (c.draft && !c.loading && !c.busy && !dirty()) loadConfig();
}

// followProfile reads the configuration again when the profile the node
// runs changes under it: a mesh-wide change reaches its moment a while
// after it was sent, and a change can come from another node, meshctl or
// another browser. Without this the editors would go on comparing their
// drafts with a profile the node no longer has. Edits to the node's own
// settings that are not saved yet are kept.
export function followProfile(st) {
  const c = cfg();
  const digest = st.profile && st.profile.digest;
  const node = st.node && st.node.config_digest;
  if (!digest) return;
  const moved = (c.seen && c.seen !== digest) || (c.seenNode && node && c.seenNode !== node);
  c.seen = digest;
  c.seenNode = node;
  if (moved && state.config) rereadConfig();
}

// rereadConfig reads the configuration again without losing edits to the
// node's own settings. When those settings changed on the node under the
// edits (another browser, meshctl), the edits stay on the page but can no
// longer be saved: the node would refuse them, since saving would undo
// that change. stale says so in the save bar.
export async function rereadConfig() {
  const c = cfg();
  try {
    const fresh = await get('/config');
    const was = state.config;
    const keep = dirty();
    state.config = fresh;
    if (!keep) { c.draft = clone(fresh.node); c.base = canon(c.draft); c.fields = {}; c.stale = false; }
    else if (was && was.node_digest !== fresh.node_digest) c.stale = true;
    redraw();
  } catch (e) { /* opening a page reads it again */ }
}

// conflict handles a change the node refused because it was made from a
// copy it has moved on from (409): says so, and reads the configuration
// again. True when e was such a refusal.
function conflict(e) {
  if (!e || e.status !== 409) return false;
  toast(e.message, 'bad', 12000);
  rereadConfig();
  return true;
}

// canon is a draft reduced to what a change means, for telling whether
// anything was edited. The node sends null for a list it has no entries
// in, and leaves out the roles that are automatic; an edit that is undone
// leaves an empty list or "auto" behind. Those are the same configuration.
function canon(draft) {
  const d = clone(draft);
  d.ports = d.ports || [];
  if (d.radios) {
    d.radios.devices = d.radios.devices || [];
    const roles = {};
    for (const k of Object.keys(d.radios.roles || {}).sort()) {
      if (d.radios.roles[k] !== 'auto') roles[k] = d.radios.roles[k];
    }
    d.radios.roles = roles;
  }
  return JSON.stringify(d);
}

// unsavedOn reports whether a page holds edits that are not saved or sent:
// the node's own settings (Settings, and Voice for its channels), a
// profile draft, the site link's members. Views note theirs in
// state.ui.unsaved as they draw.
export function unsavedOn(page) {
  const u = state.ui.unsaved || {};
  if (page === 'settings') return dirty() || !!u.profile;
  if (page === 'internet') return !!u.members;
  if (page === 'voice') return !!u.voice;
  return false;
}
export function noteUnsaved(what, on) { (state.ui.unsaved ||= {})[what] = !!on; }

export const dirty = () => { const c = cfg(); return !!c.draft && canon(c.draft) !== c.base; };
export function resetDraft() {
  const c = cfg();
  if (state.config) { c.draft = clone(state.config.node); c.base = canon(c.draft); }
  c.fields = {};
  c.stale = false;
  redraw();
}

function fieldErrors(e) {
  const out = {};
  for (const f of e.fields || []) out[f.field] = f.message;
  return out;
}

// effectsBody lists what a change does, and the Wi-Fi details that will
// be needed afterwards.
function effectsBody(res, intro, said = []) {
  const out = [];
  if (intro) out.push(h('p', intro));
  // An effect the introduction already states in words is not listed again.
  const effects = res.effects.filter((e) => !said.includes(e.code));
  if (effects.length) out.push(h('div.notices', effects.map((e) => notice(e))));
  else out.push(h('p.dim', 'Nothing running is interrupted.'));
  // The Wi-Fi details, only when the change touches the Wi-Fi: it comes up,
  // moves or restarts, gets new credentials (or a new name with the node),
  // or the node joins a mesh.
  const wifiCodes = ['ap-credentials', 'ap-band', 'ap-moves', 'ap-restart', 'ap-up', 'renamed', 'joins-mesh'];
  if (res.ap && res.ap.enabled && res.effects.some((e) => wifiCodes.includes(e.code))) {
    out.push(h('div', { style: { marginTop: '4px' } },
      h('div.label', { style: { marginBottom: '8px' } }, 'Afterwards, this node’s Wi-Fi is'),
      h('div.join',
        qr(res.ap.qr, 'Wi-Fi QR code'),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } },
          h('div', h('div.label', 'Network'), h('div.mono', { style: { fontSize: '17px' } }, res.ap.ssid)),
          h('div', h('div.label', 'Passphrase'), h('div.mono', { style: { fontSize: '17px', overflowWrap: 'anywhere' } }, res.ap.passphrase)),
          h('p.dim', { style: { fontSize: '13.5px' } }, 'Note these now if you are on this node’s Wi-Fi: you will have to join again.')))));
  }
  return out;
}

// confirmAndApply runs try(dry) twice: a dry run, a confirmation when the
// change has effects worth reading, then the real thing. It resolves to
// the result, or null when the operator backed out or nothing changed.
// Errors from the dry run are thrown (the caller shows field errors).
export async function confirmAndApply(send, opts = {}) {
  const dry = await send(true);
  dry.effects ||= [];
  // The profile in force, sent to cancel a change on its way, changes
  // nothing and is still sent.
  if (!dry.changed && !dry.effects.some((e) => e.code === 'mesh-change-cancelled')) {
    toast('Nothing to change: the node already runs this.');
    return null;
  }
  const mustAsk = opts.always || dry.ap || dry.effects.some((e) => e.level !== 'info');
  if (mustAsk) {
    const go = await new Promise((resolve) => {
      const worst = dry.effects.some((e) => e.level === 'error');
      openDialog({
        title: opts.title || 'Apply this change?',
        body: () => effectsBody(dry, opts.intro, opts.said),
        actions: [
          { label: opts.dismiss || 'Cancel', run: () => closeDialog() },
          { label: opts.confirm || 'Apply', kind: worst ? 'danger' : 'primary', run: () => { resolve(true); closeDialog(); } },
        ],
        modal: false,
        onclose: () => resolve(false),
      });
    });
    if (!go) return null;
  }
  return send(false);
}

export async function saveNode() {
  const c = cfg();
  if (c.busy) return;
  c.busy = true;
  c.fields = {};
  redraw();
  try {
    const draft = clone(c.draft);
    // The node refuses a save made from a copy it has moved on from.
    const base = encodeURIComponent(state.config.node_digest || '');
    const res = await confirmAndApply((dry) => put(`/config/node?base=${base}` + (dry ? '&dry_run=1' : ''), draft));
    if (res && res.applied) {
      toast('Saved. The node is applying it.');
      await loadConfig();
    }
  } catch (e) {
    if (conflict(e)) { /* said */ } else {
      c.fields = fieldErrors(e);
      if (!Object.keys(c.fields).length) toast(e.message, 'bad', 7000);
      else toast('Some settings were not accepted. They are marked.', 'bad');
    }
  }
  c.busy = false;
  redraw();
}

// withBase names the profile a change was made from, so that the node
// can refuse it when its profile is no longer that one (another node or
// browser changed it), or while another mesh-wide change is on its
// way. A change that would undo someone else's is never sent unseen.
function withBase(input) {
  return state.config && state.config.profile_digest ? { ...input, base: state.config.profile_digest } : input;
}

// refused runs a profile change; one the node refuses as made from an old
// copy, or while another change is on its way (409), is said here and the
// configuration read again. Other errors go back to the editor.
async function refused(run) {
  try { return await run(); } catch (e) { if (conflict(e)) return null; throw e; }
}

// saveProfile installs a mesh profile on this node, and only on this node:
// nothing carries it to the others. That is the first thing the
// confirmation says, every time, because a profile only works when every
// node has the same one.
export async function saveProfile(input, opts = {}) {
  const others = ((state.status && state.status.nodes) || []).length;
  input = withBase(input);
  const res = await refused(() => confirmAndApply((dry) => put('/profile' + (dry ? '?dry_run=1' : ''), input),
    { title: 'Change the profile on this node?', confirm: 'Change this node', always: true, said: ['this-node-only'],
      intro: others
        ? `This changes ${state.status.node.callsign} only. Nothing sends the profile to the other ${others === 1 ? 'node' : others + ' nodes'}: each keeps its own until you install this one there too.`
        : 'This changes this node only. Nothing sends the profile to other nodes: each needs it installed.',
      ...opts }));
  if (res && res.applied) {
    toast(others ? 'Profile installed on this node. The other nodes still have the old one.' : 'Profile installed on this node. It is applying it.', 'ok', 7000);
    await loadConfig();
  }
  return res;
}

// saveMeshProfile sends a profile from this node to every node of the mesh:
// all switch to it at one moment, delay seconds from now. The confirmation
// lists what that does, including nodes that cannot follow and regulatory
// modes it changes.
// what names the editor that sends it ('members', 'profile', 'voice'):
// that editor's draft is the one on its way, and no other's (sentPending).
export async function saveMeshProfile(input, delay = 60, what = '') {
  const instead = replacing();
  const body = { ...withBase(input), delay_s: delay };
  if (instead) body.replace = true;
  const res = await refused(() => confirmAndApply((dry) => post('/profile/mesh' + (dry ? '?dry_run=1' : ''), body),
    instead
      ? { title: 'Send this in the place of the change on its way?', confirm: 'Replace it', always: true,
        intro: 'This node sends this profile to every node it knows, and the change on its way is dropped; they all switch to this one at the same moment.' }
      : { title: 'Change the profile on every node?', confirm: 'Send to every node', always: true,
        intro: 'This node sends the profile to every node it knows; they all switch to it at the same moment.' }));
  if (res && res.applied) {
    const at = res.apply_at ? clock(res.apply_at) : 'shortly';
    const n = (res.nodes || []).length;
    state.ui.replacing = false;
    state.ui.sent = { what, revision: res.revision };
    toast(n ? `Sent to ${n} other node${n === 1 ? '' : 's'}. Every node switches at ${at}.` : `This node switches at ${at}.`, 'ok', 8000);
    // The profile in force is still the old one: the configuration is
    // read again when the node reports the new one (followProfile).
  }
  return res;
}

// pendingChange is the mesh-wide change on its way, if any: until every
// node has switched, the profile shown is the one about to be replaced.
export function pendingChange(st = state.status) {
  const mc = st && st.mesh_change;
  return mc && mc.state === 'pending' ? mc : null;
}

// A change on its way can be replaced, on purpose: by another change, or
// by the profile in force (which cancels it). replacing is the first,
// chosen on the banner: until it is sent or given up, the mesh-wide
// editors are open again and start from that change's profile
// (editProfile), so that what was right in it is kept and what was wrong
// is corrected.
// state.ui.replacing is the revision being replaced: another change
// that takes its place meanwhile ends it.
export function replacing(st = state.status) {
  const mc = pendingChange(st);
  return !!(mc && state.ui.replacing === mc.revision);
}

// changeLock is the change on its way that keeps the mesh-wide editors
// from sending; none while one is being replaced.
export function changeLock(st = state.status) {
  return replacing(st) ? null : pendingChange(st);
}

// editProfile is the profile the mesh-wide editors start from: the one in
// force, or the one on its way while it is being replaced.
export function editProfile() {
  return (replacing() && state.config.pending_profile) || state.config.profile;
}

// freshConfig reads the configuration for the banner's actions, which
// show on every page: on one that has not read it yet (Overview, Mesh),
// and again for the change's profile. False, and said, when it cannot be
// read.
async function freshConfig() {
  if (state.config) await rereadConfig(); else await loadConfig();
  if (state.config) return true;
  toast(`The node's configuration cannot be read${cfg().error ? ': ' + cfg().error : ''}. Nothing was sent.`, 'bad', 7000);
  return false;
}

export async function startReplacing() {
  // The change's profile comes with the configuration.
  if (!await freshConfig()) return;
  const mc = pendingChange();
  if (!mc || !state.config.pending_profile) { toast('No change is on its way any more.'); return; }
  state.ui.replacing = mc.revision;
  redraw();
}

export function stopReplacing() {
  state.ui.replacing = false;
  redraw();
}

// cancelPending sends the profile in force in the place of the change on
// its way: every node keeps what it runs.
export async function cancelPending() {
  try {
    if (!await freshConfig()) return;
    const body = { ...withBase({ profile: state.config.profile }), delay_s: 30, replace: true };
    const res = await refused(() => confirmAndApply((dry) => post('/profile/mesh' + (dry ? '?dry_run=1' : ''), body),
      { title: 'Cancel the change on its way?', confirm: 'Cancel the change', dismiss: 'Keep the change', always: true,
        intro: 'This node sends the profile the mesh runs now to every node it knows, in the place of the change on its way: at the switch nothing changes.' }));
    if (res && res.applied) {
      state.ui.replacing = false;
      state.ui.sent = { what: 'cancel', revision: res.revision };
      toast(`The change is cancelled: every node keeps its profile (as revision ${res.revision}) at ${res.apply_at ? clock(res.apply_at) : 'the switch'}.`, 'ok', 8000);
    }
  } catch (e) {
    toast(e.message.split('\n')[0], 'bad', 7000);
  }
}

// sentPending reports whether the mesh-wide change on its way is the one
// the editor `what` sent from this page. Only then is that editor's draft
// "sent": a change from another page, another browser or another node
// leaves it what it was, edits nobody has sent.
export function sentPending(what, st = state.status) {
  const mc = pendingChange(st);
  const s = state.ui.sent;
  return !!(mc && mc.sender && s && s.what === what && s.revision === mc.revision);
}

export async function identify(fingerprint) {
  try {
    await post('/radios/identify', { fingerprint, seconds: 10 });
    toast('Its LED blinks for 10 seconds.');
  } catch (e) {
    toast(e.message, 'bad', 6000);
  }
}

// The save bar every editing view ends with.
export function saveBar() {
  const c = cfg();
  if (!dirty()) return null;
  return h('div.panel.quiet', { style: { position: 'sticky', bottom: '12px', zIndex: '20' } },
    h('div.panel-body', { style: { padding: '12px 16px' } },
      h('div.row.between', { style: { flexWrap: 'wrap', gap: '10px' } },
        c.stale
          ? h('span.warn', { style: { maxWidth: '70ch' } }, 'These settings were changed on the node while you were editing (another browser, or meshctl). Saving yours would undo that: discard to see the node’s, then make your change again.')
          : h('span.dim', 'You have changes that are not saved.'),
        h('div.row',
          h('button.btn', { onclick: resetDraft, disabled: c.busy }, c.stale ? 'Discard and show the node’s' : 'Discard'),
          h('button.btn.primary', { onclick: saveNode, disabled: c.busy || c.stale }, c.busy ? 'Checking…' : 'Review and save')))));
}
