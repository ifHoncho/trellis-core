// The gate: signing in, and the setup wizard of a node that waits to be
// provisioned ("flash, scan the profile QR, name it").
import { h } from '../dom.js';
import { get, put, post } from '../api.js';
import { state, redraw, toast } from '../state.js';
import { brandMark, field, button, notice, qr, toggle, icon } from '../ui.js';
import { bandName } from '../fmt.js';
import { canScan, canScanImages, startScan, scanImage } from '../scan.js';
import { linkFields, linkOrder, nextLink } from '../links.js';

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
      ui.error = err.status === 401 ? 'Incorrect password.' : err.message;
    }
    ui.busy = false; redraw();
  };
  return frame(false,
    h('div', h('h1', 'Sign in'), h('p.lede', 'Node admin password.')),
    h('form.form', { onsubmit: submit },
      field('Password', h('input.input', { type: 'password', value: ui.password, autocomplete: 'current-password', autofocus: true, class: ui.error ? 'invalid' : '',
        oninput: (e) => { ui.password = e.target.value; } }), { error: ui.error }),
      button(ui.busy ? 'Checking…' : 'Sign in', null, 'primary', { type: 'submit', disabled: ui.busy })),
    h('p.faint', { style: { fontSize: '13.5px' } }, 'Reset on the node: ', h('code', 'sudo meshctl web password')));
}

function noPassword() {
  return frame(false,
    h('div', h('h1', 'No admin password set'), h('p.lede', 'Web UI locked: no admin password set.')),
    notice({ level: 'info', message: h('span', 'Set one on the node (SSH or console):', h('pre.block', { style: { marginTop: '8px' } }, 'sudo meshctl web password')) }),
    h('div.row', button('Continue', () => refresh(), 'primary')));
}

function unreachable() {
  return frame(false,
    h('div', h('h1', 'Node not responding'), h('p.lede', 'meshd not responding; may be restarting.')),
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
      if (ui.pw.length < 8) { ui.error = 'Min. 8 characters.'; redraw(); return; }
      if (ui.pw !== ui.pw2) { ui.error = 'Passwords do not match.'; redraw(); return; }
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
      h('div', h('h1', 'Node setup'), h('p.lede', 'Not provisioned. Set the admin password for this web UI.')),
      h('form.form', { onsubmit: submit },
        field('Admin password', h('input.input', { type: 'password', value: ui.pw, autocomplete: 'new-password', autofocus: true, oninput: (e) => { ui.pw = e.target.value; } }), { hint: 'Min. 8 characters.' }),
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
            else ui.error = 'Not a Trellis mesh profile.';
            redraw();
          });
        } catch (err) {
          ui.scanning = false;
          ui.error = 'Camera unavailable' + (window.isSecureContext ? '.' : ' (requires https://).') + ' Paste the profile code.';
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
        else ui.error = 'No profile code found in image.';
      } catch (err) { ui.error = 'Image unreadable.'; }
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
        canScan() ? button(ui.scanning ? 'Stop camera' : 'Scan QR code', () => { if (ui.scanning) { stopScan(); redraw(); } else scan(); }, ui.scanning ? '' : 'primary', { icon: 'camera' }) : null,
        canScanImages() ? h('label.btn', icon('camera'), 'Photo of the code', h('input', { type: 'file', accept: 'image/*', hidden: true, onchange: fromImage })) : null,
        h('label.btn', icon('download'), 'Profile file', h('input', { type: 'file', accept: '.yaml,.yml,text/plain', hidden: true, onchange: fromFile }))),
      ui.file
        ? notice({ level: 'ok', message: h('span', 'Profile file ', h('b', ui.fileName || 'chosen'), '. ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); ui.file = ''; redraw(); } }, 'Use code')) })
        : field('Profile code', h('textarea.textarea', { value: ui.code, placeholder: 'TRP1:…', spellcheck: 'false', autocapitalize: 'none', oninput: (e) => { ui.code = e.target.value; redraw(); } }),
          { hint: 'From a mesh node: Settings → Mesh profile → Share profile. Scan the QR code or paste the profile code.' }),
    ];
    const fresh = ui.mode !== 'new' ? [] : [
      h('div.fields',
        field('Mesh name', h('input.input', { value: ui.name, placeholder: 'e.g. alpha', maxlength: '24', autocapitalize: 'none', spellcheck: 'false', class: ui.fields.name || ui.fields.mesh_id ? 'invalid' : '', oninput: (e) => { ui.name = e.target.value; redraw(); } }),
          { error: ui.fields.name || ui.fields.mesh_id, hint: 'Team label. Mesh secret auto-generated.' }),
        h('span')),
      ui.links.map((l, i) => {
        const others = ui.links.filter((x) => x !== l);
        const row = linkFields(l, i === 0 ? 'radio' : `links[${i - 1}]`, ui.fields, { title: ui.links.length > 1 ? linkOrder(i) : '', country: ui.country, others });
        return i === 0 ? row : h('div.link-row', row,
          h('div.row.end', button('Remove link', () => { ui.links.splice(i, 1); redraw(); }, 'small')));
      }),
      h('div.row',
        button('Add a mesh link', () => { ui.links.push(nextLink(ui.links, ui.country, bands)); redraw(); }, 'small', { disabled: ui.links.length >= 10 }),
        h('span.hint', bands.length
          ? `Mesh radios: ${listBands(bands)}. One radio per link; routing uses the fastest path. Links without a local radio still serve other nodes.`
          : 'One radio per link, typically one per band; routing uses the fastest path.')),
      h('div.fields',
        field('Regulatory', h('select.select', { value: ui.regMode, onchange: (e) => { ui.regMode = e.target.value; redraw(); } },
          h('option', { value: 'country' }, 'Country: national channel and power limits'), h('option', { value: 'unrestricted' }, 'Unrestricted: hardware limits only'))),
        field('Country', h('input.input', { value: ui.country, maxlength: '2', autocapitalize: 'characters', class: ui.fields['regulatory.country'] ? 'invalid' : '', oninput: (e) => { ui.country = e.target.value; redraw(); } }), { error: ui.fields['regulatory.country'], hint: 'ISO 3166 code, e.g. US.' })),
      ui.regMode === 'unrestricted'
        ? notice({ level: 'warning', message: 'Unrestricted mode requires frequency authorization. Without it, transmitting outside licence-exempt rules is unlawful in most countries.' })
        : h('p.faint', { style: { fontSize: '13.5px' } }, 'Country mode: licence-exempt channels and power for that country. All nodes need the same setting. Changeable later in Settings.'),
    ];
    return frame(true, steps,
      h('div', h('h1', 'Mesh'), h('p.lede', 'Joining requires the mesh profile (channel and secret).')),
      h('div.choice',
        h('button', { type: 'button', 'aria-pressed': String(ui.mode === 'join'), onclick: () => { ui.mode = 'join'; ui.error = ''; redraw(); } }, h('b', 'Join a mesh'), h('span', 'Existing mesh: import its profile.')),
        h('button', { type: 'button', 'aria-pressed': String(ui.mode === 'new'), onclick: () => { stopScan(); ui.mode = 'new'; ui.error = ''; redraw(); } }, h('b', 'Start a new mesh'), h('span', 'First node of a new mesh.'))),
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
      h('div', h('h1', 'Callsign'), h('p.lede', 'Node identifier: topology map, client AP SSID, DNS name (callsign.mesh).')),
      h('form.form', { onsubmit: next },
        field('Callsign', h('input.input', { value: ui.callsign, placeholder: 'e.g. relay-1', maxlength: '63', autocapitalize: 'none', spellcheck: 'false', autofocus: true, class: ui.fields.callsign ? 'invalid' : '',
          oninput: (e) => { ui.callsign = e.target.value.toLowerCase(); redraw(); } }), { error: ui.fields.callsign, hint: 'Lowercase letters, digits, hyphens.' }),
        h('details', h('summary', 'Set node ID'),
          h('div', { style: { marginTop: '12px' } }, field('Node ID', h('input.input', { type: 'number', min: '0', max: '254', value: ui.nodeID || '', placeholder: 'automatic', oninput: (e) => { ui.nodeID = e.target.value; } }),
            { error: ui.fields.node_id, hint: '1–254, unique in the mesh. Empty: automatic, with conflict resolution.' }))),
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
    h('div.label', { style: { marginBottom: '10px' } }, ui.done ? 'Client AP' : 'Client AP after setup'),
    h('div.join', qr(p.ap.qr, 'Wi-Fi QR code'),
      h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } },
        h('div', h('div.label', 'Network'), h('div.mono', { style: { fontSize: '18px' } }, p.ap.ssid)),
        h('div', h('div.label', 'Passphrase'), h('div.mono', { style: { fontSize: '18px', overflowWrap: 'anywhere' } }, p.ap.passphrase)),
        h('p.dim', { style: { fontSize: '13.5px' } }, ui.done ? 'Reconnect via this AP.' : 'Record these now: the setup AP will shut down.'))))) : null;
  if (ui.done) {
    return frame(true, h('div.steps', [1, 2, 3].map(() => h('i.done'))),
      h('div', h('h1', `${ui.callsign} provisioned`), h('p.lede', 'Joining the mesh.')),
      wifi,
      notice({ level: 'info', message: h('span', 'Web UI: ', h('b', 'https://' + ((state.status && state.status.client_network.anycast.address) || '10.73.0.1')), ' or ',
        h('b', 'https://node.mesh'), ' (requires node DNS). Sign in with the admin password. Mesh DNS name: ', h('b', ui.callsign + '.mesh'), '.') }),
      h('div.row.end', button('Open dashboard', async () => { state.ui.setup = null; await refresh(); }, 'primary')));
  }
  return frame(true, steps,
    h('div', h('h1', 'Ready'), h('p.lede', `${ui.callsign}: ${ui.mode === 'new' ? 'create mesh “' + ui.name.trim() + '”' : 'join mesh'}.`)),
    p ? h('div.notices', (p.effects || []).map((e) => notice(e))) : null,
    wifi,
    ui.error ? notice({ level: 'error', message: ui.error }) : null,
    h('div.row.between', button('Back', () => go(2), '', { disabled: ui.busy }), button(ui.busy ? 'Setting up…' : 'Node setup', apply, 'primary', { disabled: ui.busy })));
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
