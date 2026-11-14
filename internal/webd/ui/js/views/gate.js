// The gate: signing in, and the setup wizard of a node that waits to be
// provisioned ("flash, scan the profile QR, name it").
import { h } from '../dom.js';
import { get, put, post } from '../api.js';
import { state, redraw, toast } from '../state.js';
import { brandMark, field, button, notice, qr, toggle, icon } from '../ui.js';
import { bandName } from '../fmt.js';
import { canScan, canScanImages, startScan, scanImage } from '../scan.js';
import { linkFields, nextLink } from '../links.js';

let refresh = () => {};

function frame(wide, ...body) {
  return h('div.gate', h('div.gate-card', { class: wide ? 'wide' : '' },
    h('div.gate-brand', brandMark(), h('div', h('div.brand-name', 'TRELLIS'), h('div.brand-sub', state.session.callsign ? `node ${state.session.callsign}` : 'mesh node'))),
    body));
}

// Sign in -----------------------------------------------------------------------

function signIn() {
  const ui = (state.ui.login ||= { password: '', busy: false, error: '' });
  const submit = async (e) => {
    e.preventDefault();
    if (ui.busy || !ui.password) return;
    ui.busy = true; ui.error = ''; redraw();
    try {
      await post('/login', { password: ui.password });
      ui.password = '';
      await refresh();
    } catch (err) {
      ui.error = err.status === 401 ? 'That is not the admin password.' : err.message;
    }
    ui.busy = false; redraw();
  };
  return frame(false,
    h('div', h('h1', 'Sign in'), h('p.lede', 'The admin password of this node.')),
    h('form.form', { onsubmit: submit },
      field('Password', h('input.input', { type: 'password', value: ui.password, autocomplete: 'current-password', autofocus: true, class: ui.error ? 'invalid' : '',
        oninput: (e) => { ui.password = e.target.value; } }), { error: ui.error }),
      button(ui.busy ? 'Checking…' : 'Sign in', null, 'primary', { type: 'submit', disabled: ui.busy })),
    h('p.faint', { style: { fontSize: '13.5px' } }, 'Forgotten? On the node itself: ', h('code', 'sudo meshctl web password')));
}

function noPassword() {
  return frame(false,
    h('div', h('h1', 'No admin password yet'), h('p.lede', 'This node is part of a mesh but nobody has given its web UI a password, so it cannot be opened from the network.')),
    notice({ level: 'info', message: h('span', 'Set one on the node itself, over SSH or a console:', h('pre.block', { style: { marginTop: '8px' } }, 'sudo meshctl web password')) }),
    h('div.row', button('I have set it', () => refresh(), 'primary')));
}

function unreachable() {
  return frame(false,
    h('div', h('h1', 'The node does not answer'), h('p.lede', 'This page loaded, but the node’s daemon is not answering. It may be restarting.')),
    h('div.row', button('Try again', () => refresh(), 'primary')));
}

// Setup wizard --------------------------------------------------------------------

// listBands writes bands in words: "2.4 GHz, 5 GHz and HaLow".
function listBands(bands) {
  const names = ['2g', '5g', '6g', 's1g'].filter((b) => bands.includes(b)).map(bandName);
  return names.length > 1 ? names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1] : names.join('');
}

function wizard() {
  const s = state.session;
  const ui = (state.ui.setup ||= {
    step: 0, pw: '', pw2: '', mode: 'join', code: '', file: '', name: '', links: null, regMode: 'country', country: 'US',
    callsign: /^trellis-node$|^localhost$/.test(s.callsign || '') ? '' : (s.callsign || ''), nodeID: 0,
    busy: false, error: '', fields: {}, preview: null, done: null, scanning: false, stop: null,
  });
  const first = s.admin_set ? 1 : 0;
  if (ui.step < first) ui.step = first;
  const fail = (e) => {
    ui.fields = {};
    for (const f of e.fields || []) ui.fields[f.field] = f.message;
    ui.error = e.message.split('\n')[0];
  };
  const steps = h('div.steps', [0, 1, 2, 3].slice(first).map((i) => h('i', { class: i < ui.step ? 'done' : i === ui.step ? 'now' : '' })));
  // The bands this node has a mesh radio for, for the links it proposes.
  const radios = ((state.status && state.status.radios) || []).filter((r) => r.mesh_point);
  const bands = [...new Set(radios.flatMap((r) => r.bands || []))];
  // A new mesh starts with one link, on the band of this node's radios the
  // usual order puts first; more can be added, one per band or radio. Made
  // when "Start a new mesh" is shown, by when the radios are known.
  if (ui.mode === 'new') ui.links ||= [nextLink([], ui.country, bands)];
  const profileInput = () => {
    if (ui.mode !== 'new') return ui.file ? { yaml: ui.file } : { code: ui.code.trim() };
    const [first, ...more] = ui.links.map((l) => ({ band: l.band, channel: Number(l.channel), width: Number(l.width) }));
    return { new: { name: ui.name.trim(), ...first, links: more.length ? more : undefined, mode: ui.regMode, country: ui.country.toUpperCase() } };
  };
  const stopScan = () => { if (ui.stop) ui.stop(); ui.stop = null; ui.scanning = false; };
  const go = (n) => { stopScan(); ui.error = ''; ui.step = n; redraw(); };

  // Step 0: the admin password.
  if (ui.step === 0) {
    const submit = async (e) => {
      e.preventDefault();
      ui.error = '';
      if (ui.pw.length < 8) { ui.error = 'Use at least 8 characters.'; redraw(); return; }
      if (ui.pw !== ui.pw2) { ui.error = 'The two passwords differ.'; redraw(); return; }
      ui.busy = true; redraw();
      try {
        await put('/password', { new: ui.pw });
        ui.pw = ui.pw2 = '';
        await refresh();
        ui.step = 1;
      } catch (err) { fail(err); }
      ui.busy = false; redraw();
    };
    return frame(false, steps,
      h('div', h('h1', 'Set up this node'), h('p.lede', 'It is not part of a mesh yet. First give it an admin password: it protects this page from now on.')),
      h('form.form', { onsubmit: submit },
        field('Admin password', h('input.input', { type: 'password', value: ui.pw, autocomplete: 'new-password', autofocus: true, oninput: (e) => { ui.pw = e.target.value; } }), { hint: 'At least 8 characters.' }),
        field('Again', h('input.input', { type: 'password', value: ui.pw2, autocomplete: 'new-password', oninput: (e) => { ui.pw2 = e.target.value; } }), { error: ui.error }),
        button(ui.busy ? 'Saving…' : 'Continue', null, 'primary', { type: 'submit', disabled: ui.busy })));
  }

  // Step 1: which mesh.
  if (ui.step === 1) {
    const scan = async () => {
      ui.scanning = true; ui.error = ''; redraw();
      requestAnimationFrame(async () => {
        const video = document.querySelector('.scanner video');
        try {
          ui.stop = await startScan(video, (text) => {
            ui.stop = null; ui.scanning = false;
            if (text.startsWith('TRP1:')) { ui.code = text; ui.file = ''; toast('Profile code read.'); }
            else ui.error = 'That QR code is not a Trellis mesh profile.';
            redraw();
          });
        } catch (err) {
          ui.scanning = false;
          ui.error = 'The camera could not be opened' + (window.isSecureContext ? '.' : ' (the browser only allows it on https://).') + ' Paste the profile code instead.';
          redraw();
        }
      });
    };
    const fromImage = async (e) => {
      const f = e.target.files[0];
      if (!f) return;
      try {
        const text = await scanImage(f);
        if (text && text.startsWith('TRP1:')) { ui.code = text; ui.file = ''; ui.error = ''; toast('Profile code read.'); }
        else ui.error = 'No Trellis profile code was found in that picture.';
      } catch (err) { ui.error = 'That picture could not be read.'; }
      redraw();
    };
    const fromFile = async (e) => {
      const f = e.target.files[0];
      if (!f) return;
      ui.file = await f.text(); ui.code = ''; ui.fileName = f.name; ui.error = '';
      redraw();
    };
    const next = async () => {
      ui.busy = true; ui.error = ''; ui.fields = {}; redraw();
      try {
        // A dry run with a placeholder name checks the profile on its own.
        await post('/setup?dry_run=1', { ...profileInput(), callsign: ui.callsign || 'node' });
        ui.step = 2;
      } catch (err) { fail(err); }
      ui.busy = false; redraw();
    };
    const ready = ui.mode === 'new' ? ui.name.trim() : (ui.code.trim() || ui.file);
    const join = [
      ui.scanning ? h('div.scanner', h('video', { muted: true, playsinline: true })) : null,
      h('div.row',
        canScan() ? button(ui.scanning ? 'Stop the camera' : 'Scan the QR code', () => { if (ui.scanning) { stopScan(); redraw(); } else scan(); }, ui.scanning ? '' : 'primary', { icon: 'camera' }) : null,
        canScanImages() ? h('label.btn', icon('camera'), 'Photo of the code', h('input', { type: 'file', accept: 'image/*', hidden: true, onchange: fromImage })) : null,
        h('label.btn', icon('download'), 'Profile file', h('input', { type: 'file', accept: '.yaml,.yml,text/plain', hidden: true, onchange: fromFile }))),
      ui.file
        ? notice({ level: 'ok', message: h('span', 'Profile file ', h('b', ui.fileName || 'chosen'), '. ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); ui.file = ''; redraw(); } }, 'Use a code instead')) })
        : field('Profile code', h('textarea.textarea', { value: ui.code, placeholder: 'TRP1:…', spellcheck: 'false', autocapitalize: 'none', oninput: (e) => { ui.code = e.target.value; redraw(); } }),
          { hint: 'On a node that is already in the mesh: Settings → Mesh profile → Share with another node. Scan its QR code, or copy the code and paste it here.' }),
    ];
    const fresh = ui.mode !== 'new' ? [] : [
      h('div.fields',
        field('Mesh name', h('input.input', { value: ui.name, placeholder: 'e.g. alpha', maxlength: '24', autocapitalize: 'none', spellcheck: 'false', class: ui.fields.name || ui.fields.mesh_id ? 'invalid' : '', oninput: (e) => { ui.name = e.target.value; redraw(); } }),
          { error: ui.fields.name || ui.fields.mesh_id, hint: 'A label for your team. The mesh secret is generated for you.' }),
        h('span')),
      ui.links.map((l, i) => {
        const others = ui.links.filter((x) => x !== l);
        const row = linkFields(l, i === 0 ? 'radio' : `links[${i - 1}]`, ui.fields, { title: ui.links.length > 1 ? `Mesh link ${i + 1}` : '', country: ui.country, others });
        return i === 0 ? row : h('div.link-row', row,
          h('div.row.end', button('Remove this link', () => { ui.links.splice(i, 1); redraw(); }, 'small')));
      }),
      h('div.row',
        button('Add a mesh link', () => { ui.links.push(nextLink(ui.links, ui.country, bands)); redraw(); }, 'small', { disabled: ui.links.length >= 10 }),
        h('span.hint', bands.length
          ? `This node has mesh radios for ${listBands(bands)}. Each link runs on its own radio; the mesh takes the fastest path over any of them. A link this node has no radio for still reaches nodes that do.`
          : 'Each link runs on its own radio, usually one per band; the mesh takes the fastest path over any of them.')),
      h('div.fields',
        field('Radio rules', h('select.select', { value: ui.regMode, onchange: (e) => { ui.regMode = e.target.value; redraw(); } },
          h('option', { value: 'country' }, 'Country: that country’s channels and power'), h('option', { value: 'unrestricted' }, 'Unrestricted: all the hardware allows'))),
        field('Country', h('input.input', { value: ui.country, maxlength: '2', autocapitalize: 'characters', class: ui.fields['regulatory.country'] ? 'invalid' : '', oninput: (e) => { ui.country = e.target.value; redraw(); } }), { error: ui.fields['regulatory.country'], hint: 'Two letters, e.g. US.' })),
      ui.regMode === 'unrestricted'
        ? notice({ level: 'warning', message: 'Unrestricted mode is for operators who hold their own frequency authorization. Without one, transmitting outside the local licence-exempt rules is unlawful in most countries: choose country mode.' })
        : h('p.faint', { style: { fontSize: '13.5px' } }, 'Country mode keeps every radio inside that country’s licence-exempt channels and power. Every node of the mesh needs the same rules. You can change them later in Settings, on each node.'),
    ];
    return frame(true, steps,
      h('div', h('h1', 'Which mesh?'), h('p.lede', 'A node joins a mesh by taking its profile: the channel and the secret all its nodes share.')),
      h('div.choice',
        h('button', { type: 'button', 'aria-pressed': String(ui.mode === 'join'), onclick: () => { ui.mode = 'join'; ui.error = ''; redraw(); } }, h('b', 'Join a mesh'), h('span', 'There are nodes already. Take their profile.')),
        h('button', { type: 'button', 'aria-pressed': String(ui.mode === 'new'), onclick: () => { stopScan(); ui.mode = 'new'; ui.error = ''; redraw(); } }, h('b', 'Start a new mesh'), h('span', 'This is the first node.'))),
      h('div.form', ui.mode === 'join' ? join : fresh),
      ui.error ? notice({ level: 'error', message: ui.error }) : null,
      h('div.row.end', button(ui.busy ? 'Checking…' : 'Continue', next, 'primary', { disabled: !ready || ui.busy })));
  }

  // Step 2: the node's name.
  if (ui.step === 2) {
    const next = async (e) => {
      e.preventDefault();
      ui.busy = true; ui.error = ''; ui.fields = {}; redraw();
      try {
        ui.preview = await post('/setup?dry_run=1', { ...profileInput(), callsign: ui.callsign.trim(), node_id: Number(ui.nodeID) || 0 });
        ui.step = 3;
      } catch (err) { fail(err); }
      ui.busy = false; redraw();
    };
    return frame(false, steps,
      h('div', h('h1', 'Name this node'), h('p.lede', 'The callsign is how the team sees it: on the map of the mesh, as its Wi-Fi name, and as callsign.mesh.')),
      h('form.form', { onsubmit: next },
        field('Callsign', h('input.input', { value: ui.callsign, placeholder: 'e.g. relay-1', maxlength: '63', autocapitalize: 'none', spellcheck: 'false', autofocus: true, class: ui.fields.callsign ? 'invalid' : '',
          oninput: (e) => { ui.callsign = e.target.value.toLowerCase(); redraw(); } }), { error: ui.fields.callsign, hint: 'Lowercase letters, digits and hyphens.' }),
        h('details', h('summary', 'Pin a node ID'),
          h('div', { style: { marginTop: '12px' } }, field('Node ID', h('input.input', { type: 'number', min: '0', max: '254', value: ui.nodeID || '', placeholder: 'automatic', oninput: (e) => { ui.nodeID = e.target.value; } }),
            { error: ui.fields.node_id, hint: '1 to 254, unique in the mesh. Leave empty: the node picks one and settles conflicts by itself.' }))),
        ui.error && !ui.fields.callsign ? notice({ level: 'error', message: ui.error }) : null,
        h('div.row.between', button('Back', () => go(1)), button(ui.busy ? 'Checking…' : 'Continue', null, 'primary', { type: 'submit', disabled: ui.busy || !ui.callsign.trim() }))));
  }

  // Step 3: review, apply, and what to do afterwards.
  const p = ui.done || ui.preview;
  const apply = async () => {
    ui.busy = true; ui.error = ''; redraw();
    try {
      ui.done = await post('/setup', { ...profileInput(), callsign: ui.callsign.trim(), node_id: Number(ui.nodeID) || 0 });
    } catch (err) { fail(err); }
    ui.busy = false; redraw();
  };
  const wifi = p && p.ap && p.ap.enabled ? h('div.panel', h('div.panel-body',
    h('div.label', { style: { marginBottom: '10px' } }, ui.done ? 'This node’s Wi-Fi is now' : 'Afterwards, this node’s Wi-Fi is'),
    h('div.join', qr(p.ap.qr, 'Wi-Fi QR code'),
      h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } },
        h('div', h('div.label', 'Network'), h('div.mono', { style: { fontSize: '18px' } }, p.ap.ssid)),
        h('div', h('div.label', 'Passphrase'), h('div.mono', { style: { fontSize: '18px', overflowWrap: 'anywhere' } }, p.ap.passphrase)),
        h('p.dim', { style: { fontSize: '13.5px' } }, ui.done ? 'Join it to come back to this node.' : 'Write these down or take a screenshot now: the setup Wi-Fi you may be on goes away.'))))) : null;
  if (ui.done) {
    return frame(true, h('div.steps', [1, 2, 3].map(() => h('i.done'))),
      h('div', h('h1', `${ui.callsign} is set up`), h('p.lede', 'It is joining the mesh now. Give it a few seconds.')),
      wifi,
      notice({ level: 'info', message: h('span', 'Then open ', h('b', 'https://' + ((state.status && state.status.client_network.anycast.address) || '10.73.0.1')), ' again and sign in with the admin password. (',
        h('b', 'https://node.mesh'), ' is the same page, where the phone uses the node for name lookups.) Other nodes of the mesh see this one as ', h('b', ui.callsign + '.mesh'), '.') }),
      h('div.row.end', button('Open the dashboard', async () => { state.ui.setup = null; await refresh(); }, 'primary')));
  }
  return frame(true, steps,
    h('div', h('h1', 'Ready'), h('p.lede', `${ui.callsign} is about to ${ui.mode === 'new' ? 'start the mesh “' + ui.name.trim() + '”' : 'join the mesh'}.`)),
    p ? h('div.notices', (p.effects || []).map((e) => notice(e))) : null,
    wifi,
    ui.error ? notice({ level: 'error', message: ui.error }) : null,
    h('div.row.between', button('Back', () => go(2), '', { disabled: ui.busy }), button(ui.busy ? 'Setting up…' : 'Set up this node', apply, 'primary', { disabled: ui.busy })));
}

export function gate(ctx) {
  refresh = ctx.refreshSession;
  const s = state.session;
  if (s.unreachable && !s.meshd_up) return unreachable();
  if (s.setup && (s.authenticated || !s.admin_set)) return wizard();
  if (state.ui.setup && state.ui.setup.done) return wizard();
  if (!s.admin_set) return noPassword();
  return signIn();
}
