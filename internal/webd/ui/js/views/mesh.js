// Mesh: the topology, this node's direct links and its routes.
import { h } from '../dom.js';
import { state, history, nameOf, nodeByAddr, shortMac } from '../state.js';
import { panel, table, bars, spark, pill, button } from '../ui.js';
import { mbps, ago, duration, plural, signal, meshLinks, carried, linkName, linkOn } from '../fmt.js';
import { topology, legend, resetLayout, hasPins, acrossSites } from '../topo.js';
import { stamp } from '../time.js';

export function mesh() {
  const st = state.status;
  const topo = state.topology;
  const links = meshLinks(st);
  // Peers by link and address: a peer that moves its radio between links
  // shows up on both for a moment.
  const peers = new Map(links.flatMap((l) => (l.peers || []).map((p) => [`${l.interface}|${p.mac}`, p])));
  // Fastest first; through a site link's tunnel last (its figure is not
  // measured, see crosses).
  const tunnel = (via) => (via || '').startsWith('site-');
  const nbrs = [...(st.batman.neighbors || [])].sort((a, b) => tunnel(a.via) - tunnel(b.via) || b.throughput_mbps - a.throughput_mbps);
  const best = (st.batman.originators || []).filter((o) => o.best);
  const others = st.nodes || [];
  const silent = topo ? topo.nodes.filter((n) => !n.reported && n.callsign).length : 0;

  const label = (mac) => {
    const n = nodeByAddr(mac);
    // A node that holds the site link says so, with its site.
    return h('div', h('div.primary', n ? n.callsign : shortMac(mac)),
      h('div.secondary', n ? `#${n.node_id} · ${n.address}` : mac, n && n.site_link ? ` · site link, site ${n.site_link.site}` : ''));
  };
  // The links this node has a radio for: it may be the second without the
  // first. Each is named by its band and channel, as on every page.
  const radioLinks = links.filter(carried);
  const failures = links.flatMap((l) => (l.sae_failures || []).map((e) => ({ ...e, link: linkName(l, st) })))
    .sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
  // The link a neighbour is heard on: a node on two links with this one
  // is listed once per link. Wired neighbours are on a port, not a link.
  // One heard through a tunnel of the site link is at another site.
  const onLink = (n) => { const l = linkOn(st, n.via); return l ? linkName(l, st) : tunnel(n.via) ? 'site link' : 'wired'; };
  // A route is direct when its next hop is the destination node itself.
  // With several links batman-adv names a node by its first link's address
  // (the originator) but may reach it over another, so compare nodes, not
  // addresses.
  const isDirect = (o) => {
    if (o.next_hop === o.mac) return true;
    const a = nodeByAddr(o.mac), b = nodeByAddr(o.next_hop);
    return !!a && a === b;
  };
  // A tunnel of the site link reports no speed: batman-adv is given the
  // profile's figure, so a neighbour through one, and a route that crosses
  // one (through this node's tunnel, or a node the map shows only beyond
  // a site link), has no throughput worth printing.
  const far = acrossSites(topo);
  const crosses = (o) => {
    if (tunnel(o.via)) return true;
    const n = far && nodeByAddr(o.mac);
    return !!n && far.has(n.node_id);
  };
  const routes = best.sort((a, b) => crosses(a) - crosses(b) || b.throughput_mbps - a.throughput_mbps);
  const notMeasured = (text, what) => h('span.faint', { title: `${what} a site link, over the internet: its speed is not measured. batman-adv ranks it by the profile's figure (site_link.mbit).` }, text);

  return h('div.page',
    h('header.page-head',
      h('div', h('h1', 'Mesh'),
        h('p.sub', `${plural(others.length + 1, 'node')} on “${st.profile.name}”, ${plural(nbrs.length, 'direct link')} from here. `,
          'Better links draw their nodes closer together.')),
      h('div.tools', hasPins() ? button('Reset layout', resetLayout, 'small') : null)),

    panel('Topology', { flush: true, meta: topo ? `asked every ${duration((topo.interval_ms || 5000) / 1000)}` + (silent ? ` · ${plural(silent, 'node')} not reporting links` : '') : 'asking…' },
      topology(topo, {}), legend(topo)),

    h('div.grid',
      panel('Direct links', { class: 'c12', flush: true, meta: radioLinks.length ? radioLinks.map((l) => linkName(l, st)).join('  +  ') : st.mesh_link.transport },
        table({ stack: true, key: (n) => n.mac + n.via }, [
          { title: 'Node', cls: 'wide', wide: true, cell: (n) => label(n.mac) },
          { title: 'Link', cell: (n) => h('span.mono.nowrap', onLink(n)) },
          { title: 'Signal', cell: (n) => { const p = peers.get(`${n.via}|${n.mac}`); return p ? h('span.nowrap', bars(p.signal_dbm), ' ', h('span.mono.dim', `${p.signal_dbm} dBm`)) : h('span.faint', (n.via || '').startsWith('site-') ? 'over the internet' : '–'); } },
          { title: 'Throughput', cls: 'num', cell: (n) => (tunnel(n.via) ? notMeasured('not measured', 'A neighbour through') : h('span', h('b', mbps(n.throughput_mbps)), h('span.faint', ' Mbit/s'))) },
          { title: 'Last 3 min', cell: (n) => (tunnel(n.via) ? h('span.faint', '–') : h('div', { style: { width: '110px' } }, spark(history('nbr:' + n.mac)))) },
          { title: 'Heard', cls: 'num', cell: (n) => ago(n.last_seen_ms) },
        ], nbrs, radioLinks.some((l) => l.supplicant.joined) || st.mesh_link.transport === 'wired' ? 'No neighbours in range.' : 'The mesh link is not up yet.')),

      panel('Routes', { class: 'c12', flush: true, meta: 'batman-adv, best path to each node' },
        table({ stack: true, key: (o) => o.mac }, [
          { title: 'To', cls: 'wide', wide: true, cell: (o) => label(o.mac) },
          { title: 'Next hop', cell: (o) => (isDirect(o) ? pill('direct', 'plain') : h('span.mono', 'via ', nameOf(o.next_hop))) },
          { title: 'Path', cls: 'num', cell: (o) => (crosses(o) ? notMeasured('over the internet', 'A path across') : h('span', h('b', mbps(o.throughput_mbps)), h('span.faint', ' Mbit/s'))) },
        ], routes, 'No routes yet.'))),

    // Each link records its own; newest first, across links.
    failures.length ? panel('Failed authentications', { flush: true, meta: 'peers on the channel that did not accept this node: most often another mesh secret' },
      table({ stack: true }, [
        { title: 'When', cell: (e) => h('span.mono', stamp(e.time)) },
        { title: 'Link', cell: (e) => h('span.mono.nowrap', e.link) },
        { title: 'Peer', cell: (e) => h('span.mono', nameOf(e.mac) !== shortMac(e.mac) ? nameOf(e.mac) : e.mac) },
        { title: 'What', cls: 'wide', wide: true, cell: (e) => e.message || e.kind },
      ], failures.slice(0, 8))) : null);
}
