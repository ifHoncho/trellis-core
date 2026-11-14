// Mesh: the topology, this node's direct links and its routes.
import { h } from '../dom.js';
import { state, history, nameOf, nodeByAddr, shortMac } from '../state.js';
import { panel, table, bars, spark, pill, button } from '../ui.js';
import { mbps, ago, duration, plural, signal, meshLinks, carried } from '../fmt.js';
import { topology, legend, resetLayout, hasPins } from '../topo.js';

export function mesh() {
  const st = state.status;
  const topo = state.topology;
  const links = meshLinks(st);
  // Peers by link and address: a peer that moves its radio between links
  // shows up on both for a moment.
  const peers = new Map(links.flatMap((l) => (l.peers || []).map((p) => [`${l.interface}|${p.mac}`, p])));
  const nbrs = [...(st.batman.neighbors || [])].sort((a, b) => b.throughput_mbps - a.throughput_mbps);
  const routes = (st.batman.originators || []).filter((o) => o.best).sort((a, b) => b.throughput_mbps - a.throughput_mbps);
  const others = st.nodes || [];
  const silent = topo ? topo.nodes.filter((n) => !n.reported && n.callsign).length : 0;

  const label = (mac, via) => {
    const n = nodeByAddr(mac);
    return h('div', h('div.primary', n ? n.callsign : shortMac(mac)), h('div.secondary', (n ? `#${n.node_id} · ${n.address}` : mac) + (via ? ` · ${via}` : '')));
  };
  // With more than one mesh link a neighbour can appear once per link: name
  // the link then.
  // The links this node has a radio for: it may be the second without the first.
  const radioLinks = links.filter(carried);
  const manyLinks = radioLinks.length > 1;
  // A route is direct when its next hop is the destination node itself.
  // With several links batman-adv names a node by its first link's address
  // (the originator) but may reach it over another, so compare nodes, not
  // addresses.
  const isDirect = (o) => {
    if (o.next_hop === o.mac) return true;
    const a = nodeByAddr(o.mac), b = nodeByAddr(o.next_hop);
    return !!a && a === b;
  };

  return h('div.page',
    h('header.page-head',
      h('div', h('h1', 'Mesh'),
        h('p.sub', `${plural(others.length + 1, 'node')} on “${st.profile.name}”, ${plural(nbrs.length, 'direct link')} from here. `,
          'Better links draw their nodes closer together.')),
      h('div.tools', hasPins() ? button('Reset layout', resetLayout, 'small') : null)),

    panel('Topology', { flush: true, meta: topo ? `asked every ${duration((topo.interval_ms || 5000) / 1000)}` + (silent ? ` · ${plural(silent, 'node')} not reporting links` : '') : 'asking…' },
      topology(topo, {}), legend()),

    h('div.grid',
      panel('Direct links', { class: 'c7', flush: true, meta: radioLinks.length ? radioLinks.filter((l) => l.freq_mhz).map((l) => `${l.interface} · ${l.freq_mhz} MHz`).join('  +  ') || radioLinks[0].interface : st.mesh_link.transport },
        table({ stack: true, key: (n) => n.mac + n.via }, [
          { title: 'Node', cls: 'wide', wide: true, cell: (n) => label(n.mac, manyLinks ? n.via : '') },
          { title: 'Signal', cell: (n) => { const p = peers.get(`${n.via}|${n.mac}`); return p ? h('span.nowrap', bars(p.signal_dbm), ' ', h('span.mono.dim', `${p.signal_dbm} dBm`)) : h('span.faint', n.via); } },
          { title: 'Throughput', cls: 'num', cell: (n) => h('span', h('b', mbps(n.throughput_mbps)), h('span.faint', ' Mbit/s')) },
          { title: 'Last 3 min', cell: (n) => h('div', { style: { width: '110px' } }, spark(history('nbr:' + n.mac))) },
          { title: 'Heard', cls: 'num', cell: (n) => ago(n.last_seen_ms) },
        ], nbrs, radioLinks.some((l) => l.supplicant.joined) || st.mesh_link.transport === 'wired' ? 'No neighbours in range.' : 'The mesh link is not up yet.')),

      panel('Routes', { class: 'c5', flush: true, meta: 'batman-adv, best path to each node' },
        table({ stack: true, key: (o) => o.mac }, [
          { title: 'To', cls: 'wide', wide: true, cell: (o) => label(o.mac) },
          { title: 'Next hop', cell: (o) => (isDirect(o) ? pill('direct', 'plain') : h('span.mono', 'via ', nameOf(o.next_hop))) },
          { title: 'Path', cls: 'num', cell: (o) => h('span', h('b', mbps(o.throughput_mbps)), h('span.faint', ' Mbit/s')) },
        ], routes, 'No routes yet.'))),

    (st.mesh_link.sae_failures || []).length ? panel('Failed authentications', { flush: true, meta: 'peers that did not answer (they changed channel or restarted), or that have another mesh secret' },
      table({ stack: true }, [
        { title: 'When', cell: (e) => h('span.mono', new Date(e.time).toLocaleTimeString()) },
        { title: 'Radio', cell: (e) => h('span.mono', e.mac) },
        { title: 'What', cls: 'wide', wide: true, cell: (e) => e.message || e.kind },
      ], [...st.mesh_link.sae_failures].reverse().slice(0, 8))) : null);
}
