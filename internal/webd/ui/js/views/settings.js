// Settings: everything about this node that an operator may change.
import { h } from '../dom.js';
import { get, put, post } from '../api.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { panel, field, toggle, button, kv, table, qr, notice, copyText, download, pill, asSentence } from '../ui.js';
import { bandName, since, duration, count, plural, meshLinks, meshRadios, carried, fewerRadios, gnssShort } from '../fmt.js';
import { ensureConfig, cfg, saveBar, saveProfile, saveMeshProfile, loadConfig, changeLock, editProfile, sentPending, noteUnsaved } from '../config.js';
import { channelsSection } from './channels.js';
import { appearanceSection } from './appearance.js';
import { softwarePanel, meshUpgradePanel } from './software.js';
import { trialsPanel } from './trials.js';
import { hardwarePanel, controlLoopPanel } from './hardware.js';
import { linkFields, linkLine, linkOrder, nextLink } from '../links.js';
import { clock, stamp } from '../time.js';

const SECTIONS = [
  ['node', 'Node'], ['wifi', 'Wi-Fi'], ['internet', 'Internet'], ['ports', 'Ports'],
  ['profile', 'Mesh profile'], ['channels', 'Channels'], ['access', 'Access'], ['system', 'System'],
  ['appearance', 'Appearance'],
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
        { error: err('callsign'), hint: `Lowercase letters, digits, hyphens. DNS name ${d.callsign || 'callsign'}.mesh; default client AP SSID.` }),
      field('Node ID', h('input.input', { type: 'number', min: '0', max: '254', value: d.node_id || '', placeholder: `auto (current: ${st.node.node_id})`,
        oninput: (e) => { d.node_id = e.target.value === '' ? 0 : Number(e.target.value); redraw(); } }),
        { error: err('node_id'), hint: `1–254, unique in the mesh. Sets the node address (10.73.ID.1). Empty: automatic, with conflict resolution.` }))),
  panel('Team map', {},
    h('div.form',
      toggle('Report node position to ATAK', 'With a GNSS fix, the node appears on ATAK maps as an equipment marker; last known position kept if it leaves the mesh. Position data stays in the mesh.',
        d.positions?.share !== false, (v) => { d.positions = v ? {} : { share: false }; redraw(); }),
      st.profile.positions_shared === false ? notice({ level: 'info', message: 'Disabled for all nodes by the mesh profile.' }) : null)),
  panel('Advanced', {},
    h('div.form',
      h('div.fields',
        field('Mesh link MTU', input(d, 'mesh_mtu', { type: 'number', min: '1280', max: '2304' }), { error: err('mesh_mtu'), hint: '1560: full-size packets plus batman-adv headers.' })),
      toggle('Allow legacy batman-adv', 'Testing only. One legacy node forces mesh-wide multicast flooding.', d.allow_old_batman, (v) => { d.allow_old_batman = v; redraw(); })))];
}

function wifiSection(d, st) {
  const ap = d.ap;
  const kept = ap.passphrase === '<redacted>';
  return [panel('Client AP', {},
    h('div.form',
      toggle('Enable client AP', 'Per-node access point for clients.', ap.enabled !== false, (v) => { ap.enabled = v; redraw(); }),
      h('div.fields',
        field('Network name', input(ap, 'ssid', { maxlength: '32', placeholder: d.callsign + ' (the callsign)', invalid: err('ap.ssid') }), { error: err('ap.ssid'), hint: 'Empty: callsign.' }),
        field('Passphrase', h('input.input', { type: 'text', value: kept ? '' : ap.passphrase || '', placeholder: kept ? 'set (type to replace)' : 'auto-generated', maxlength: '63',
          autocomplete: 'off', class: err('ap.passphrase') ? 'invalid' : '', oninput: (e) => { ap.passphrase = e.target.value; redraw(); } }),
          { error: err('ap.passphrase'), hint: kept ? h('span', 'Custom passphrase set. ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); ap.passphrase = ''; redraw(); } }, 'Revert to generated')) : '8–63 characters. Empty: auto-generated.' })),
      h('div.fields',
        field('Band', select(ap, 'band', [['5g', '5 GHz (preferred)'], ['2g', '2.4 GHz']], { after: () => { ap.channel = 0; } }),
          { error: err('ap.band'), hint: 'Falls back to 2.4 GHz if the radio cannot run a 5 GHz AP. Never on the mesh channel.' }),
        field('Channel', h('input.input', { type: 'number', min: '0', value: ap.channel || '', placeholder: 'automatic', oninput: (e) => { ap.channel = e.target.value === '' ? 0 : Number(e.target.value); redraw(); } }),
          { error: err('ap.channel'), hint: 'Empty: automatic, non-DFS. Any supported channel; DFS channels start after a CAC of 1 min or more.' }),
        // Ignored since the node has one regulatory domain (the profile's):
        // shown only to clear an old value that differs from it.
        ap.country && ap.country.toUpperCase() !== st.regulatory.country ? field('Country (ignored)', input(ap, 'country', { maxlength: '2', autocapitalize: 'characters' }),
          { error: err('ap.country'), hint: h('span', `AP uses the mesh profile country (${st.regulatory.country}). `, h('a', { href: '#', onclick: (e) => { e.preventDefault(); ap.country = ''; redraw(); } }, 'Clear')) }) : null),
      toggle('Client isolation', 'Clients reach the mesh but not each other.', !!ap.isolate, (v) => { ap.isolate = v; redraw(); })))];
}

function internetSection(d, st) {
  const g = d.gateway;
  const ui = (state.ui.settings ||= {});
  if (ui.probes == null || ui.probesFor !== cfg().base) { ui.probes = (g.probe_targets || []).join(', '); ui.probesFor = cfg().base; }
  return [panel('Uplink sharing', {},
    h('div.form',
      toggle('Act as gateway', 'Share a working uplink with the mesh. Off: use other gateways only.', g.mode !== 'off', (v) => { g.mode = v ? 'auto' : 'off'; redraw(); }),
      h('div.fields',
        field('Uplink interface', input(g, 'wan', { placeholder: 'automatic' + (st.gateway.wan ? ` (current: ${st.gateway.wan})` : ''), maxlength: '15', invalid: err('gateway.wan') }),
          { error: err('gateway.wan'), hint: 'Empty: uplink-role port, else the default route interface.' }),
        field('Announced downlink (Mbit/s)', input(g, 'down_mbit', { type: 'number', min: '1' }), { error: err('gateway.down_mbit') }),
        field('Announced uplink (Mbit/s)', input(g, 'up_mbit', { type: 'number', min: '1' }), { error: err('gateway.up_mbit'), hint: 'Used with path quality in gateway selection.' })))),
  panel('Uplink health check', {},
    h('div.fields',
      field('Probe targets', h('input.input', { value: ui.probes, class: Object.keys(cfg().fields).some((k) => k.startsWith('gateway.probe_targets')) ? 'invalid' : '',
        oninput: (e) => { ui.probes = e.target.value; g.probe_targets = e.target.value.split(/[\s,]+/).filter(Boolean); redraw(); } }),
        { error: Object.entries(cfg().fields).filter(([k]) => k.startsWith('gateway.probe_targets')).map(([, v]) => v)[0], hint: 'Uplink healthy while any target responds.' }),
      field('Interval (ms)', input(g, 'probe_interval_ms', { type: 'number', min: '200', max: '10000' }), { error: err('gateway.probe_interval_ms'), hint: '3 consecutive misses: uplink down.' })))];
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
      { title: 'Now', cell: (n) => { const p = (st.ports || []).find((x) => x.name === n); return p ? h('span', pill(p.role, 'plain'), p.carrier ? h('span.ok', ' link up') : h('span.faint', ' no link')) : h('span.faint', 'not present'); } },
      { title: 'Role', cls: 'wide', wide: true, cell: (n) => h('select.select', { value: roleOf(n), onchange: (e) => setRole(n, e.target.value), style: { maxWidth: '340px' } },
        h('option', { value: '' }, 'Unlisted: automatic, shared with the system'),
        h('option', { value: 'auto' }, 'Automatic: role detected by Trellis'),
        h('option', { value: 'client' }, 'Devices: always a client port'),
        h('option', { value: 'wan' }, 'Uplink: always the internet uplink'),
        h('option', { value: 'mesh' }, 'Mesh: wired backhaul to another node')) },
    ], names, 'No Ethernet ports detected.'),
    ),
  h('p.dim', { style: { fontSize: '14px', maxWidth: '75ch' } },
    'Automatic: client port for a host or switch; uplink when a DHCP-serving router is detected. ',
    'Changing the role of the port serving this session may disconnect it.')];
}

async function shareProfile() {
  let ex;
  try { ex = await get('/profile/export'); } catch (e) { toast(e.message, 'bad', 6000); return; }
  openDialog({
    title: 'Share mesh profile',
    body: () => [
      notice({ level: 'warning', message: 'Contains the mesh secret: grants mesh access. Share with authorized personnel only.' }),
      h('div.join', qr(ex.qr, 'Mesh profile QR code'),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
          h('p.dim', { style: { fontSize: '14px' } }, 'On the new node: Setup → “Join a mesh”, then scan or paste the profile code.'),
          h('div.row',
            button('Copy profile code', async () => toast((await copyText(ex.code)) ? 'Profile code copied.' : 'Copy failed. Select and copy manually.'), 'small', { icon: 'copy' }),
            button('Download file', () => download(`${ex.profile.name}-profile.yaml`, ex.yaml, 'application/yaml'), 'small', { icon: 'download' })))),
      h('pre.block', { style: { maxHeight: '90px' } }, ex.code),
    ],
    actions: [{ label: 'Done', kind: 'primary', run: closeDialog }],
  });
}

// meshChangePanel shows a mesh-wide profile change: when every node switches,
// and on the node that sent it, where each node stands.
function meshChangePanel(mc) {
  const at = new Date(mc.apply_at);
  const secs = Math.max(0, Math.round((at - Date.now()) / 1000));
  const when = mc.state === 'pending'
    ? `Switchover at ${clock(at)} (in ${secs} s).`
    : mc.state === 'applied' ? `Active since ${clock(at)}.` : `Failed on this node: ${mc.error}`;
  const nodes = mc.nodes || [];
  const have = nodes.filter((n) => n.state === 'has it').length;
  return panel('Mesh change', { meta: `rev ${mc.revision} · from ${mc.sender ? 'this node' : mc.from}` },
    notice({ level: mc.state === 'failed' ? 'error' : mc.state === 'pending' ? 'warning' : 'ok', message: `${mc.summary ? mc.summary + '. ' : ''}${when}` }),
    nodes.length ? h('div', { style: { marginTop: '12px' } },
      h('div.label', { style: { marginBottom: '6px' } }, `${have}/${nodes.length} nodes received`),
      h('div.others-list', nodes.map((n) => h('div', h('span.mono', n.callsign),
        h('span', { class: n.state === 'has it' ? 'ok' : n.state.startsWith('refused') ? 'bad' : 'dim' }, n.state))))) : null);
}

function profileSection(d, st) {
  const ui = (state.ui.profile ||= { draft: null, fields: {}, busy: false, replace: false, code: '', scope: null, delay: 60 });
  // A change goes to every node unless the operator chooses this node only
  // (or no other node is known).
  ui.scope ||= (st.nodes || []).length ? 'mesh' : 'node';
  const cur = editProfile();
  if (!ui.draft || ui.for !== JSON.stringify(cur)) { ui.draft = JSON.parse(JSON.stringify(cur)); ui.for = JSON.stringify(cur); ui.fields = {}; }
  const p = ui.draft;
  const changed = JSON.stringify(p) !== ui.for;
  // Only a change this editor sent makes its draft "sent".
  const mine = sentPending('profile', st);
  noteUnsaved('profile', changed && !mine);
  const send = async (input, scope = 'node') => {
    ui.busy = true; ui.fields = {}; redraw();
    try {
      const res = scope === 'mesh' ? await saveMeshProfile(input, ui.delay, 'profile') : await saveProfile(input);
      if (res && res.applied) { ui.replace = false; ui.code = ''; }
    } catch (e) {
      for (const f of e.fields || []) ui.fields[f.field] = f.message;
      toast(e.message.split('\n')[0], 'bad', 7000);
    }
    ui.busy = false; redraw();
  };
  // A mesh-wide change on its way: another sent to every node now would
  // replace it and drop what it changes (the node refuses that). Changing
  // this node only stays possible.
  const waits = !!changeLock(st) && ui.scope === 'mesh';
  const unrestricted = p.regulatory.mode === 'unrestricted';
  // Which of the profile's links this node has a radio for, and what the
  // other nodes say about their radios: what a channel change has to fit.
  const live = meshLinks(st);
  const many = (cur.links || []).length > 0;
  const here = (i) => (live[i] && !carried(live[i]) && st.mesh_link.transport !== 'undetermined'
    ? h('span.faint', '  ·  no radio on this node') : null);
  const others = st.nodes || [];
  const myBands = [...new Set((st.radios || []).filter((r) => r.mesh_point).flatMap((r) => r.bands || []))];
  return [
    st.mesh_change ? meshChangePanel(st.mesh_change) : null,
    panel('Mesh', { tools: [button('Share profile', shareProfile, 'small primary')] },
      kv([
        ['Mesh', h('span', cur.name, h('span.faint', `  ·  mesh ID ${cur.mesh_id}`))],
        // The links by name, in the profile's order (the order a node with
        // fewer radios takes them in).
        [many ? 'Mesh links' : 'Mesh link', h('div', [cur.radio, ...(cur.links || [])].map((l, i) =>
          h('div', linkLine({ ...l, running_width_mhz: live[i] && live[i].running_width_mhz }), here(i))))],
        ['Regulatory', cur.regulatory.mode === 'unrestricted' ? h('span.warn', 'unrestricted') : `country (${cur.regulatory.country})`],
        ['Addresses', cur.prefix],
        // The site link is part of the profile: its members are edited
        // on the Internet page, where its state shows.
        ['Site link', (cur.site_link && (cur.site_link.members || []).length)
          ? h('span', `${cur.site_link.members.length} member${cur.site_link.members.length === 1 ? '' : 's'} at ${new Set(cur.site_link.members.map((m) => m.site)).size} site${new Set(cur.site_link.members.map((m) => m.site)).size === 1 ? '' : 's'}`,
              h('span.faint', '  ·  '), h('a', { href: '#/internet' }, 'Status and members →'))
          : h('span', h('span.sans.dim', 'none'), h('span.faint', '  ·  '), h('a', { href: '#/internet' }, 'Configure →'))],
        ['Revision', h('span', String(cur.revision || 0), h('span.sans.dim', cur.change && cur.change.by ? `  ·  last mesh-wide change: ${cur.change.by}, ${stamp(cur.change.at)}` : '  ·  no mesh-wide change'))],
        ['Secret', h('span.sans.dim', 'stored on the node; shared via “Share profile” only')],
      ]),
      // A node that cannot carry every link uses some of them only.
      fewerRadios(st) ? h('div', { style: { marginTop: '14px' } }, notice({ level: 'info', message: fewerRadios(st).lead + ' Radio assignment: Radios page.' },
        h('a', { href: '#/radios', style: { marginLeft: '6px' } }, 'Radios →'))) : null),
    panel('Channel and regulatory', { meta: ui.scope === 'mesh' ? 'all nodes' : `${st.node.callsign} only` },
      h('div.form',
        ui.scope === 'mesh'
          ? [notice({ level: 'info', message: `Sent to all known nodes (${plural(others.length, 'other node')}); simultaneous switchover. Out-of-range nodes update on next contact if they share a link; the review lists nodes that cannot follow.` }),
            // Habits that keep a mesh-wide change from splitting the mesh.
            h('p', { style: { fontSize: '13px', color: 'var(--ink-3)', lineHeight: '1.45', margin: '0', maxWidth: '85ch' } },
              'Send changes with all nodes up and in range, from one location at a time. A node that misses a change must rediscover the mesh. If partitioned groups both change the mesh, the later change wins on merge.')]
          : notice({ level: 'warning', message: others.length
            ? `Applies to ${st.node.callsign} only; not sent to the other ${others.length === 1 ? 'node' : others.length + ' nodes'}. A node with a different channel or secret is cut off from the mesh.`
            : 'Applies to this node only. All nodes need the same profile; a different channel or secret cuts a node off.' }),
        others.length ? h('div.others',
          h('div.label', 'Mesh radios on other nodes'),
          h('div.others-list', others.map((n) => {
            const r = meshRadios(n);
            return h('div', h('span.mono', n.callsign), h('span', { class: r ? 'dim' : 'faint', title: r ? null : 'Older version: radios not reported.' }, r || 'not reported'));
          })),
          h('span.hint', 'A link reaches only nodes with a radio for its band. One link per radio.')) : null,
        linkFields(p.radio, 'radio', ui.fields, { title: (p.links || []).length ? linkOrder(0) : '', country: p.regulatory.country, others: p.links || [] }),
        (p.links || []).map((l, i) => h('div.link-row',
          linkFields(l, `links[${i}]`, ui.fields, { title: linkOrder(i + 1), country: p.regulatory.country, others: [p.radio, ...p.links.filter((x) => x !== l)] }),
          h('div.row.end',
            // Swap with the link before: the order is which link a node
            // with too few radios takes first.
            button('Move up', () => { const all = [p.radio, ...p.links]; [all[i], all[i + 1]] = [all[i + 1], all[i]]; p.radio = all[0]; p.links = all.slice(1); redraw(); }, 'small'),
            button('Remove link', () => { p.links.splice(i, 1); if (!p.links.length) delete p.links; redraw(); }, 'small')))),
        h('div.row',
          button('Add a mesh link', () => { (p.links ||= []).push(nextLink([p.radio, ...p.links], p.regulatory.country, myBands)); redraw(); }, 'small', { disabled: 1 + (p.links || []).length >= 10 }),
          h('span.hint', 'One radio per link, typically one per band. Routing uses the fastest path across links. ',
            (p.links || []).length ? 'Order applies to nodes with fewer radios than links: links are taken in order. Put the fastest link first. Nodes on different links connect through a node carrying both.' : '')),
        h('div.fields',
          field('Regulatory mode', select(p.regulatory, 'mode', [['country', 'Country: national channel and power limits'], ['unrestricted', 'Unrestricted: hardware limits only']]), { error: ui.fields['regulatory.mode'] }),
          field('Country', input(p.regulatory, 'country', { maxlength: '2', autocapitalize: 'characters' }), { error: ui.fields['regulatory.country'], hint: 'ISO 3166 code, e.g. US.' })),
        toggle('Node positions to ATAK', 'Nodes with a GNSS fix report position to ATAK; last known positions retained. Off: all nodes stop reporting and last known positions are cleared (EMCON). EUD positions are set in ATAK.',
          p.positions?.share !== false, (v) => { p.positions = v ? {} : { share: false }; redraw(); }),
        unrestricted ? notice({ level: 'warning', message: 'Unrestricted mode requires frequency authorization. Without it, transmitting outside licence-exempt rules is unlawful in most countries.' }) : null,
        h('div.row.between', { style: { flexWrap: 'wrap', gap: '12px' } },
          h('div.row', { style: { flexWrap: 'wrap', gap: '12px' } },
            h('div.seg', [['mesh', 'All nodes'], ['node', 'This node only']].map(([id, label]) =>
              h('button', { 'aria-pressed': String(ui.scope === id), disabled: id === 'mesh' && !others.length, onclick: () => { ui.scope = id; redraw(); } }, label))),
            ui.scope === 'mesh' ? h('label.row', { style: { gap: '8px' } }, h('span.dim', 'Switchover in'),
              h('select.select', { value: String(ui.delay), onchange: (e) => { ui.delay = Number(e.target.value); } },
                [[30, '30 seconds'], [60, '1 minute'], [120, '2 minutes'], [300, '5 minutes']].map(([v, l]) => h('option', { value: String(v) }, l)))) : null),
          h('div.row',
            changed && !mine ? button('Discard', () => { ui.draft = null; redraw(); }, '', { disabled: ui.busy }) : null,
            button(ui.busy ? 'Checking…' : waits ? 'Waiting for switchover…' : ui.scope === 'mesh' ? 'Review and send' : 'Review and change', () => send({ profile: p }, ui.scope), 'primary',
              { disabled: !changed || ui.busy || waits, title: waits ? 'Mesh change pending: a new change replaces it' : null }))))),
    panel('Join another mesh', {},
      ui.replace
        ? h('div.form',
            field('Profile code', h('textarea.textarea', { value: ui.code, placeholder: 'TRP1:…', spellcheck: 'false', oninput: (e) => { ui.code = e.target.value; redraw(); } }),
              { hint: 'From the other mesh’s “Share profile”, or its profile file contents.' }),
            h('div.row.end',
              button('Cancel', () => { ui.replace = false; ui.code = ''; redraw(); }),
              button('Review and install', () => send(ui.code.trim().startsWith('TRP1:') ? { code: ui.code.trim() } : { yaml: ui.code }), 'danger', { disabled: !ui.code.trim() || ui.busy })))
        : h('div.row.between',
            h('p.dim', { style: { fontSize: '14px', maxWidth: '60ch' } }, 'Installing another mesh profile moves this node to that mesh.'),
            button('Install another profile', () => { ui.replace = true; redraw(); }))),
  ];
}

function accessSection(d, st) {
  const ui = (state.ui.access ||= { current: '', next: '', again: '', busy: false, error: '' });
  const change = async () => {
    ui.error = '';
    if (ui.next !== ui.again) { ui.error = 'Passwords do not match.'; redraw(); return; }
    ui.busy = true; redraw();
    try {
      await put('/password', { current: ui.current, new: ui.next });
      ui.current = ui.next = ui.again = '';
      toast('Password changed. Other sessions signed out.');
    } catch (e) { ui.error = e.message; }
    ui.busy = false; redraw();
  };
  return [
    panel('Web UI access', {},
      h('div.form',
        toggle('Allow access from uplink network', 'Off: mesh, client AP and client ports only. On: also the uplink network (password-protected).',
          !!d.web.wan_access, (v) => { d.web.wan_access = v; redraw(); }),
        kv([
          ['From clients', h('span', 'https://', st.client_network.anycast.address || '10.73.0.1', h('span.sans.dim', '  or  '), 'https://node.mesh')],
          ['From the mesh', h('span', 'https://', st.node.address.split('/')[0], h('span.sans.dim', '  or  '), 'https://', st.node.callsign, '.mesh')],
        ]),
        h('p.faint', { style: { fontSize: '13.5px', maxWidth: '75ch' } }, 'IP addresses always work. .mesh names resolve only via this node’s DNS; Private DNS, browser secure DNS or a VPN bypass it.'))),
    firewallPanel(d, st),
    panel('Admin password', {},
      h('div.form',
        h('div.fields',
          field('Current password', h('input.input', { type: 'password', value: ui.current, autocomplete: 'current-password', oninput: (e) => { ui.current = e.target.value; redraw(); } })),
          field('New password', h('input.input', { type: 'password', value: ui.next, autocomplete: 'new-password', oninput: (e) => { ui.next = e.target.value; redraw(); } }), { hint: 'Min. 8 characters.' }),
          field('Confirm new password', h('input.input', { type: 'password', value: ui.again, autocomplete: 'new-password', oninput: (e) => { ui.again = e.target.value; redraw(); } }))),
        ui.error ? h('p.bad', ui.error) : null,
        h('div.row.between',
          h('span.faint', { style: { fontSize: '13.5px' } }, 'Reset on the node: ', h('code', 'sudo meshctl web password')),
          button(ui.busy ? 'Changing…' : 'Change password', change, 'primary', { disabled: ui.busy || !ui.current || ui.next.length < 8 })))),
  ];
}

// firewallPanel: the node's firewall towards everything that is not the
// mesh (firewall.*; nft.Input in internal/nft, table inet trellis_fw).
// Trusted interfaces are never filtered; the rules apply to the rest.
const FW_TRUSTED = 'lo, br-mesh, anycast0';
function firewallPanel(d, st) {
  const fw = (d.firewall ||= { wan: 'open' });
  const closed = fw.wan === 'closed';
  const g = st.gateway || {};
  const ports = (fw.allow || []).join(' ');
  const setPorts = (text) => {
    const list = text.split(/[\s,]+/).map((p) => p.trim().toLowerCase()).filter(Boolean);
    fw.allow = list.length ? list : null;
    redraw();
  };
  // What the rules accept as the form stands (inputPolicy in r_firewall.go).
  const accepts = [...new Set([
    ...(fw.ssh !== false ? ['tcp/22'] : []),
    ...(d.web.wan_access ? ['tcp/80', 'tcp/443'] : []),
    ...(fw.allow || []),
  ])];
  const now = {
    closed: h('span', h('span.ok', 'closed'),
      h('span.faint', `  ·  open: ${(g.firewall_open || []).join(' ') || 'none'}  ·  ${count(g.firewall_dropped || 0)} dropped`)),
    failed: h('span.bad', 'closed, rules failed to apply: node open (see Alerts)'),
  }[g.firewall] || h('span', h('span.warn', 'open'), h('span.faint', '  ·  no Trellis rules; host firewall applies'));
  return panel('Firewall', { meta: 'nftables · inet trellis_fw' },
    h('div.form',
      kv([
        ['Now', now],
        ['Trusted', h('span', h('span.mono', FW_TRUSTED), h('span.faint', '  ·  mesh, client AP, client ports: not filtered'))],
        ['Filtered', h('span.faint', 'all other interfaces: uplink, VPN, containers, other host links')],
      ]),
      toggle('Drop new connections from filtered interfaces', [
        ['Input', 'accept established/related, ICMP/ICMPv6, DHCP replies (udp/68, udp/546) and the ports below; drop all else.'],
        ['Forward', 'drop new connections from filtered interfaces into the mesh; DNAT port forwards pass. Outbound traffic unaffected.'],
        ['Off', 'Trellis installs no rules.'],
      ].map(([k, v]) => h('span', { style: { display: 'block' } }, h('b', k), ': ', v)),
        closed, (v) => { fw.wan = v ? 'closed' : 'open'; redraw(); }),
      closed ? toggle('Accept SSH (tcp/22)', 'Management from the uplink network.', fw.ssh !== false,
        (v) => { fw.ssh = v ? null : false; redraw(); }) : null,
      closed ? field('Also accept', h('input.input', { value: ports, placeholder: 'tcp/5201 udp/51820', spellcheck: false,
        onchange: (e) => setPorts(e.target.value) }), { hint: 'tcp/PORT or udp/PORT, space-separated. tcp/80 and tcp/443 added while “Allow access from uplink network” is on.' }) : null,
      closed ? kv([['Effective', accepts.length
        ? h('span', h('span.mono', accepts.join(' ')), h('span.faint', '  ·  plus replies, ICMP, DHCP'))
        : h('span.faint', 'replies, ICMP and DHCP only')]]) : null));
}

// timePanel: how the node keeps time, and its GNSS receiver.
function timePanel(st) {
  const c = st.clock || {}, g = st.gnss || {};
  const clock = c.service !== 'chrony'
    ? h('span.warn', c.note ? `Not managed · ${c.note}` : 'Not managed')
    : c.synced
      ? h('span', h('span.ok', 'Synced'), ` · ${c.source}`, h('span.faint', ` · stratum ${c.stratum} · offset ${c.offset_ms.toFixed(1)} ms`))
      : h('span', h('span.warn', 'Not synced'), c.note ? h('span.faint', ` · ${c.note}`) : null);
  const rows = [
    ['Clock', clock],
    ['Time server', c.serving ? 'On' : h('span.faint', 'Off')],
  ];
  if ((c.mesh_servers || []).length) rows.push(['Upstream', c.mesh_servers.join(', ')]);
  rows.push(['GNSS', g.receiver
    ? h('span', h('span', { class: g.fix === 'none' ? 'warn' : 'ok' }, gnssShort(g)), h('span.faint', `  ·  ${[g.driver, g.device].filter(Boolean).join(' · ')}`))
    : h('span.faint', g.note || 'No receiver')]);
  if (g.lat != null && g.lon != null) {
    rows.push(['Position', h('span.mono', `${g.lat.toFixed(5)}, ${g.lon.toFixed(5)}` + (g.alt_m != null ? ` · ${Math.round(g.alt_m)} m` : ''))]);
  }
  const ps = st.positions || {};
  if (ps.device_note) rows.push(['Device positions', h('span.warn', ps.device_note)]);
  if (g.receiver || ps.off || ps.device) {
    rows.push(['ATAK PLI', ps.shared
      ? h('span', h('span.ok', 'On'), h('span.faint', (ps.last_sent_ago_ms >= 0 ? `  ·  last sent ${Math.round(ps.last_sent_ago_ms / 1000)} s ago` : '') + (ps.device ? `  ·  position source: device ${ps.device}` : '')))
      : h('span.faint', ps.note ? `Off · ${ps.note}` : 'Off')]);
  }
  if (g.receiver) {
    // NMEA is held back while positions are not shared or there is no
    // current fix: meshd says which, not "no devices".
    rows.push(['External GPS', ps.nmea_devices > 0
      ? h('span', h('span.ok', 'On'), h('span.faint', `  ·  ${ps.nmea_devices} ${ps.nmea_devices === 1 ? 'device' : 'devices'} · NMEA to UDP 4349`))
      : h('span.faint', ps.nmea_held ? `Off · ${ps.nmea_held}` : 'No devices on this node')]);
  }
  if ((ps.last_known || []).length) {
    rows.push(['Last known', h('span', ps.last_known.map((k) => `${k.callsign} (${since(k.heard)} ago)`).join(', '),
      h('span.faint', ps.announcing ? '  ·  held by this node' : '  ·  held by another node'))]);
  }
  if (g.receiver) rows.push(['PPS', g.pps ? h('span.ok', 'Locked') : h('span.faint', 'None · NMEA time only, ~100 ms')]);
  if (g.receiver && g.note) rows.push(['', h('span.faint', g.note)]);
  return panel('Time and position', { meta: 'chrony and gpsd' }, kv(rows));
}

// regulatoryLine is the regulatory mode in force and where its rules come
// from: "Country US · Trellis rules", "Unrestricted".
function regulatoryLine(r) {
  const mode = r.active_mode === 'unrestricted' ? 'Unrestricted' : `Country ${r.country || r.kernel_alpha2 || '–'}`;
  const from = { trellis: 'Trellis rules', kernel: 'kernel regulatory.db' }[r.source];
  return [mode, from, r.kernel_alpha2 && r.kernel_alpha2 !== r.country ? `kernel ${r.kernel_alpha2}` : '', r.note || ''].filter(Boolean).join(' · ');
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
        ['Hostname', st.hostname],
        ['Service uptime', since(st.started_at)],
        ['batman-adv', `${b.module_version || '–'} · ${b.algo || '–'}` + (b.pinned_mismatch ? ` · profile requires ${st.profile.pinned_batman}` : '')],
        ['Regulatory', regulatoryLine(st.regulatory)],
        ['Gossip', `Tx ${count(st.gossip.sent)} · Rx ${count(st.gossip.received)} · auth fail ${st.gossip.auth_failed}`],
        ['meshd', `PID ${st.process.pid} · ${st.process.open_files} FDs · ${st.process.goroutines} goroutines`],
      ])),
    softwarePanel(st),
    meshUpgradePanel(st),
    trialsPanel(st),
    timePanel(st),
    hardwarePanel(st),
    controlLoopPanel(st),
    panel('Diagnostics', { tools: [
        button('Reapply config', async () => { try { await post('/reconcile'); toast('Reapplying configuration'); } catch (e) { toast(e.message, 'bad'); } }, 'small'),
        button('Status JSON', () => download(`${st.node.callsign}-status.json`, JSON.stringify(st, null, 2), 'application/json'), 'small', { icon: 'download' }),
        button(ui.busy ? 'Running…' : 'Run host checks', doctor, 'small primary', { disabled: ui.busy })] },
      ui.report
        ? [h('pre.block', ui.report), h('div.row.end', { style: { marginTop: '10px' } }, button('Copy report', async () => toast((await copyText(ui.report)) ? 'Report copied.' : 'Could not copy.'), 'small', { icon: 'copy' }))]
        : h('p.dim', { style: { fontSize: '14px' } }, 'Host checks (', h('code', 'meshctl doctor'), '): kernel, batman-adv, helpers, radios, configuration. Output formatted for bug reports.')),
  ];
}

export function settings() {
  const st = state.status;
  const c = ensureConfig();
  const sec = SECTIONS.some(([id]) => id === state.sub) ? state.sub : 'node';
  const head = h('header.page-head',
    h('div', h('h1', 'Settings'), h('p.sub', 'Changes are validated before apply; service impact shown for review.')));
  const tabs = h('div.seg', { style: { alignSelf: 'flex-start', maxWidth: '100%', overflowX: 'auto' } },
    SECTIONS.map(([id, label]) => h('button', { 'aria-pressed': String(sec === id), onclick: () => { location.hash = '#/settings/' + id; } }, label)));
  if (!c.draft || !state.config) {
    return h('div.page', head, tabs, h('div.empty', c.error ? ['Configuration load failed: ', c.error, ' ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); c.error = null; loadConfig(); } }, 'Try again')] : 'Loading configuration…'));
  }
  const body = { node: nodeSection, wifi: wifiSection, internet: internetSection, ports: portsSection, profile: profileSection, channels: (d, st) => channelsSection(st), access: accessSection, system: systemSection, appearance: () => appearanceSection() }[sec](c.draft, st);
  return h('div.page', head, tabs, h('div', { key: sec, style: { display: 'flex', flexDirection: 'column', gap: 'var(--gap)' } }, body), saveBar());
}
