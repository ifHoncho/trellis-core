// Internet: where this node's devices get it from, and what else could.
import { h } from '../dom.js';
import { state, history } from '../state.js';
import { panel, stat, table, pill, spark, kv, dot } from '../ui.js';
import { mbps, ago, since } from '../fmt.js';
import { internetSummary } from './overview.js';
import { siteLink } from './sitelink.js';

function path(st) {
  const g = st.gateway;
  const hops = [h('div.hop', h('b', 'Devices'), h('small', `on ${st.node.callsign}`)), h('i.wire'), h('div.hop.me', h('b', st.node.callsign), h('small', 'this node'))];
  if (g.selected && g.selected.self) {
    hops.push(h('i.wire'), h('div.hop', h('b', g.wan || 'uplink'), h('small', 'this node’s uplink')), g.web_check === 'portal'
      ? [h('i.wire.down'), h('div.hop.down', h('b', 'Login page'), h('small', 'sign in to reach the internet'))]
      : [h('i.wire'), h('div.hop', h('b', 'Internet'), h('small', g.rtt_ms ? `${g.rtt_ms.toFixed(0)} ms away` : 'reachable'))]);
  } else if (g.selected) {
    hops.push(h('i.wire'), h('div.hop', h('b', g.selected.callsign), h('small', `gateway · ${mbps(g.selected.throughput_mbps)} Mbit/s path`)), h('i.wire'), h('div.hop', h('b', 'Internet'), h('small', 'through its uplink')));
  } else {
    hops.push(h('i.wire.down'), h('div.hop.down', h('b', 'No gateway'), h('small', 'nothing to route to')));
  }
  return h('div.path', hops);
}

function webCheck(g) {
  switch (g.web_check) {
    case 'ok': return 'passes';
    case 'portal': return h('span.warn', `behind a login page: ${g.web_check_detail}`);
    case 'no-answer': return h('span.dim', `no answer (${g.web_check_detail}); shared on the probes alone`);
    default: return h('span.dim', 'checking');
  }
}

function portalNote(st) {
  const g = st.gateway;
  if (g.web_check !== 'portal') return null;
  const self = g.selected && g.selected.self;
  return panel('Sign in to the uplink', {},
    h('p', `${g.wan} is on a network that shows a login page before it lets traffic out (${g.web_check_detail}). The node does not share it with the mesh until someone signs in.`),
    h('p.dim', self
      ? 'No other gateway is usable, so devices on this node use it: open any http:// page from one of them to reach the login page. Sharing starts on its own within seconds of signing in.'
      : 'Devices on this node use another gateway for now. To sign in, reach the login page from a device on this node while no other gateway is usable, or set gateway.web_check to off to share the uplink as it is.'));
}

export function internet() {
  const st = state.status;
  const g = st.gateway;
  const sum = internetSummary(st);
  const rows = [];
  if (g.wan_known) {
    const portal = g.web_check === 'portal';
    rows.push({ self: true, callsign: st.node.callsign, node_id: st.node.node_id, address: st.node.address.split('/')[0], usable: g.wan_healthy && g.mode !== 'off' && !portal,
      why: g.mode === 'off' ? 'sharing is switched off on this node' : portal ? 'its uplink is behind a login page' : g.wan_healthy ? '' : (g.probe_error || 'its uplink is not answering'), selected: g.selected && g.selected.self });
  }
  for (const c of g.candidates || []) rows.push({ ...c, selected: g.selected && !g.selected.self && g.selected.node_id === c.node_id });

  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Internet'),
      h('p.sub', 'Any node with a working uplink shares it. Each node picks the best gateway it can reach and moves to another within seconds if that one fails.'))),

    portalNote(st),

    panel('Path in use', { meta: g.selected ? `for ${since(g.selected.since)}` : '' }, path(st),
      h('p.dim', { style: { marginTop: '14px', fontSize: '14px' } }, sum.note, '.')),

    h('div.grid',
      panel('This node’s uplink', { class: 'c5' },
        g.wan_known
          ? [h('div.stats',
              g.web_check === 'portal'
                ? stat('State', 'Login page', '', { text: true, tone: 'warn' })
                : stat('State', g.wan_healthy ? 'Healthy' : 'Down', '', { text: true, tone: g.wan_healthy ? 'ok' : 'bad' }),
              stat('Round trip', g.wan_healthy && g.rtt_ms ? g.rtt_ms.toFixed(0) : '–', 'ms'),
              stat('Loss', String(g.loss_pct), '%')),
            h('div', { style: { margin: '14px 0 12px' } }, spark(history('rtt'))),
            kv([
              ['Interface', g.wan],
              ['Sharing', g.mode === 'off' ? 'off (node setting)' : g.role === 'gateway' ? `on · announces ${g.announced}` : g.web_check === 'portal' ? 'not with the mesh until someone signs in' : 'on · waiting for a healthy uplink'],
              ['Probes', (g.probe_targets || []).join(', ')],
              g.web_check ? ['Web check', webCheck(g)] : null,
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
      ])),

    siteLink(st));
}
