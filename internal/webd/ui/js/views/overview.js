// Overview: is this node all right, and what is it part of?
import { h } from '../dom.js';
import { state, history, alerts, nameOf, shownWarnings } from '../state.js';
import { panel, stat, dot, pill, spark, notice, bars } from '../ui.js';
import { mbps, since, bandName, roleKind, roleLink, meshLinks, linkName, linkWidth, linkAt, plural, signal, carried, clockShort, gnssShort } from '../fmt.js';
import { topology } from '../topo.js';
import { rolloutSummary } from './software.js';

// headline is one sentence on the node's state, most serious first.
export function headline(st) {
  const shown = shownWarnings(st);
  const errs = shown.filter((w) => w.level === 'error').length;
  const warns = shown.filter((w) => w.level === 'warning').length;
  const peers = (st.batman.neighbors || []).length;
  if (errs) return { tone: 'bad', lead: plural(errs, 'fault'), rest: 'needs attention' };
  if (!st.batman.present) return { tone: 'bad', lead: 'Mesh down', rest: 'batman-adv is not up on this node' };
  if (!st.reconcile.converged) return { tone: 'warn', lead: 'Settling', rest: 'the node is still applying its configuration' };
  if (!peers && (st.nodes || []).length === 0) return { tone: 'warn', lead: 'Alone', rest: 'no other node is in range on this mesh' };
  if (warns) return { tone: 'warn', lead: 'Operating', rest: `with ${plural(warns, 'warning')}` };
  return { tone: 'ok', lead: 'Operating normally', rest: '' };
}

export function internetSummary(st) {
  const g = st.gateway;
  if (g.web_check === 'portal') return { tone: 'warn', value: 'Login page', note: `Uplink ${g.wan} is behind a login page: not shared until someone signs in${g.selected && !g.selected.self ? `; devices use ${g.selected.callsign} meanwhile` : ''}` };
  if (g.selected && g.selected.self) return { tone: 'ok', value: 'Shared', note: `This node's uplink (${g.wan}) serves the mesh` };
  if (g.selected) return { tone: 'ok', value: `Via ${g.selected.callsign}`, note: `${mbps(g.selected.throughput_mbps)} Mbit/s path to that gateway` };
  if (g.wan_known && !g.wan_healthy) return { tone: 'bad', value: 'Down', note: `Uplink ${g.wan} is not answering, and no other gateway is usable` };
  return { tone: 'warn', value: 'None', note: 'No gateway on the mesh. Devices still reach each other.' };
}

export function overview() {
  const st = state.status;
  const hl = headline(st);
  const net = internetSummary(st);
  const nbrs = st.batman.neighbors || [];
  const bestNbr = nbrs.reduce((a, b) => (b.throughput_mbps > (a ? a.throughput_mbps : -1) ? b : a), null);
  const clients = st.ap.clients || [];
  const wired = (st.client_network.leases || []).filter((l) => !clients.some((c) => c.mac === l.mac) && Date.parse(l.expires) > Date.now());
  const mc = st.batman.multicast;
  const mcTone = { efficient: 'ok', partial: 'warn', flooding: 'bad' }[mc.path] || 'warn';
  // A mesh-wide profile change on its way comes first: every node switches.
  const mch = st.mesh_change && st.mesh_change.state === 'pending' ? st.mesh_change : null;
  const change = mch ? [{ level: 'warning', source: 'mesh', code: 'profile change',
    message: `Revision ${mch.revision} of the mesh profile from ${mch.sender ? 'this node' : mch.from}${mch.summary ? ` (${mch.summary})` : ''}: every node switches at ${new Date(mch.apply_at).toLocaleTimeString()}.` }] : [];
  // So does a mesh-wide upgrade under way.
  const ro = st.rollout && !st.rollout.ended ? st.rollout : null;
  if (ro) { const sum = rolloutSummary(ro); change.push({ level: sum.tone, source: 'mesh', code: 'upgrade', message: sum.text }); }
  const notes = [...change, ...alerts(st), ...shownWarnings(st).filter((w) => w.level === 'info')].slice(0, 4);
  const links = meshLinks(st);

  return h('div.page',
    h('section.hero',
      h('div',
        h('div.label', `Node ${st.node.node_id} · mesh ${st.profile.name}`),
        h('h1', st.node.callsign),
        h('div.state', h('span', { class: hl.tone }, dot(hl.tone)), h('span', h('b', hl.lead), hl.rest ? ' ' + hl.rest : ''))),
      h('div.hero-facts',
        h('div', h('span', 'Address'), st.node.address.split('/')[0]),
        h('div', h('span', 'Up'), since(st.started_at)),
        // Every link of the profile, by its band and channel; one this node
        // has no radio for is the mesh's, not this node's, and says so.
        h('div', h('span', links.length > 1 ? 'Mesh links' : 'Mesh link'),
          links.map((l) => h('div', `${linkName(l, st)} · ${linkWidth(l, st)} MHz`,
            l.running_width_mhz && l.running_width_mhz !== linkWidth(l, st) ? h('span.warn', ` (running ${l.running_width_mhz})`) : null,
            links.length > 1 && !carried(l) ? h('span.faint', ' · not on this node') : null))),
        h('div', h('span', 'Time'), h('a', { href: '#/settings/system' }, clockShort(st.clock))),
        st.gnss && st.gnss.receiver ? h('div', h('span', 'GNSS'), h('a', { href: '#/settings/system' }, gnssShort(st.gnss))) : null,
        h('div', h('span', 'Version'), st.version))),

    h('div.grid',
      panel('Mesh', { class: 'c8', flush: true, meta: h('a', { href: '#/mesh' }, 'Topology and routes →') },
        topology(state.topology, { small: true })),
      h('div.c4.stackcol',
        panel('Nodes', {},
          h('div.stats',
            stat('In the mesh', String((st.nodes || []).length + 1)),
            stat('Direct links', String(nbrs.length))),
          h('div', { style: { marginTop: '14px' } }, spark(history('tput'))),
          h('div.stat', { style: { marginTop: '6px' } }, h('span.note',
            bestNbr ? ['Best link ', h('b.mono', mbps(bestNbr.throughput_mbps)), ' Mbit/s to ', nameOf(bestNbr.mac)] : 'No direct links yet'))),
        panel('Internet', { meta: h('a', { href: '#/internet' }, 'Gateways →') },
          stat('For devices on this node', net.value, '', { text: true, tone: net.tone, note: net.note }),
          st.gateway.wan_healthy ? h('div.stats.tight', { style: { marginTop: '14px' } },
            stat('Round trip', st.gateway.rtt_ms ? st.gateway.rtt_ms.toFixed(0) : '–', 'ms'),
            stat('Loss', String(st.gateway.loss_pct), '%')) : null))),

    h('div.grid',
      panel('Devices on this node', { class: 'c4', meta: h('a', { href: '#/devices' }, 'Join code →') },
        h('div.stats',
          stat('Wi-Fi', String(clients.length)),
          stat('Wired', String(wired.length))),
        h('div.people', { style: { marginTop: '10px' } },
          clients.slice(0, 4).map((c) => h('div.person',
            h('div', h('div.mono', c.hostname || c.mac), h('div.faint.mono', { style: { fontSize: '12px' } }, c.ip || 'no address yet')),
            bars(c.signal_dbm))),
          wired.slice(0, 2).map((l) => h('div.person',
            h('div', h('div.mono', l.hostname || l.mac), h('div.faint.mono', { style: { fontSize: '12px' } }, l.ip)),
            h('span.faint.mono', { style: { fontSize: '12px' } }, 'cable'))),
          !clients.length && !wired.length ? h('div.faint', { style: { padding: '8px 0' } },
            st.ap.enabled ? `Nobody yet. Wi-Fi “${st.ap.ssid}” is up.` : 'Nobody yet. The client Wi-Fi is off.') : null)),

      panel('Radios', { class: 'c4', meta: h('a', { href: '#/radios' }, 'Roles →') },
        st.radios.length ? st.radios.map((r) => {
          // The badge says what the radio does; the line under its name,
          // on which link (band and channel), as everywhere else.
          const link = linkAt(st, roleLink(r.role));
          const on = link ? linkName(link, st) : r.role === 'ap' && st.ap.interface ? `${bandName(st.ap.band)} · ch ${st.ap.channel}` : '';
          return h('div.radio-line',
            pill(roleKind(r.role), r.role === 'unused' ? 'plain' : r.role.includes('mesh') ? 'solid' : 'accent'),
            h('div', h('div', { style: { fontSize: '14px' } }, r.label || r.chipset || r.driver), h('div.faint.mono', { style: { fontSize: '12px' } }, [on, r.phy].filter(Boolean).join(' · '))),
            // The best signal on this radio's own link: each link has its own peers.
            link && (link.peers || []).length ? bars(Math.max(...link.peers.map((p) => p.signal_dbm))) : null);
        }) : h('div.faint', st.mesh_link.transport === 'wired' ? 'No radios: the mesh runs over wired ports.' : 'No radios found.')),

      panel('Multicast', { class: 'c4', meta: h('a', { href: '#/multicast' }, 'Counters →') },
        stat('ATAK and other group traffic', mc.path ? mc.path[0].toUpperCase() + mc.path.slice(1) : 'Unknown', '', { text: true, tone: mcTone,
          note: mc.path === 'efficient' ? 'Sent only to nodes with listeners.' : mc.path === 'partial' ? 'Some nodes still receive every group.' : 'Every multicast packet goes to every node.' }),
        h('div', { style: { marginTop: '12px' } }, spark(rates(history('mcast')))))),

    notes.length ? panel('Notices', { meta: h('a', { href: '#/alerts' }, 'All →') }, h('div.notices', notes.map((w) => notice(w)))) : null);
}

// rates turns a history of counters into per-sample differences.
export function rates(values) {
  const out = [];
  for (let i = 1; i < values.length; i++) out.push(Math.max(0, values[i] - values[i - 1]));
  return out;
}
