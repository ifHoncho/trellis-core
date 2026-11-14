// Devices: the end-user devices on this node, and how one joins.
import { h } from '../dom.js';
import { get } from '../api.js';
import { state, redraw, toast } from '../state.js';
import { panel, table, bars, pill, qr, kv, button, copyText, stat } from '../ui.js';
import { duration, bandName, mbps, plural } from '../fmt.js';

async function loadJoin() {
  const ui = state.ui.devices;
  try {
    ui.join = await get('/ap');
    ui.error = null;
  } catch (e) {
    ui.error = e.message;
  }
  ui.loading = false;
  redraw();
}

const ROLE_TEXT = {
  wan: 'uplink', client: 'devices', mesh: 'mesh (wired backhaul)', probing: 'looking…', 'no-link': 'no cable',
  peer: 'another Trellis node', 'in-use': 'used by the system', missing: 'not present',
};

export function devices() {
  const st = state.status;
  const ui = (state.ui.devices ||= { join: null, loading: false, reveal: false, ssid: null });
  // The join code is fetched once, and again when the Wi-Fi's name changes.
  if (st.ap.enabled && !ui.loading && (!ui.join || ui.ssid !== st.ap.ssid)) {
    ui.loading = true;
    ui.ssid = st.ap.ssid;
    loadJoin();
  }
  const clients = st.ap.clients || [];
  const wired = (st.client_network.leases || []).filter((l) => !clients.some((c) => c.mac === l.mac) && Date.parse(l.expires) > Date.now());
  const j = ui.join;
  const dhcp = st.client_network.dhcp;

  const joinPanel = st.ap.enabled
    ? panel('Join this node’s Wi-Fi', { class: 'c5', meta: st.ap.interface ? `${bandName(st.ap.band)} · ch ${st.ap.channel}` : '' },
        h('div.join',
          j ? qr(j.qr, `Wi-Fi QR code for ${j.ssid}`) : h('div.qr'),
          h('div', { style: { display: 'flex', flexDirection: 'column', gap: '14px' } },
            stat('Network', st.ap.ssid || (j && j.ssid) || '–', '', { text: true }),
            h('div.stat', h('span.label', 'Passphrase'),
              h('div.secret', h('code', { class: ui.reveal ? '' : 'masked' }, j ? (ui.reveal ? j.passphrase : '•'.repeat(10)) : '…'),
                j ? h('button.btn.small.ghost', { onclick: () => { ui.reveal = !ui.reveal; redraw(); } }, ui.reveal ? 'Hide' : 'Show') : null,
                j ? h('button.btn.small.ghost', { onclick: async () => toast((await copyText(j.passphrase)) ? 'Passphrase copied.' : 'Could not copy: select it instead.') }, 'Copy') : null)),
            h('p.dim', { style: { fontSize: '13.5px' } }, 'Point a phone’s camera at the code to join. Each node has its own Wi-Fi, so a phone stays with its owner’s node.'),
            j && j.note ? h('p.warn', { style: { fontSize: '13.5px' } }, j.note) : null,
            ui.error ? h('p.bad', { style: { fontSize: '13.5px' } }, ui.error) : null)))
    : panel('Client Wi-Fi', { class: 'c5' },
        h('p.dim', 'The client Wi-Fi is switched off on this node. Devices can still join by cable.'),
        h('p', { style: { marginTop: '10px' } }, h('a', { href: '#/settings/wifi' }, 'Wi-Fi settings →')));

  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Devices'),
      h('p.sub', `${plural(clients.length + wired.length, 'device')} on this node. Addresses come from ${dhcp.pool_start || '–'} to ${dhcp.pool_end ? dhcp.pool_end.split('.').pop() : '–'}; a device keeps its address when it moves to another node.`))),

    h('div.grid',
      joinPanel,
      panel('On Wi-Fi', { class: 'c7', flush: true, meta: st.ap.hostapd.state ? `access point ${st.ap.hostapd.state.toLowerCase()}` : '' },
        table({ stack: true, key: (c) => c.mac }, [
          { title: 'Device', cls: 'wide', wide: true, cell: (c) => h('div', h('div.primary', c.hostname || 'unnamed device'), h('div.secondary', `${c.ip || 'no address yet'} · ${c.mac}`)) },
          { title: 'Signal', cell: (c) => h('span.nowrap', bars(c.signal_dbm), ' ', h('span.mono.dim', `${c.signal_dbm} dBm`)) },
          { title: 'Rate', cls: 'num', cell: (c) => h('span', mbps(c.tx_mbps), h('span.faint', ' Mbit/s')) },
          { title: 'Joined', cls: 'num', cell: (c) => duration(c.connected_s) },
        ], clients, st.ap.enabled ? 'No device has joined yet.' : 'The client Wi-Fi is off.'))),

    h('div.grid',
      panel('By cable', { class: 'c5', flush: true },
        table({ stack: true, key: (l) => l.mac }, [
          { title: 'Device', cls: 'wide', wide: true, cell: (l) => h('div', h('div.primary', l.hostname || 'unnamed device'), h('div.secondary', `${l.ip} · ${l.mac}`)) },
          { title: 'Lease', cls: 'num', cell: (l) => duration((Date.parse(l.expires) - Date.now()) / 1000) + ' left' },
        ], wired, 'No wired device holds an address from this node.')),
      panel('Ethernet ports', { class: 'c7', flush: true, meta: h('a', { href: '#/settings/ports' }, 'Port roles →') },
        table({ stack: true, key: (p) => p.name }, [
          { title: 'Port', cell: (p) => h('span.primary', p.name) },
          { title: 'Used for', cell: (p) => pill(ROLE_TEXT[p.role] || p.role, p.role === 'client' || p.role === 'wan' || p.role === 'mesh' ? 'accent' : 'plain') },
          { title: 'Cable', cell: (p) => (p.carrier ? h('span.ok', 'connected') : h('span.faint', 'none')) },
          { title: 'Why', cls: 'wide', wide: true, cell: (p) => h('span.dim', { style: { fontSize: '13.5px' } }, [p.configured ? `set to ${p.configured}` : 'automatic', p.why].filter(Boolean).join(' · ')) },
        ], st.ports || [], 'This node has no Ethernet ports Trellis can see.'))));
}
