// Editing configuration: a draft of the node config, and applying a
// change in two steps. Every change is tried first (dry run); when the
// node says it interrupts something, the operator sees exactly what
// before it is applied.
import { h } from './dom.js';
import { get, put, post } from './api.js';
import { state, redraw, toast, openDialog, closeDialog } from './state.js';
import { notice, qr } from './ui.js';

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

export const dirty = () => { const c = cfg(); return !!c.draft && canon(c.draft) !== c.base; };
export function resetDraft() {
  const c = cfg();
  if (state.config) c.draft = clone(state.config.node);
  c.fields = {};
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
  if (res.ap && res.ap.enabled) {
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
  if (!dry.changed) {
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
          { label: 'Cancel', run: () => closeDialog() },
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
    const res = await confirmAndApply((dry) => put('/config/node' + (dry ? '?dry_run=1' : ''), draft));
    if (res && res.applied) {
      toast('Saved. The node is applying it.');
      await loadConfig();
    }
  } catch (e) {
    c.fields = fieldErrors(e);
    if (!Object.keys(c.fields).length) toast(e.message, 'bad', 7000);
    else toast('Some settings were not accepted. They are marked.', 'bad');
  }
  c.busy = false;
  redraw();
}

// saveProfile installs a mesh profile on this node, and only on this node:
// nothing carries it to the others. That is the first thing the
// confirmation says, every time, because a profile only works when every
// node has the same one.
export async function saveProfile(input, opts = {}) {
  const others = ((state.status && state.status.nodes) || []).length;
  const res = await confirmAndApply((dry) => put('/profile' + (dry ? '?dry_run=1' : ''), input),
    { title: 'Change the profile on this node?', confirm: 'Change this node', always: true, said: ['this-node-only'],
      intro: others
        ? `This changes ${state.status.node.callsign} only. Nothing sends the profile to the other ${others === 1 ? 'node' : others + ' nodes'}: each keeps its own until you install this one there too.`
        : 'This changes this node only. Nothing sends the profile to other nodes: each needs it installed.',
      ...opts });
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
export async function saveMeshProfile(input, delay = 60) {
  const body = { ...input, delay_s: delay };
  const res = await confirmAndApply((dry) => post('/profile/mesh' + (dry ? '?dry_run=1' : ''), body),
    { title: 'Change the profile on every node?', confirm: 'Send to every node', always: true,
      intro: 'This node sends the profile to every node it knows; they all switch to it at the same moment.' });
  if (res && res.applied) {
    const at = res.apply_at ? new Date(res.apply_at).toLocaleTimeString() : 'shortly';
    toast(`Sent to ${(res.nodes || []).length + 1} nodes. Every node switches at ${at}.`, 'ok', 8000);
    await loadConfig();
  }
  return res;
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
      h('div.row.between',
        h('span.dim', 'You have changes that are not saved.'),
        h('div.row',
          h('button.btn', { onclick: resetDraft, disabled: c.busy }, 'Discard'),
          h('button.btn.primary', { onclick: saveNode, disabled: c.busy }, c.busy ? 'Checking…' : 'Review and save')))));
}
