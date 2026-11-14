// Internet: where this node's devices get it from, and what else could.
import { h } from '../dom.js';
import { state, history } from '../state.js';
import { panel, stat, table, pill, spark, kv, dot } from '../ui.js';
import { mbps, ago, since } from '../fmt.js';
import { internetSummary } from './overview.js';

function path(st) {
  const g = st.gateway;
  const hops = [h('div.hop', h('b', 'Devices'), h('small', `on ${st.node.callsign}`)), h('i.wire'), h('div.hop.me', h('b', st.node.callsign), h('small', 'this node'))];
  if (g.selected && g.selected.self) {
    hops.push(h('i.wire'), h('div.hop', h('b', g.wan || 'uplink'), h('small', 'this node’s uplink')), h('i.wire'), h('div.hop', h('b', 'Internet'), h('small', g.rtt_ms ? `${g.rtt_ms.toFixed(0)} ms away` : 'reachable')));
  } else if (g.selected) {
    hops.push(h('i.wire'), h('div.hop', h('b', g.selected.callsign), h('small', `gateway · ${mbps(g.selected.throughput_mbps)} Mbit/s path`)), h('i.wire'), h('div.hop', h('b', 'Internet'), h('small', 'through its uplink')));
  } else {
    hops.push(h('i.wire.down'), h('div.hop.down', h('b', 'No gateway'), h('small', 'nothing to route to')));
  }
  return h('div.path', hops);
}

export function internet() {
  const st = state.status;
  const g = st.gateway;
  const sum = internetSummary(st);
  const rows = [];
  if (g.wan_known) {
    rows.push({ self: true, callsign: st.node.callsign, node_id: st.node.node_id, address: st.node.address.split('/')[0], usable: g.wan_healthy && g.mode !== 'off',
      why: g.mode === 'off' ? 'sharing is switched off on this node' : g.wan_healthy ? '' : (g.probe_error || 'its uplink is not answering'), selected: g.selected && g.selected.self });
  }
  for (const c of g.candidates || []) rows.push({ ...c, selected: g.selected && !g.selected.self && g.selected.node_id === c.node_id });

  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Internet'),
      h('p.sub', 'Any node with a working uplink shares it. Each node picks the best gateway it can reach and moves to another within seconds if that one fails.'))),

    panel('Path in use', { meta: g.selected ? `for ${since(g.selected.since)}` : '' }, path(st),
      h('p.dim', { style: { marginTop: '14px', fontSize: '14px' } }, sum.note, '.')),

    h('div.grid',
      panel('This node’s uplink', { class: 'c5' },
        g.wan_known
          ? [h('div.stats',
              stat('State', g.wan_healthy ? 'Healthy' : 'Down', '', { text: true, tone: g.wan_healthy ? 'ok' : 'bad' }),
              stat('Round trip', g.wan_healthy && g.rtt_ms ? g.rtt_ms.toFixed(0) : '–', 'ms'),
              stat('Loss', String(g.loss_pct), '%')),
            h('div', { style: { margin: '14px 0 12px' } }, spark(history('rtt'))),
            kv([
              ['Interface', g.wan],
              ['Sharing', g.mode === 'off' ? 'off (node setting)' : g.role === 'gateway' ? `on · announces ${g.announced}` : 'on · waiting for a healthy uplink'],
              ['Probes', (g.probe_targets || []).join(', ')],
              g.probe_error ? ['Last error', g.probe_error] : null,
              ['Since', since(g.wan_since) + ' ago'],
            ])]
          : h('div.faint', g.mode === 'off' ? 'Sharing is switched off on this node, and it has no uplink of its own to watch.' : 'This node has no uplink: no port with a WAN, and the system has no default route.'),
        ),
      panel('Gateways on the mesh', { class: 'c7', flush: true, meta: `${g.switches} switch${g.switches === 1 ? '' : 'es'} since start` },
        table({ stack: true, rowClass: (r) => (r.selected ? 'selected' : ''), key: (r) => r.callsign + r.node_id }, [
          { title: 'Gateway', cls: 'wide', wide: true, cell: (r) => h('div', h('div.primary', r.callsign || r.originator, r.self ? h('span.faint', ' (this node)') : null), h('div.secondary', r.address || '')) },
          { title: 'State', cell: (r) => (r.selected ? pill('in use', 'solid') : r.usable ? pill('ready', 'ok') : pill('not usable', 'bad')) },
          { title: 'Path', cls: 'num', cell: (r) => (r.self ? h('span.faint', 'local') : h('span', h('b', mbps(r.throughput_mbps)), h('span.faint', ' Mbit/s'))) },
          { title: 'Note', cls: 'wide', wide: true, cell: (r) => (r.why ? h('span.dim', r.why) : r.self ? '' : h('span.faint', `heard ${ago(r.last_seen_ms)} ago`)) },
        ], rows, 'No node on the mesh announces an uplink. Devices still reach each other by address and by .mesh name.'))),

    panel('Name resolution', {},
      kv([
        ['Devices ask', h('span', '10.73.0.1'.replace('10.73.0.1', st.client_network.anycast.address || '–'), h('span.sans.dim', ' (this node, at the address every node answers on)'))],
        ['.mesh names', h('span.sans', 'answered here, from what the nodes tell each other')],
        ['Other names', (g.dns || []).length ? h('span', g.dns.join(', '), h('span.sans.dim', g.selected && !g.selected.self ? ' (the gateway in use)' : ' (this node’s own resolvers)')) : h('span.sans.warn', 'nowhere to send them: they fail at once')],
      ])));
}
