// Multicast: how group traffic (ATAK position reports, chat, voice
// plugins) travels, and what the node keeps off the mesh.
import { h } from '../dom.js';
import { state, history } from '../state.js';
import { panel, stat, table, kv, spark, dot } from '../ui.js';
import { count, bytes } from '../fmt.js';
import { rates } from './overview.js';

const PATHS = {
  efficient: ['ok', 'Efficient', 'Group traffic goes only to nodes that have a listener, in batman-adv’s multicast packets. This is what keeps ATAK cheap on the air.'],
  partial: ['warn', 'Partial', 'Some nodes ask for every group (they have no usable querier yet, often just after a restart), so every multicast packet also goes to them.'],
  flooding: ['bad', 'Flooding', 'Every multicast packet goes to every node. On a radio mesh that costs airtime on every link.'],
};
const RULES = {
  'anycast-arp': 'ARP for the shared gateway address', 'anycast-mac': 'Frames from the shared gateway MAC', 'dhcp-broadcast': 'DHCP broadcasts (each node serves its own devices)',
  'ipv6-ra': 'IPv6 router advertisements', dhcpv6: 'DHCPv6', chatter: 'Discovery chatter (SSDP, LLMNR, NetBIOS)', 'link-local-multicast': 'Link-local multicast over the limit (mDNS bursts)',
  'ipv4-broadcast': 'Other IPv4 broadcasts', 'rogue-dhcp-server': 'DHCP servers on a device port', 'rogue-ipv6-ra': 'Router advertisements from a device',
  'rogue-multicast-querier': 'Multicast queriers on a device port', 'gossip-to-from-clients': 'Node control traffic to or from devices',
};

export function multicast() {
  const st = state.status;
  const m = st.batman.multicast;
  const [tone, word, text] = PATHS[m.path] || ['warn', 'Unknown', 'The node has not worked out the multicast path yet.'];
  const c = m.counters;
  const f = st.client_network.filters;
  const drops = Object.entries(f.drops || {}).map(([rule, n]) => ({ rule, n })).sort((a, b) => b.n - a.n);
  const yes = (b) => (b ? h('span.ok', 'yes') : h('span.faint', 'no'));

  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Multicast'),
      h('p.sub', 'ATAK sends positions and chat to multicast groups. The mesh delivers them to listeners only, and keeps household chatter off the air.'))),

    h('div.grid',
      panel('Path in use', { class: 'c5' },
        stat('Group traffic', word, '', { text: true, tone }),
        h('p.dim', { style: { margin: '10px 0 16px', fontSize: '14px' } }, text),
        kv([
          ['Optimisations', yes(m.optimisations)],
          ['Querier seen', h('span', 'IPv4 ', yes(m.querier_ipv4), ' · IPv6 ', yes(m.querier_ipv6))],
          ['This node wants all', h('span', 'IPv4 ', m.want_all_ipv4 ? h('span.warn', 'yes') : h('span.faint', 'no'), ' · IPv6 ', m.want_all_ipv6 ? h('span.warn', 'yes') : h('span.faint', 'no'))],
          ['Nodes reporting', `${m.peers_with_flags} of ${(st.nodes || []).length}`],
          ['Wanting all IPv4', m.peers_want_all_ipv4 ? h('span.warn', String(m.peers_want_all_ipv4)) : '0'],
          ['Without packet type', m.peers_no_mc_ptype ? h('span.warn', String(m.peers_no_mc_ptype)) : '0'],
          ['Old batman-adv', m.legacy_peers ? h('span.bad', `${m.legacy_peers} (forces flooding)`) : '0'],
        ])),
      panel('Multicast packets', { class: 'c7', meta: 'batman-adv’s own packet type, since the mesh interface came up' },
        h('div.stats',
          stat('Sent', count(c.tx), '', { note: bytes(c.tx_bytes) }),
          stat('Received', count(c.rx), '', { note: `${count(c.rx_local)} for devices here` }),
          stat('Forwarded', count(c.fwd), '', { note: bytes(c.fwd_bytes) })),
        h('div.label', { style: { margin: '18px 0 6px' } }, 'Packets per 2 s, last 3 min'),
        spark(rates(history('mcast'))))),

    panel('Kept off the mesh', { flush: true, meta: f.applied ? `${f.rules} filter rules in place` : h('span.bad', f.error || 'filters not applied') },
      table({ stack: true, key: (d) => d.rule }, [
        { title: 'What', cls: 'wide', wide: true, cell: (d) => h('div', h('div', RULES[d.rule] || d.rule), h('div.secondary', d.rule)) },
        { title: 'Frames dropped', cls: 'num', cell: (d) => (d.n ? h('b', count(d.n)) : h('span.faint', '0')) },
      ], drops, 'No counters yet.')));
}
