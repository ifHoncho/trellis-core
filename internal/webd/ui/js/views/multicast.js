// Multicast: how group traffic (ATAK position reports, chat, voice
// plugins) travels, and what the node keeps off the mesh.
import { h } from '../dom.js';
import { state, history } from '../state.js';
import { panel, stat, table, kv, spark, dot } from '../ui.js';
import { count, bytes } from '../fmt.js';
import { rates } from './overview.js';

const PATHS = {
  efficient: ['ok', 'Efficient', 'Group traffic forwarded only to nodes with subscribers (batman-adv multicast). Minimizes ATAK airtime.'],
  partial: ['warn', 'Partial', 'Some nodes request all groups (no usable querier yet, typically after restart) and receive all multicast.'],
  flooding: ['bad', 'Flooding', 'All multicast flooded to all nodes. Costs airtime on every link.'],
};
const RULES = {
  'anycast-arp': 'ARP for the shared gateway address', 'anycast-mac': 'Frames from the shared gateway MAC', 'dhcp-broadcast': 'DHCP broadcasts (served per node)',
  'ipv6-ra': 'IPv6 router advertisements', dhcpv6: 'DHCPv6', chatter: 'Discovery protocols (SSDP, LLMNR, NetBIOS)', 'link-local-multicast': 'Link-local multicast over rate limit (mDNS bursts)',
  'ipv4-broadcast': 'Other IPv4 broadcasts', 'rogue-dhcp-server': 'DHCP servers on client ports', 'rogue-ipv6-ra': 'Router advertisements from clients',
  'rogue-multicast-querier': 'Multicast queriers on client ports', 'gossip-to-from-clients': 'Node control traffic to/from clients',
  'broadcast-warm-up': 'Mesh broadcast warm-up frames (after a node restart)',
};

export function multicast() {
  const st = state.status;
  const m = st.batman.multicast;
  const [tone, word, text] = PATHS[m.path] || ['warn', 'Unknown', 'Multicast path not determined yet.'];
  const c = m.counters;
  const f = st.client_network.filters;
  const drops = Object.entries(f.drops || {}).map(([rule, n]) => ({ rule, n })).sort((a, b) => b.n - a.n);
  const yes = (b) => (b ? h('span.ok', 'yes') : h('span.faint', 'no'));

  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Multicast'),
      h('p.sub', 'ATAK PLI and chat use multicast groups. Delivered to subscribers only; local broadcast traffic filtered.'))),

    h('div.grid',
      panel('Path in use', { class: 'c5' },
        stat('Group traffic', word, '', { text: true, tone }),
        h('p.dim', { style: { margin: '10px 0 16px', fontSize: '14px' } }, text),
        kv([
          ['Optimisations', yes(m.optimisations)],
          ['Querier seen', h('span', 'IPv4 ', yes(m.querier_ipv4), ' · IPv6 ', yes(m.querier_ipv6))],
          ['Requests all groups', h('span', 'IPv4 ', m.want_all_ipv4 ? h('span.warn', 'yes') : h('span.faint', 'no'), ' · IPv6 ', m.want_all_ipv6 ? h('span.warn', 'yes') : h('span.faint', 'no'))],
          ['Nodes reporting', `${m.peers_with_flags} of ${(st.nodes || []).length}`],
          ['Requesting all IPv4', m.peers_want_all_ipv4 ? h('span.warn', String(m.peers_want_all_ipv4)) : '0'],
          ['Without packet type', m.peers_no_mc_ptype ? h('span.warn', String(m.peers_no_mc_ptype)) : '0'],
          ['Legacy batman-adv', m.legacy_peers ? h('span.bad', `${m.legacy_peers} (forces flooding)`) : '0'],
        ])),
      panel('Multicast packets', { class: 'c7', meta: 'batman-adv multicast packets since interface up' },
        h('div.stats',
          stat('Sent', count(c.tx), '', { note: bytes(c.tx_bytes) }),
          stat('Received', count(c.rx), '', { note: `${count(c.rx_local)} to local clients` }),
          stat('Forwarded', count(c.fwd), '', { note: bytes(c.fwd_bytes) })),
        h('div.label', { style: { margin: '18px 0 6px' } }, 'Packets per 2 s, last 3 min'),
        spark(rates(history('mcast'))))),

    panel('Kept off the mesh', { flush: true, meta: f.applied ? `${f.rules} filter rules in place` : h('span.bad', f.error || 'filters not applied') },
      table({ stack: true, key: (d) => d.rule }, [
        { title: 'What', cls: 'wide', wide: true, cell: (d) => h('div', h('div', RULES[d.rule] || d.rule), h('div.secondary', d.rule)) },
        { title: 'Frames dropped', cls: 'num', cell: (d) => (d.n ? h('b', count(d.n)) : h('span.faint', '0')) },
      ], drops, 'No counters.')));
}
