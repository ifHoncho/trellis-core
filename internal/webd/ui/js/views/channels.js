// Channels (Settings → Channels): how busy each mesh link's channel is, now
// at every node and across the band from the last survey.
// Busy is the share of time the channel was taken by anything at that node:
// other networks, interference, the mesh itself.
import { h } from '../dom.js';
import { get, post } from '../api.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { panel, button, notice } from '../ui.js';
import { meshLinks, linkName, linkWidth, carried, plural } from '../fmt.js';
import { loadConfig, saveMeshProfile, editProfile } from '../config.js';
import { stamp } from '../time.js';

const ui = { survey: undefined, timer: null, busy: false };

const tone = (pct) => (pct == null || pct < 0 ? 'unknown' : pct >= 40 ? 'high' : pct >= 20 ? 'mid' : 'low');

// The 20 MHz channels a link of this width covers from primary ch (as
// meshd's channelBlock).
function block(band, ch, width) {
  if (band !== '5g' || width <= 20) return [ch];
  const base = ch >= 149 ? 149 : ch >= 100 ? 100 : 36;
  const n = width / 20;
  const start = base + Math.floor((ch - base) / 4 / n) * n * 4;
  return Array.from({ length: n }, (_, i) => start + 4 * i);
}
const isRadar = (band, ch) => band === '5g' && ch >= 52 && ch <= 144;

async function fetchSurvey() {
  try { ui.survey = await get('/survey'); } catch (e) { toast(e.message, 'bad'); }
  clearTimeout(ui.timer);
  if (ui.survey && !ui.survey.done) ui.timer = setTimeout(fetchSurvey, 2000);
  redraw();
}

function startSurvey() {
  openDialog({
    title: 'Survey the channels?',
    body: () => [
      h('p', 'Every node listens across the band of each of its mesh links for a few seconds, without transmitting. Each link pauses briefly meanwhile, and so does a client Wi-Fi that shares a mesh radio.'),
    ],
    actions: [
      { label: 'Cancel', run: () => closeDialog() },
      { label: 'Survey', kind: 'primary', run: async () => {
        closeDialog();
        ui.busy = true; redraw();
        try { ui.survey = await post('/survey', {}); } catch (e) { toast(e.message, 'bad', 7000); }
        ui.busy = false;
        fetchSurvey();
      } },
    ],
  });
}

// moveLink sends the profile with link n (from 1) on channel ch to every node.
async function moveLink(n, ch) {
  if (!state.config) await loadConfig();
  if (!state.config) { toast('The configuration is not loaded.', 'bad'); return; }
  const p = JSON.parse(JSON.stringify(editProfile()));
  if (n === 1) p.radio.channel = ch; else p.links[n - 2].channel = ch;
  try { await saveMeshProfile({ profile: p }, 60); } catch (e) { toast(e.message.split('\n')[0], 'bad', 7000); }
}

// A horizontal bar: label, bar, value.
function hbar(label, pct, sub) {
  const t = tone(pct);
  return h('div.hbar',
    h('span.hbar-label.mono', label),
    h('span.hbar-track', h('span', { class: 'hbar-fill ' + t, style: { width: `${Math.max(pct || 0, 1)}%` } })),
    h('span', { class: 'hbar-value ' + t }, pct >= 0 ? `${pct}%` : '–'),
    sub ? h('span.hbar-sub.faint', sub) : null);
}

// nowView: the link's own channel at each node, busiest first.
function nowView(l, i, st) {
  const rows = [];
  if (carried(l)) rows.push({ name: `${st.node.callsign} (this node)`, pct: l.channel_busy_pct });
  const silent = [];
  for (const n of st.nodes || []) {
    const v = (n.channel_busy || [])[i];
    if (v >= 0) rows.push({ name: n.callsign, pct: v }); else silent.push(n.callsign);
  }
  const measured = rows.filter((r) => r.pct >= 0).sort((a, b) => b.pct - a.pct);
  if (carried(l) && !(l.channel_busy_pct >= 0)) silent.unshift(st.node.callsign);
  return h('div.chan-now',
    h('div.label', `Now, on channel ${l.channel || st.profile.channel}`),
    measured.length ? h('div.hbars', measured.map((r) => hbar(r.name, r.pct))) : h('p.faint', 'No node measures this link’s load (no radio on it, or a driver that does not say).'),
    measured.length && silent.length ? h('p.faint.small', `Not measured on ${silent.join(', ')}: no radio on this link, or a driver that does not say.`) : null);
}

// bandView: every surveyed channel of the link's band, its height the
// busiest node there.
function bandView(l, i, sv, sug) {
  const band = l.band, link = i + 1;
  const width = l.width_mhz;
  const per = new Map(); // ch → {busy, node, nets}
  for (const n of sv.nodes) {
    for (const sl of n.links || []) {
      if (sl.link !== link || sl.band !== band) continue;
      for (const c of sl.channels) {
        const cur = per.get(c.ch) || { busy: -1, node: '', nets: 0 };
        if (c.busy > cur.busy) { cur.busy = c.busy; cur.node = n.callsign; }
        cur.nets = Math.max(cur.nets, c.nets || 0);
        per.set(c.ch, cur);
      }
    }
  }
  const chans = [...per.keys()].filter((c) => !(band === '2g' && c === 14)).sort((a, b) => a - b);
  if (!chans.length) return h('p.faint', sug && sug.note ? capital(sug.note) + '.' : 'Not surveyed on this band.');
  const curBlock = block(band, l.channel, width);
  const bestBlock = sug && sug.best ? block(band, sug.best.ch, width) : [];
  return h('div.chan-band',
    h('div.label', `Across the band, busiest node per channel (${width} MHz: a link uses ${plural(curBlock.length, 'channel')})`),
    h('div.chan-wrap',
      h('div.chan-chart', chans.map((c) => {
        const d = per.get(c);
        const cls = ['chan-col', curBlock.includes(c) ? 'current' : '', bestBlock.includes(c) ? 'best' : '', isRadar(band, c) ? 'radar' : ''].filter(Boolean).join(' ');
        return h('div', { class: cls, title: `Channel ${c}: ${d.busy >= 0 ? d.busy + '% busy at ' + d.node : 'busy not measured'}${d.nets ? `, ${plural(d.nets, 'other network')}` : ''}${isRadar(band, c) ? ' · radar channel' : ''}` },
          h('span.chan-pct', d.busy >= 0 ? `${d.busy}%` : '–'),
          h('span', { class: 'chan-bar ' + tone(d.busy), style: { height: `${d.busy >= 0 ? Math.max(d.busy * 0.85, 2) : 2}%` } }));
      })),
      h('div.chan-axis', chans.map((c) => h('span', { class: isRadar(band, c) ? 'radar' : '' }, String(c))))),
    h('div.chan-legend.faint', h('span.k.current-k'), 'this link', bestBlock.length ? [h('span.k.best-k'), 'suggested'] : null,
      band === '5g' ? [h('span.k.radar-k'), 'radar channel'] : null));
}

const capital = (s) => (s ? s[0].toUpperCase() + s.slice(1) : s);

export function channelsSection(st) {
  if (ui.survey === undefined) { ui.survey = null; fetchSurvey(); }
  const sv = ui.survey;
  const links = meshLinks(st);
  const running = ui.busy || (sv && !sv.done);
  const answered = sv ? sv.nodes.filter((n) => n.state === 'answered').length : 0;
  const head = panel('Channel survey', {
    tools: [button(running ? 'Surveying…' : 'Survey channels', startSurvey, 'small primary', { disabled: running })],
  },
    h('p.dim', { style: { fontSize: '14px', maxWidth: '85ch' } },
      'Busy is the share of time a channel is taken at a node: by other networks, interference, or the mesh itself. Each node measures its links’ channels all the time; a survey has every node listen across the whole band for a few seconds, and finds the clearest channel for each link.'),
    sv ? h('p', { style: { fontSize: '14px', marginTop: '8px' } },
      running ? `Surveying: ${answered} of ${sv.nodes.length} nodes have answered…`
        : `Last survey ${stamp(sv.started)}: ${answered} of ${plural(sv.nodes.length, 'node')} answered.`,
      sv.nodes.filter((n) => !['answered', 'asked'].includes(n.state)).map((n) => h('span.faint', ` ${n.callsign}: ${n.state}.`)))
      : h('p.faint', { style: { fontSize: '14px', marginTop: '8px' } }, 'No survey yet.'));
  const cards = links.map((l, i) => {
    const sug = sv ? (sv.suggestions || []).find((s) => s.link === i + 1) : null;
    const lw = { ...l, band: l.band || st.profile.band, channel: l.channel || st.profile.channel, width_mhz: linkWidth(l, st) };
    if (lw.band === 's1g') {
      return panel(`${linkName(l, st)} · ${lw.width_mhz} MHz`, {}, h('p.faint', 'HaLow radios report neither their channel’s load nor a survey.'));
    }
    return panel(`${linkName(l, st)} · ${lw.width_mhz} MHz`, {},
      nowView(lw, i, st),
      sv && lw.band !== 's1g' ? bandView(lw, i, sv, sug) : lw.band === 's1g' ? h('p.faint', 'HaLow radios report no channel survey.') : null,
      sug && sv.done ? h('div.chan-verdict',
        notice({ level: sug.best ? 'warning' : 'ok', message: capital(sug.note) + '.' }),
        sug.best ? h('div.row.end', { style: { marginTop: '10px' } }, button(`Move this link to channel ${sug.best.ch} on every node`, () => moveLink(i + 1, sug.best.ch), 'primary')) : null) : null);
  });
  return [head, ...cards];
}
