// Settings: everything about this node that an operator may change.
import { h } from '../dom.js';
import { get, put, post } from '../api.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { panel, field, toggle, button, kv, table, qr, notice, copyText, download, pill } from '../ui.js';
import { bandName, since, duration, count, meshLinks, meshRadios, carried } from '../fmt.js';
import { ensureConfig, cfg, saveBar, saveProfile, loadConfig } from '../config.js';
import { linkFields, linkLine, nextLink } from '../links.js';

const SECTIONS = [
  ['node', 'Node'], ['wifi', 'Wi-Fi'], ['internet', 'Internet'], ['ports', 'Ports'],
  ['profile', 'Mesh profile'], ['access', 'Access'], ['system', 'System'],
];
const err = (name) => cfg().fields[name];
const input = (obj, key, attrs = {}) => h('input.input', {
  value: obj[key] ?? '', class: attrs.invalid ? 'invalid' : '',
  oninput: (e) => { obj[key] = attrs.type === 'number' ? (e.target.value === '' ? 0 : Number(e.target.value)) : e.target.value; redraw(); }, ...attrs,
});
const select = (obj, key, options, attrs = {}) => h('select.select', {
  value: String(obj[key] ?? ''), onchange: (e) => { obj[key] = attrs.number ? Number(e.target.value) : e.target.value; if (attrs.after) attrs.after(); redraw(); },
}, options.map(([v, label]) => h('option', { value: String(v) }, label)));

// Sections ----------------------------------------------------------------------

function nodeSection(d, st) {
  return [panel('Identity', {},
    h('div.fields',
      field('Callsign', input(d, 'callsign', { maxlength: '63', autocapitalize: 'none', spellcheck: 'false', invalid: err('callsign') }),
        { error: err('callsign'), hint: `Lowercase letters, digits and hyphens. Other nodes reach this one as ${d.callsign || 'callsign'}.mesh, and its Wi-Fi takes this name unless you set another.` }),
      field('Node ID', h('input.input', { type: 'number', min: '0', max: '254', value: d.node_id || '', placeholder: `automatic (now ${st.node.node_id})`,
        oninput: (e) => { d.node_id = e.target.value === '' ? 0 : Number(e.target.value); redraw(); } }),
        { error: err('node_id'), hint: `1 to 254, unique in the mesh; it sets the node’s address (10.73.ID.1). Leave empty to let the node choose and settle conflicts by itself.` }))),
  panel('Advanced', {},
    h('div.form',
      h('div.fields',
        field('Mesh link MTU', input(d, 'mesh_mtu', { type: 'number', min: '1280', max: '2304' }), { error: err('mesh_mtu'), hint: '1560 carries full-size packets plus batman-adv’s headers.' })),
      toggle('Allow an old batman-adv', 'For testing only. One such node makes the whole mesh flood multicast.', d.allow_old_batman, (v) => { d.allow_old_batman = v; redraw(); })))];
}

function wifiSection(d, st) {
  const ap = d.ap;
  const kept = ap.passphrase === '<redacted>';
  return [panel('Client Wi-Fi', {},
    h('div.form',
      toggle('Run a Wi-Fi network for this node’s devices', 'Phones and tablets join it. Each node has its own.', ap.enabled !== false, (v) => { ap.enabled = v; redraw(); }),
      h('div.fields',
        field('Network name', input(ap, 'ssid', { maxlength: '32', placeholder: d.callsign + ' (the callsign)', invalid: err('ap.ssid') }), { error: err('ap.ssid'), hint: 'Leave empty to use the callsign.' }),
        field('Passphrase', h('input.input', { type: 'text', value: kept ? '' : ap.passphrase || '', placeholder: kept ? 'set — type to replace it' : 'generated for this node', maxlength: '63',
          autocomplete: 'off', class: err('ap.passphrase') ? 'invalid' : '', oninput: (e) => { ap.passphrase = e.target.value; redraw(); } }),
          { error: err('ap.passphrase'), hint: kept ? h('span', 'A passphrase of your own is set. ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); ap.passphrase = ''; redraw(); } }, 'Go back to the generated one')) : '8 to 63 characters, or leave empty for the one the node generated.' })),
      h('div.fields',
        field('Band', select(ap, 'band', [['5g', '5 GHz (preferred)'], ['2g', '2.4 GHz']], { after: () => { ap.channel = 0; } }),
          { error: err('ap.band'), hint: 'The node falls back to 2.4 GHz when the radio cannot run a 5 GHz access point, and always keeps away from the mesh channel.' }),
        field('Channel', h('input.input', { type: 'number', min: '0', value: ap.channel || '', placeholder: 'automatic', oninput: (e) => { ap.channel = e.target.value === '' ? 0 : Number(e.target.value); redraw(); } }),
          { error: err('ap.channel'), hint: 'Empty: the node picks one without radar rules. Any channel the radio has may be set; on a radar (DFS) channel the network starts after a check of a minute or more.' }),
        // Ignored since the node has one regulatory domain (the profile's):
        // shown only to clear an old value that differs from it.
        ap.country && ap.country.toUpperCase() !== st.regulatory.country ? field('Country (ignored)', input(ap, 'country', { maxlength: '2', autocapitalize: 'characters' }),
          { error: err('ap.country'), hint: h('span', `The Wi-Fi follows the mesh profile’s country, ${st.regulatory.country}. `, h('a', { href: '#', onclick: (e) => { e.preventDefault(); ap.country = ''; redraw(); } }, 'Clear this')) }) : null),
      toggle('Keep devices on this Wi-Fi apart', 'They still reach the rest of the mesh, but not each other directly.', !!ap.isolate, (v) => { ap.isolate = v; redraw(); })))];
}

function internetSection(d, st) {
  const g = d.gateway;
  const ui = (state.ui.settings ||= {});
  if (ui.probes == null || ui.probesFor !== cfg().base) { ui.probes = (g.probe_targets || []).join(', '); ui.probesFor = cfg().base; }
  return [panel('Sharing this node’s uplink', {},
    h('div.form',
      toggle('Share an internet uplink with the mesh', 'When this node has a working one. Off: it only uses other nodes’ gateways.', g.mode !== 'off', (v) => { g.mode = v ? 'auto' : 'off'; redraw(); }),
      h('div.fields',
        field('Uplink interface', input(g, 'wan', { placeholder: 'automatic' + (st.gateway.wan ? ` (now ${st.gateway.wan})` : ''), maxlength: '15', invalid: err('gateway.wan') }),
          { error: err('gateway.wan'), hint: 'Empty: a port with the uplink role, else whatever carries the system’s default route.' }),
        field('Announced download, Mbit/s', input(g, 'down_mbit', { type: 'number', min: '1' }), { error: err('gateway.down_mbit') }),
        field('Announced upload, Mbit/s', input(g, 'up_mbit', { type: 'number', min: '1' }), { error: err('gateway.up_mbit'), hint: 'Other nodes weigh this against the mesh path when they choose a gateway.' })))),
  panel('Uplink health check', {},
    h('div.fields',
      field('Addresses to ping', h('input.input', { value: ui.probes, class: Object.keys(cfg().fields).some((k) => k.startsWith('gateway.probe_targets')) ? 'invalid' : '',
        oninput: (e) => { ui.probes = e.target.value; g.probe_targets = e.target.value.split(/[\s,]+/).filter(Boolean); redraw(); } }),
        { error: Object.entries(cfg().fields).filter(([k]) => k.startsWith('gateway.probe_targets')).map(([, v]) => v)[0], hint: 'The uplink counts as healthy while any of them answers.' }),
      field('Every, ms', input(g, 'probe_interval_ms', { type: 'number', min: '200', max: '10000' }), { error: err('gateway.probe_interval_ms'), hint: 'Three misses in a row mark the uplink down.' })))];
}

function portsSection(d, st) {
  const names = [...new Set([...(st.ports || []).map((p) => p.name), ...(d.ports || []).map((p) => p.name)])];
  const roleOf = (name) => ((d.ports || []).find((p) => p.name === name) || {}).role || '';
  const setRole = (name, role) => {
    d.ports = (d.ports || []).filter((p) => p.name !== name);
    if (role) d.ports.push({ name, role });
    redraw();
  };
  return [panel('Ethernet ports', { flush: true },
    table({ stack: true, key: (n) => n }, [
      { title: 'Port', cell: (n) => h('span.primary', n) },
      { title: 'Now', cell: (n) => { const p = (st.ports || []).find((x) => x.name === n); return p ? h('span', pill(p.role, 'plain'), p.carrier ? h('span.ok', ' cable in') : h('span.faint', ' no cable')) : h('span.faint', 'not present'); } },
      { title: 'Role', cls: 'wide', wide: true, cell: (n) => h('select.select', { value: roleOf(n), onchange: (e) => setRole(n, e.target.value), style: { maxWidth: '340px' } },
        h('option', { value: '' }, 'Not listed: automatic, shared with the system'),
        h('option', { value: 'auto' }, 'Automatic: Trellis owns it and works out what is plugged in'),
        h('option', { value: 'client' }, 'Devices: always a port for end-user devices'),
        h('option', { value: 'wan' }, 'Uplink: always this node’s internet uplink'),
        h('option', { value: 'mesh' }, 'Mesh: wired backhaul to another node')) },
    ], names, 'This node has no Ethernet ports Trellis can see.'),
    ),
  h('p.dim', { style: { fontSize: '14px', maxWidth: '75ch' } },
    'An automatic port becomes a device port when a laptop or switch is plugged in, and an uplink when a router that hands out addresses is. ',
    'If you reach this page through a port, changing that port’s role may cut you off.')];
}

async function shareProfile() {
  let ex;
  try { ex = await get('/profile/export'); } catch (e) { toast(e.message, 'bad', 6000); return; }
  openDialog({
    title: 'Share the mesh profile',
    body: () => [
      notice({ level: 'warning', message: 'This is the mesh secret. Anyone who has it can join a node to the mesh. Show it only to the people setting up nodes.' }),
      h('div.join', qr(ex.qr, 'Mesh profile QR code'),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
          h('p.dim', { style: { fontSize: '14px' } }, 'On the new node’s setup page choose “Join a mesh”, then scan this code, or paste the profile code.'),
          h('div.row',
            button('Copy profile code', async () => toast((await copyText(ex.code)) ? 'Profile code copied.' : 'Could not copy: select it below.'), 'small', { icon: 'copy' }),
            button('Download file', () => download(`${ex.profile.name}-profile.yaml`, ex.yaml, 'application/yaml'), 'small', { icon: 'download' })))),
      h('pre.block', { style: { maxHeight: '90px' } }, ex.code),
    ],
    actions: [{ label: 'Done', kind: 'primary', run: closeDialog }],
  });
}

function profileSection(d, st) {
  const ui = (state.ui.profile ||= { draft: null, fields: {}, busy: false, replace: false, code: '' });
  const cur = state.config.profile;
  if (!ui.draft || ui.for !== JSON.stringify(cur)) { ui.draft = JSON.parse(JSON.stringify(cur)); ui.for = JSON.stringify(cur); ui.fields = {}; }
  const p = ui.draft;
  const changed = JSON.stringify(p) !== ui.for;
  const send = async (input) => {
    ui.busy = true; ui.fields = {}; redraw();
    try {
      const res = await saveProfile(input);
      if (res && res.applied) { ui.replace = false; ui.code = ''; }
    } catch (e) {
      for (const f of e.fields || []) ui.fields[f.field] = f.message;
      toast(e.message.split('\n')[0], 'bad', 7000);
    }
    ui.busy = false; redraw();
  };
  const unrestricted = p.regulatory.mode === 'unrestricted';
  // Which of the profile's links this node has a radio for, and what the
  // other nodes say about their radios: what a channel change has to fit.
  const live = meshLinks(st);
  const many = (cur.links || []).length > 0;
  const here = (i) => (live[i] && !carried(live[i]) && st.mesh_link.transport !== 'not yet determined'
    ? h('span.faint', '  ·  no radio for it on this node') : null);
  const others = st.nodes || [];
  const myBands = [...new Set((st.radios || []).filter((r) => r.mesh_point).flatMap((r) => r.bands || []))];
  return [
    panel('The mesh this node belongs to', { tools: [button('Share with another node', shareProfile, 'small primary')] },
      kv([
        ['Mesh', h('span', cur.name, h('span.faint', `  ·  on the air as ${cur.mesh_id}`))],
        [many ? 'Mesh link 1' : 'Channel', h('span', linkLine({ ...cur.radio, running_width_mhz: live[0] && live[0].running_width_mhz }), here(0))],
        ...(cur.links || []).map((l, i) => [`Mesh link ${i + 2}`, h('span', linkLine({ ...l, running_width_mhz: live[i + 1] && live[i + 1].running_width_mhz }), here(i + 1))]),
        ['Regulatory', cur.regulatory.mode === 'unrestricted' ? h('span.warn', 'unrestricted') : `country (${cur.regulatory.country})`],
        ['Addresses', cur.prefix],
        ['Secret', h('span.sans.dim', 'kept on the node; shared only through “Share with another node”')],
      ])),
    panel('Channel and rules', { meta: `on ${st.node.callsign} only` },
      h('div.form',
        notice({ level: 'warning', message: others.length
          ? `This changes ${st.node.callsign} only. Nothing sends it to the other ${others.length === 1 ? 'node' : others.length + ' nodes'} of the mesh, and a node whose channel or secret differs cannot hear the rest. To change the whole mesh, install the same profile on every node: the farthest first, this one last.`
          : 'This changes this node only. Every node of a mesh needs the same profile: one whose channel or secret differs cannot hear the rest.' }),
        others.length ? h('div.others',
          h('div.label', 'Mesh radios on the other nodes'),
          h('div.others-list', others.map((n) => {
            const r = meshRadios(n);
            return h('div', h('span.mono', n.callsign), h('span', { class: r ? 'dim' : 'faint', title: r ? null : 'This node runs a version that does not report its radios.' }, r || 'not reported'));
          })),
          h('span.hint', 'A link only reaches the nodes that have a radio for its band. One radio carries one link at a time.')) : null,
        linkFields(p.radio, 'radio', ui.fields, { title: (p.links || []).length ? 'Mesh link 1' : '', country: p.regulatory.country, others: p.links || [] }),
        (p.links || []).map((l, i) => h('div.link-row',
          linkFields(l, `links[${i}]`, ui.fields, { title: `Mesh link ${i + 2}`, country: p.regulatory.country, others: [p.radio, ...p.links.filter((x) => x !== l)] }),
          h('div.row.end',
            // Swap with the link before: the order is which link a node
            // with too few radios takes first.
            button('Move up', () => { const all = [p.radio, ...p.links]; [all[i], all[i + 1]] = [all[i + 1], all[i]]; p.radio = all[0]; p.links = all.slice(1); redraw(); }, 'small'),
            button('Remove this link', () => { p.links.splice(i, 1); if (!p.links.length) delete p.links; redraw(); }, 'small')))),
        h('div.row',
          button('Add a mesh link', () => { (p.links ||= []).push(nextLink([p.radio, ...p.links], p.regulatory.country, myBands)); redraw(); }, 'small', { disabled: 1 + (p.links || []).length >= 10 }),
          h('span.hint', 'Each link runs on its own radio, usually one per band, and the mesh takes the fastest path over any of them. ',
            (p.links || []).length ? 'The order matters to a node with fewer radios than links: it takes the first links its radios can carry. Put the fastest link first if such nodes should use it; nodes on different links reach each other through a node that carries both.' : '')),
        h('div.fields',
          field('Regulatory mode', select(p.regulatory, 'mode', [['country', 'Country: that country’s channels and power'], ['unrestricted', 'Unrestricted: all the hardware allows']]), { error: ui.fields['regulatory.mode'] }),
          field('Country', input(p.regulatory, 'country', { maxlength: '2', autocapitalize: 'characters' }), { error: ui.fields['regulatory.country'], hint: 'Two-letter code, e.g. US.' })),
        unrestricted ? notice({ level: 'warning', message: 'Unrestricted mode is for operators with their own frequency authorization. Without one, transmitting outside the local licence-exempt rules is unlawful in most countries: use country mode.' }) : null,
        h('div.row.end',
          changed ? button('Discard', () => { ui.draft = null; redraw(); }, '', { disabled: ui.busy }) : null,
          button(ui.busy ? 'Checking…' : 'Review and change', () => send({ profile: p }), 'primary', { disabled: !changed || ui.busy })))),
    panel('Move this node to another mesh', {},
      ui.replace
        ? h('div.form',
            field('Profile code', h('textarea.textarea', { value: ui.code, placeholder: 'TRP1:…', spellcheck: 'false', oninput: (e) => { ui.code = e.target.value; redraw(); } }),
              { hint: 'From the other mesh’s “Share with another node”, or the contents of its profile file.' }),
            h('div.row.end',
              button('Cancel', () => { ui.replace = false; ui.code = ''; redraw(); }),
              button('Review and install', () => send(ui.code.trim().startsWith('TRP1:') ? { code: ui.code.trim() } : { yaml: ui.code }), 'danger', { disabled: !ui.code.trim() || ui.busy })))
        : h('div.row.between',
            h('p.dim', { style: { fontSize: '14px', maxWidth: '60ch' } }, 'Installing another mesh’s profile takes this node out of this mesh and into that one.'),
            button('Install another profile', () => { ui.replace = true; redraw(); }))),
  ];
}

function accessSection(d, st) {
  const ui = (state.ui.access ||= { current: '', next: '', again: '', busy: false, error: '' });
  const change = async () => {
    ui.error = '';
    if (ui.next !== ui.again) { ui.error = 'The two new passwords differ.'; redraw(); return; }
    ui.busy = true; redraw();
    try {
      await put('/password', { current: ui.current, new: ui.next });
      ui.current = ui.next = ui.again = '';
      toast('Password changed. Other browsers were signed out.');
    } catch (e) { ui.error = e.message; }
    ui.busy = false; redraw();
  };
  return [
    panel('Where this page answers', {},
      h('div.form',
        toggle('Also on the node’s uplink network', 'Off: only from the mesh and from this node’s own Wi-Fi and device ports. On: also from the network its uplink is plugged into. Until Trellis has its firewall, the admin password is all that protects it there.',
          !!d.web.wan_access, (v) => { d.web.wan_access = v; redraw(); }),
        kv([
          ['From its own devices', h('span', 'https://', st.client_network.anycast.address || '10.73.0.1', h('span.sans.dim', '  or  '), 'https://node.mesh')],
          ['From the mesh', h('span', 'https://', st.node.address.split('/')[0], h('span.sans.dim', '  or  '), 'https://', st.node.callsign, '.mesh')],
        ]),
        h('p.faint', { style: { fontSize: '13.5px', maxWidth: '75ch' } }, 'The addresses always work. The .mesh names only resolve on a device that asks this node for names: a phone with Private DNS set to a provider, a browser with secure DNS, or a VPN asks elsewhere and reports that the site cannot be found.'))),
    panel('Admin password', {},
      h('div.form',
        h('div.fields',
          field('Current password', h('input.input', { type: 'password', value: ui.current, autocomplete: 'current-password', oninput: (e) => { ui.current = e.target.value; redraw(); } })),
          field('New password', h('input.input', { type: 'password', value: ui.next, autocomplete: 'new-password', oninput: (e) => { ui.next = e.target.value; redraw(); } }), { hint: 'At least 8 characters.' }),
          field('New password again', h('input.input', { type: 'password', value: ui.again, autocomplete: 'new-password', oninput: (e) => { ui.again = e.target.value; redraw(); } }))),
        ui.error ? h('p.bad', ui.error) : null,
        h('div.row.between',
          h('span.faint', { style: { fontSize: '13.5px' } }, 'Forgotten? On the node: ', h('code', 'sudo meshctl web password')),
          button(ui.busy ? 'Changing…' : 'Change password', change, 'primary', { disabled: ui.busy || !ui.current || ui.next.length < 8 })))),
  ];
}

function systemSection(d, st) {
  const ui = (state.ui.system ||= { report: null, busy: false });
  const doctor = async () => {
    ui.busy = true; redraw();
    try { ui.report = (await get('/doctor')).report; } catch (e) { toast(e.message, 'bad', 6000); }
    ui.busy = false; redraw();
  };
  const b = st.batman;
  return [
    panel('This node', {},
      kv([
        ['Trellis', st.version],
        ['Host', st.hostname],
        ['Running for', since(st.started_at)],
        ['batman-adv', `${b.module_version || '–'} · ${b.algo || '–'}` + (b.pinned_mismatch ? `  (the mesh pins ${st.profile.pinned_batman})` : '')],
        ['Regulatory', `${st.regulatory.active_mode}` + (st.regulatory.kernel_alpha2 ? ` · kernel domain ${st.regulatory.kernel_alpha2}` : '') +
          ({ trellis: ' · rules from Trellis', kernel: ' · rules from the kernel’s regulatory.db' }[st.regulatory.source] || '') + (st.regulatory.note ? ` · ${st.regulatory.note}` : '')],
        ['Gossip', `${count(st.gossip.sent)} sent · ${count(st.gossip.received)} received · ${st.gossip.auth_failed} refused`],
        ['meshd', `pid ${st.process.pid} · ${st.process.open_files} files · ${st.process.goroutines} goroutines`],
      ])),
    panel('Control loop', { flush: true, meta: st.reconcile.converged ? `settled · pass ${count(st.reconcile.passes)} took ${st.reconcile.last_duration}` : 'still applying changes',
      },
      table({ stack: true, key: (r) => r.name }, [
        { title: 'Part', cell: (r) => h('span.primary', r.name) },
        { title: 'Changes', cls: 'num', cell: (r) => count(r.changes) },
        { title: 'Last change', cls: 'num', cell: (r) => (Date.parse(r.last_changed) > 86400000 ? since(r.last_changed) + ' ago' : h('span.faint', 'never')) },
        { title: 'State', cls: 'wide', wide: true, cell: (r) => (r.last_error ? h('span.bad', r.last_error) : h('span.ok', 'ok')) },
      ], st.reconcile.reconcilers || [])),
    panel('Diagnostics', { tools: [
        button('Apply config again', async () => { try { await post('/reconcile'); toast('The node is checking everything again.'); } catch (e) { toast(e.message, 'bad'); } }, 'small'),
        button('Status as JSON', () => download(`${st.node.callsign}-status.json`, JSON.stringify(st, null, 2), 'application/json'), 'small', { icon: 'download' }),
        button(ui.busy ? 'Running…' : 'Run host checks', doctor, 'small primary', { disabled: ui.busy })] },
      ui.report
        ? [h('pre.block', ui.report), h('div.row.end', { style: { marginTop: '10px' } }, button('Copy report', async () => toast((await copyText(ui.report)) ? 'Report copied.' : 'Could not copy.'), 'small', { icon: 'copy' }))]
        : h('p.dim', { style: { fontSize: '14px' } }, 'The host checks are what ', h('code', 'meshctl doctor'), ' prints: kernel, batman-adv, helper programs, radios and configuration, in a form to paste into a report.')),
  ];
}

export function settings() {
  const st = state.status;
  const c = ensureConfig();
  const sec = SECTIONS.some(([id]) => id === state.sub) ? state.sub : 'node';
  const head = h('header.page-head',
    h('div', h('h1', 'Settings'), h('p.sub', 'Changes are checked first. When one would interrupt something, you see what before it is applied.')));
  const tabs = h('div.seg', { style: { alignSelf: 'flex-start', maxWidth: '100%', overflowX: 'auto' } },
    SECTIONS.map(([id, label]) => h('button', { 'aria-pressed': String(sec === id), onclick: () => { location.hash = '#/settings/' + id; } }, label)));
  if (!c.draft || !state.config) {
    return h('div.page', head, tabs, h('div.empty', c.error ? ['Could not load the configuration: ', c.error, ' ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); c.error = null; loadConfig(); } }, 'Try again')] : 'Loading the configuration…'));
  }
  const body = { node: nodeSection, wifi: wifiSection, internet: internetSection, ports: portsSection, profile: profileSection, access: accessSection, system: systemSection }[sec](c.draft, st);
  return h('div.page', head, tabs, h('div', { key: sec, style: { display: 'flex', flexDirection: 'column', gap: 'var(--gap)' } }, body), saveBar());
}
