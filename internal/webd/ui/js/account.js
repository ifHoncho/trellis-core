// The operator account: the Linux user trellis, its SSH keys and
// password. Used by the setup wizard's Access step and Settings → Access.
import { h } from './dom.js';
import { get, put, post } from './api.js';
import { state, redraw, toast } from './state.js';
import { panel, kv, field, toggle, button, notice } from './ui.js';

export const ACCOUNT = 'trellis';

// Key types OpenSSH takes for login (internal/account: keyTypes).
const KEY_TYPES = new Set(['ssh-ed25519', 'ssh-rsa', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com', 'sk-ecdsa-sha2-nistp256@openssh.com']);

// keyFromLine reads one public key line and its SHA-256 fingerprint, as
// ssh-keygen -l shows it. Throws with a short reason.
export async function keyFromLine(line) {
  const parts = line.trim().split(/\s+/);
  const at = parts.findIndex((p) => KEY_TYPES.has(p));
  if (at < 0 || at + 1 >= parts.length) throw new Error('Not an SSH public key (ssh-ed25519, ssh-rsa, ecdsa-sha2-…).');
  const type = parts[at], data = parts[at + 1], comment = parts.slice(at + 2).join(' ');
  let blob;
  try { blob = Uint8Array.from(atob(data), (c) => c.charCodeAt(0)); } catch (e) { throw new Error(`${type} key: not base64.`); }
  const n = blob.length >= 4 ? (blob[0] << 24 | blob[1] << 16 | blob[2] << 8 | blob[3]) >>> 0 : -1;
  if (n < 0 || 4 + n > blob.length || new TextDecoder().decode(blob.slice(4, 4 + n)) !== type) throw new Error(`${type} key: data is not a ${type} key.`);
  const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', blob));
  const fingerprint = 'SHA256:' + btoa(String.fromCharCode(...sum)).replace(/=+$/, '');
  return { type, data, comment, fingerprint, line: line.trim() };
}

// keyLine is a key as an authorized_keys line (the API's account.keys).
export function keyLine(k) {
  return k.line || [k.options, k.type, k.data, k.comment].filter(Boolean).join(' ');
}

function addKeys(m, keys) {
  let added = 0;
  for (const k of keys) {
    if (!m.keys.some((x) => x.data === k.data)) { m.keys.push(k); added++; }
  }
  return added;
}

// passwordAuthToggle: sshd's password authentication for the account, on
// by default (m.keyOnly is its opposite, as the API has it). It can be
// turned off only once the account has a public key.
export function passwordAuthToggle(m) {
  const locked = !m.keys.length && !m.keyOnly;
  return toggle('Password authentication', locked ? 'Off: requires a public key' : null, !m.keyOnly,
    (v) => { m.keyOnly = !v; redraw(); }, { disabled: locked });
}

// keysEditor: the keys in m.keys, removable; a pasted key; a GitHub
// user's keys, fetched by the node.
export function keysEditor(m) {
  const paste = async () => {
    m.error = '';
    const lines = m.paste.split('\n').map((l) => l.trim()).filter((l) => l && !l.startsWith('#'));
    try {
      const keys = [];
      for (const l of lines) keys.push(await keyFromLine(l));
      addKeys(m, keys);
      m.paste = '';
    } catch (e) { m.error = e.message; }
    redraw();
  };
  const github = async () => {
    m.busy = true; m.error = ''; redraw();
    try {
      const r = await post('/account/github', { user: m.gh.trim() });
      const n = addKeys(m, (r.keys || []).map((k) => ({ ...k, line: keyLine(k) })));
      toast(n ? `GitHub ${m.gh.trim()}: ${n} key${n === 1 ? '' : 's'} imported.` : `GitHub ${m.gh.trim()}: no new keys.`);
      m.gh = '';
    } catch (e) { m.error = e.message; }
    m.busy = false; redraw();
  };
  return h('div.form',
    h('div.label', 'SSH keys'),
    m.keys.length
      ? h('div.key-list', m.keys.map((k, i) => h('div.key-row',
        h('div', h('div.mono', k.fingerprint), h('div.faint', [k.type, k.comment].filter(Boolean).join(' · '))),
        // Password authentication is off only with a key: the last one
        // gone turns it back on.
        button('Remove', () => { m.keys.splice(i, 1); if (!m.keys.length) m.keyOnly = false; redraw(); }, 'small'))))
      : h('p.faint', 'None'),
    field('Public key', h('textarea.textarea', { value: m.paste, placeholder: 'ssh-ed25519 AAAA… user@host', spellcheck: 'false', autocapitalize: 'none',
      oninput: (e) => { m.paste = e.target.value; redraw(); } })),
    h('div.row.end', button('Add key', paste, 'small', { disabled: !m.paste.trim() })),
    field('GitHub user', h('div.input-action',
      h('input.input', { value: m.gh, placeholder: 'username', maxlength: '39', autocapitalize: 'none', spellcheck: 'false',
        oninput: (e) => { m.gh = e.target.value; redraw(); },
        onkeydown: (e) => { if (e.key === 'Enter') { e.preventDefault(); if (m.gh.trim() && !m.busy) github(); } } }),
      button(m.busy ? 'Importing…' : 'Import', github, '', { disabled: m.busy || !m.gh.trim() })),
    { hint: 'Source: github.com/USER.keys. Uplink required.' }),
    m.error ? notice({ level: 'error', message: m.error }) : null);
}

// accountPanel: Settings → Access. Its own state; saved on its own.
export function accountPanel() {
  const ui = (state.ui.account ||= { st: null, loading: false, keys: [], paste: '', gh: '', keyOnly: false, pw: '', pw2: '', busy: false, error: '' });
  const load = async () => {
    ui.loading = true; ui.loadError = ''; redraw();
    try {
      ui.st = await get('/account');
      ui.keys = (ui.st.keys || []).map((k) => ({ ...k, line: keyLine(k) }));
      ui.keyOnly = !!ui.st.key_only;
    } catch (e) { ui.loadError = e.message; }
    ui.loading = false; redraw();
  };
  if (!ui.st && !ui.loading && !ui.loadError) load();
  if (!ui.st && ui.loadError) return panel('Operator account', {}, h('div.form', notice({ level: 'warning', message: ui.loadError }), h('div.row.end', button('Retry', load))));
  if (!ui.st) return panel('Operator account', {}, h('p.faint', 'Loading…'));
  const st = ui.st;
  const saved = (st.keys || []).map((k) => k.data).join(' ');
  const dirty = ui.keys.map((k) => k.data).join(' ') !== saved || ui.keyOnly !== !!st.key_only || ui.pw;
  const save = async () => {
    ui.error = '';
    if (ui.pw && ui.pw !== ui.pw2) { ui.error = 'Passwords do not match.'; redraw(); return; }
    ui.busy = true; redraw();
    try {
      const body = { keys: ui.keys.map(keyLine), key_only: ui.keyOnly };
      if (ui.pw) body.password = ui.pw;
      ui.st = await put('/account', body);
      ui.keys = (ui.st.keys || []).map((k) => ({ ...k, line: keyLine(k) }));
      ui.pw = ui.pw2 = '';
      toast('Operator account saved.');
    } catch (e) { ui.error = e.message; }
    ui.busy = false; redraw();
  };
  const ssh = { active: h('span.ok', 'On'), inactive: h('span.warn', 'Off'), 'not-installed': h('span.warn', 'Not installed · apt install openssh-server') }[st.ssh] || st.ssh;
  return panel('Operator account', { meta: 'SSH · sudo · meshctl' },
    h('div.form',
      kv([
        ['Account', h('span.mono', st.name || ACCOUNT)],
        ['Status', st.exists ? 'Active' : h('span.faint', 'None')],
        ['Password', st.password ? 'Set' : h('span.faint', 'Not set')],
        ['SSH server', ssh],
      ]),
      keysEditor(ui),
      passwordAuthToggle(ui),
      h('div.fields',
        field(st.password ? 'New password' : 'Password', h('input.input', { type: 'password', value: ui.pw, autocomplete: 'new-password', oninput: (e) => { ui.pw = e.target.value; redraw(); } }), { hint: 'Min. 8 characters. Empty: unchanged.' }),
        field('Confirm', h('input.input', { type: 'password', value: ui.pw2, autocomplete: 'new-password', oninput: (e) => { ui.pw2 = e.target.value; redraw(); } }))),
      ui.error ? h('p.bad', ui.error) : null,
      h('div.row.between',
        h('span.faint', { style: { fontSize: '13.5px' } }, 'Login: ', h('code', `ssh ${ACCOUNT}@NODE`)),
        button(ui.busy ? 'Saving…' : 'Save account', save, 'primary', { disabled: ui.busy || !dirty || (ui.pw && ui.pw.length < 8) || (ui.keyOnly && !ui.keys.length) }))));
}
