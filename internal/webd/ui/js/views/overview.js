// Overview: is this node all right, and what is it part of?
import { h } from '../dom.js';
import { state, history, alerts, nameOf, shownWarnings } from '../state.js';
import { panel, stat, dot, pill, spark, notice, bars } from '../ui.js';
import { mbps, since, duration, bandName, roleKind, roleLink, meshLinks, linkName, linkWidth, linkAt, plural, signal, carried, clockShort, gnssShort } from '../fmt.js';
import { topology } from '../topo.js';
import { rolloutSummary } from './software.js';
import { clock, day, zoneLabel, nodeNow, isZulu } from '../time.js';

// headline is one sentence on the node's state, most serious first.
export function headline(st) {
  const shown = shownWarnings(st);
  const errs = shown.filter((w) => w.level === 'error').length;
  const warns = shown.filter((w) => w.level === 'warning').length;
  const peers = (st.batman.neighbors || []).length;
  if (errs) return { tone: 'bad', lead: plural(errs, 'fault'), rest: '· see Alerts' };
  if (!st.batman.present) return { tone: 'bad', lead: 'Mesh down', rest: '· batman-adv not running' };
  if (!st.reconcile.converged) return { tone: 'warn', lead: 'Applying configuration', rest: '' };
  if (!peers && (st.nodes || []).length === 0) return { tone: 'warn', lead: 'No peers', rest: '· no mesh nodes in range' };
  if (warns) return { tone: 'warn', lead: 'Operational', rest: `· ${plural(warns, 'warning')}` };
  return { tone: 'ok', lead: 'Operational', rest: '' };
}

export function internetSummary(st) {
  const g = st.gateway;
  if (g.web_check === 'portal') return { tone: 'warn', value: 'Captive portal', note: `Uplink ${g.wan} blocked until sign-in${g.selected && !g.selected.self ? ` · using ${g.selected.callsign}` : ''}` };
  if (g.selected && g.selected.self) return { tone: 'ok', value: 'Local uplink', note: `${g.wan} · gateway for the mesh` };
  if (g.selected) return { tone: 'ok', value: `Via ${g.selected.callsign}`, note: `Path ${mbps(g.selected.throughput_mbps)} Mbit/s` };
  if (g.wan_known && !g.wan_healthy) return { tone: 'bad', value: 'Down', note: `Uplink ${g.wan} down · no other gateway` };
  return { tone: 'warn', value: 'None', note: 'No gateway in the mesh' };
}

// timeFact is the node's clock, in the zone of whoever is looking (the
// device's, the one chosen in Settings → Appearance, else Zulu), with
// Zulu beside it and where the node gets its time from. A clock that
// follows nothing may be wrong, and says so.
function timeFact(st) {
  const now = nodeNow();
  const kept = st.clock && st.clock.service === 'chrony' && st.clock.synced;
  if (!now) return h('div', h('span', 'Time'), h('a', { href: '#/settings/system' }, clockShort(st.clock)));
  return h('div.timefact', h('span', 'Time'),
    h('div',
      h('a.clock', { href: '#/settings/appearance', class: kept ? '' : 'warn',
        title: (kept ? 'Node clock' : 'Node clock, not synced: may be wrong') + ` · ${isZulu() ? 'Zulu' : zoneLabel(now)} (Settings → Appearance)` },
        clock(now, { zone: true })),
      h('div.sub', day(now), isZulu() ? '' : ` · ${clock(now, { in: 'UTC', seconds: false })}`),
      h('div.sub', h('a', { href: '#/settings/system' }, clockShort(st.clock)))));
}

// siteFact is the site link in a line: this node's tunnels when it is a
// member, and how many members of the mesh are linked.
export function siteSummary(st) {
  const sl = st.site_link;
  const roster = (sl && sl.roster) || [];
  if (!roster.length) return null;
  const sites = new Set(roster.map((m) => m.site));
  if (sites.size < 2) return { tone: 'warn', text: 'One site only', note: 'All members at one site: nothing to link' };
  const peers = sl.peers || [];
  const up = peers.filter((p) => p.up).length;
  // A member is linked when it reports a tunnel up; one this node does
  // not hear is at a site that is not joined.
  const linked = roster.filter((m) => m.reported && m.up > 0).length;
  const unheard = roster.filter((m) => !m.heard).length;
  if (sl.member && peers.length) {
    return { tone: up === peers.length ? 'ok' : up ? 'warn' : 'bad', text: `${up}/${peers.length} tunnels up`,
      note: `Site ${sl.site} · members linked ${linked}/${roster.length}${unheard ? ` · ${unheard} unreachable` : ''}` };
  }
  return { tone: linked === roster.length ? 'ok' : linked ? 'warn' : 'bad', text: linked ? `${linked}/${roster.length} members linked` : 'Down',
    note: unheard ? `${unheard}/${roster.length} members unreachable` : `Sites: ${[...sites].join(', ')}` };
}

function siteFact(st) {
  const s = siteSummary(st);
  if (!s) return null;
  return h('div', h('span', 'Site link'), h('a', { href: '#/internet', class: s.tone === 'ok' ? '' : s.tone, title: s.note }, s.text));
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
    message: `Mesh profile rev ${mch.revision} from ${mch.sender ? 'this node' : mch.from}${mch.summary ? ` (${mch.summary})` : ''}: switchover at ${clock(mch.apply_at)}.` }] : [];
  // So does a mesh-wide upgrade under way.
  const ro = st.rollout && !st.rollout.ended ? st.rollout : null;
  if (ro) { const sum = rolloutSummary(ro); change.push({ level: sum.tone, source: 'mesh', code: 'upgrade', message: sum.text }); }
  // Faults before warnings: only four fit, and the headline counts faults.
  const serious = alerts(st).sort((a, b) => (a.level === 'error' ? 0 : 1) - (b.level === 'error' ? 0 : 1));
  const notes = [...change, ...serious, ...shownWarnings(st).filter((w) => w.level === 'info')].slice(0, 4);
  const links = meshLinks(st);

  return h('div.page',
    h('section.hero',
      h('div',
        h('div.label', `Node ${st.node.node_id} · mesh ${st.profile.name}`),
        h('h1', st.node.callsign),
        h('div.state', h('span', { class: hl.tone }, dot(hl.tone)), h('span', h('b', hl.lead), hl.rest ? ' ' + hl.rest : ''))),
      h('div.hero-facts',
        h('div', h('span', 'Address'), st.node.address.split('/')[0]),
        // The board's uptime, else Trellis's.
        h('div', h('span', 'Uptime'), st.host && st.host.uptime_s ? duration(st.host.uptime_s) : since(st.started_at)),
        // Every link of the profile, by its band and channel; one this node
        // has no radio for is the mesh's, not this node's, and says so.
        h('div', h('span', links.length > 1 ? 'Mesh links' : 'Mesh link'),
          links.map((l) => h('div', `${linkName(l, st)} · ${linkWidth(l, st)} MHz`,
            l.running_width_mhz && l.running_width_mhz !== linkWidth(l, st) ? h('span.warn', ` (running ${l.running_width_mhz} MHz)`) : null,
            links.length > 1 && !carried(l) ? h('span.faint', ' · no radio') : null))),
        timeFact(st),
        siteFact(st),
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
            bestNbr ? ['Best link ', h('b.mono', mbps(bestNbr.throughput_mbps)), ' Mbit/s · ', nameOf(bestNbr.mac)] : 'No neighbours'))),
        panel('Internet', { meta: h('a', { href: '#/internet' }, 'Gateways →') },
          stat('Path', net.value, '', { text: true, tone: net.tone, note: net.note }),
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
            h('div', h('div.mono', c.hostname || c.mac), h('div.faint.mono', { style: { fontSize: '12px' } }, c.ip || 'No address')),
            bars(c.signal_dbm))),
          wired.slice(0, 2).map((l) => h('div.person',
            h('div', h('div.mono', l.hostname || l.mac), h('div.faint.mono', { style: { fontSize: '12px' } }, l.ip)),
            h('span.faint.mono', { style: { fontSize: '12px' } }, 'wired'))),
          !clients.length && !wired.length ? h('div.faint', { style: { padding: '8px 0' } },
            st.ap.enabled ? `No clients · AP “${st.ap.ssid}” up` : 'No clients · AP off') : null)),

      panel('Radios', { class: 'c4', meta: h('a', { href: '#/radios' }, 'Roles →') },
        st.radios.length ? st.radios.map((r) => {
          // The badge says what the radio does; the line under its name,
          // on which link (band and channel), as everywhere else.
          const link = linkAt(st, roleLink(r.role));
          const on = link ? linkName(link, st) : r.role === 'ap' && st.ap.interface ? `${bandName(st.ap.band)} · ch ${st.ap.channel}` : '';
          return h('div.radio-line',
            pill(roleKind(r.role), r.role === 'unused' ? 'plain' : r.role.includes('mesh') ? 'solid' : 'accent'),
            h('div', h('div', { style: { fontSize: '14px' } }, r.label || r.chipset || r.driver), h('div.faint.mono', { style: { fontSize: '12px' } }, [on, r.phy, r.thermal ? `${Math.round(r.thermal.temp_c)} °C` : null].filter(Boolean).join(' · '),
              r.thermal && r.thermal.throttling ? h('span.warn', ' · Throttling') : null)),
            // The best signal on this radio's own link: each link has its own
            // peers. A peer with no signal reading (0) does not count.
            link && (link.peers || []).length ? bars(bestSignal(link.peers)) : null);
        }) : h('div.faint', st.mesh_link.transport === 'wired' ? 'No radios: the mesh runs over wired ports.' : 'No radios found.')),

      panel('Multicast', { class: 'c4', meta: h('a', { href: '#/multicast' }, 'Counters →') },
        stat('ATAK and other group traffic', mc.path ? mc.path[0].toUpperCase() + mc.path.slice(1) : 'Unknown', '', { text: true, tone: mcTone,
          note: mc.path === 'efficient' ? 'Pruned to subscribed nodes' : mc.path === 'partial' ? 'Partly pruned · some nodes receive all groups' : 'Flooded to all nodes' }),
        h('div', { style: { marginTop: '12px' } }, spark(rates(history('mcast')))))),

    notes.length ? panel('Notices', { meta: h('a', { href: '#/alerts' }, 'All →') }, h('div.notices', notes.map((w) => notice(w)))) : null);
}

// bestSignal is the strongest signal among peers that report one, or 0
// (unknown) when none does.
export function bestSignal(peers) {
  const known = peers.map((p) => p.signal_dbm).filter((d) => d < 0);
  return known.length ? Math.max(...known) : 0;
}

// rates turns a history of counters into per-sample differences.
export function rates(values) {
  const out = [];
  for (let i = 1; i < values.length; i++) out.push(Math.max(0, values[i] - values[i - 1]));
  return out;
}
