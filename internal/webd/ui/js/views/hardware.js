// Settings → System: the computer the node runs on (status.host, the
// radios' temperatures and PCIe links), and the control loop in brief.
import { h } from '../dom.js';
import { history } from '../state.js';
import { panel, stat, spark, kv, table } from '../ui.js';
import { bytes, duration, since, count, radioBands } from '../fmt.js';
import { pcieNote, pcieLink } from './radios.js';

// The same lines meshd's alerts use (internal/hoststat).
const WARM_MARGIN = 10, CRIT_MARGIN = 5, FULL_WARN = 90, FULL_BAD = 97, MEM_WARN = 10, MEM_BAD = 5;

export const memUsedPct = (x) => (x.mem_total ? 100 * (1 - x.mem_available / x.mem_total) : 0);
export const fsUsedPct = (f) => (f.used + f.avail ? 100 * f.used / (f.used + f.avail) : 0);

function tempTone(t) {
  if (t.crit_c && t.temp_c >= t.crit_c - CRIT_MARGIN) return 'bad';
  if (t.temp_c >= t.hot_c) return 'warn';
  return '';
}
const fsTone = (pct) => (pct >= FULL_BAD ? 'bad' : pct >= FULL_WARN ? 'warn' : 'ok');

// recordHost keeps the short histories the panel draws: one sample per
// status, three minutes.
export function recordHost(st, record) {
  const x = st.host;
  if (!x) return;
  if ((x.temps || []).length) record('host:temp', x.temps[0].temp_c);
  record('host:cpu', Math.max(0, x.cpu_pct));
  if (x.mem_total) record('host:mem', memUsedPct(x));
}

// cell is a figure with its line under it.
const cell = (s, line) => h('div.hwcell', s, line || null);

function power(x) {
  const recent = x.undervoltage_at && Date.now() - Date.parse(x.undervoltage_at) < 10 * 60e3;
  if (x.undervoltage) return stat('Supply', 'Undervoltage', '', { text: true, tone: 'warn', note: 'CPU throttled' });
  if (x.undervoltage == null) return stat('Supply', 'N/A', '', { text: true, tone: 'faint', note: 'Not reported by this board' });
  return stat('Supply', 'OK', '', { text: true, tone: recent ? 'warn' : 'ok',
    note: x.undervoltage_at ? `Last undervoltage ${since(x.undervoltage_at)} ago` : 'No undervoltage events' });
}

function storage(fss) {
  if (!fss.length) return h('span.faint', 'N/A');
  return h('div.fslist', fss.map((f) => {
    const pct = fsUsedPct(f);
    return h('div.fsrow', { key: f.mount, title: `${f.device} · ${f.type}` },
      h('span.mono', f.mount),
      h('div.meter', { class: fsTone(pct), style: { '--w': pct.toFixed(1) + '%' } }, h('i')),
      h('span', h('span', { class: fsTone(pct) === 'ok' ? '' : fsTone(pct) }, `${pct.toFixed(0)}%`), h('span.faint', ` · ${bytes(f.avail)} free / ${bytes(f.size)}`)));
  }));
}

// radioHeat lists the radios that report a temperature.
function radioHeat(st) {
  const rs = (st.radios || []).filter((r) => r.thermal);
  if (!rs.length) return null;
  return h('div', rs.map((r) => h('div', { key: r.fingerprint },
    `${r.label || r.chipset || r.phy} ${radioBands(r.bands)}: `,
    h('span', { class: r.thermal.throttling ? 'warn' : '' }, `${Math.round(r.thermal.temp_c)} °C`),
    r.thermal.throttling ? h('span.warn', ` · Throttling, Tx duty ${r.thermal.duty_pct}%`)
      : r.thermal.throttle_at_c ? h('span.faint', ` · Throttle at ${Math.round(r.thermal.throttle_at_c)} °C`) : null)));
}

// pcieLinks lists the PCIe links radios sit on, one per card.
function pcieLinks(st) {
  const seen = new Map();
  for (const r of st.radios || []) if (r.pcie && !seen.has(r.pcie.device)) seen.set(r.pcie.device, r);
  if (!seen.size) return null;
  return h('div', [...seen.values()].map((r) => h('div', { key: r.pcie.device },
    `${r.chipset || r.label || r.phy}: `,
    h('span', { class: r.pcie.health === 'marginal' ? 'warn' : '' }, pcieLink(r.pcie)),
    h('span.faint', ` · ${pcieNote(r.pcie)}`))));
}

export function hardwarePanel(st) {
  const x = st.host;
  if (!x) return panel('Hardware', {}, h('p.dim', 'N/A: requires a newer Trellis version on this node.'));
  const temps = x.temps || [];
  const hot = temps[0];
  const memPct = memUsedPct(x);
  const memTone = !x.mem_total ? '' : x.mem_available / x.mem_total * 100 < MEM_BAD ? 'bad' : x.mem_available / x.mem_total * 100 < MEM_WARN ? 'warn' : '';
  const rows = [
    ['Storage', storage(x.filesystems || [])],
    x.cpu_max_mhz ? ['CPU clock', `${count(x.cpu_mhz)} / ${count(x.cpu_max_mhz)} MHz`] : null,
    x.swap_total ? ['Swap', `${bytes(x.swap_total - x.swap_free)} / ${bytes(x.swap_total)}`] : null,
    temps.length > 1 ? ['Sensors', h('div', temps.map((t) => h('div', { key: t.name }, `${t.name}: `, h('span', { class: tempTone(t) }, `${Math.round(t.temp_c)} °C`))))] : null,
  ];
  const heat = radioHeat(st), links = pcieLinks(st);
  if (heat) rows.push(['Radios', heat]);
  if (links) rows.push(['PCIe', links]);
  const meta = [x.model, x.uptime_s ? `uptime ${duration(x.uptime_s)}` : null].filter(Boolean).join(' · ');
  return panel('Hardware', { meta },
    h('div.hwstats',
      cell(hot
        ? h('div', { title: `Sensor: ${hot.name}` }, stat('CPU temp', String(Math.round(hot.temp_c)), '°C', { tone: tempTone(hot),
          note: hot.temp_c >= hot.hot_c ? 'Throttling' : `Throttle at ${Math.round(hot.hot_c)} °C` }))
        : stat('CPU temp', 'N/A', '', { text: true, tone: 'faint' }),
      hot ? spark(history('host:temp'), { max: hot.hot_c }) : null),
      cell(stat('CPU', x.cpu_pct >= 0 ? x.cpu_pct.toFixed(0) : '–', '%', { note: `Load ${x.load[0].toFixed(2)} · ${x.cpus} cores` }),
        spark(history('host:cpu'), { max: 100 })),
      cell(stat('Memory', x.mem_total ? memPct.toFixed(0) : '–', '%', { tone: memTone, note: x.mem_total ? `${bytes(x.mem_available)} free / ${bytes(x.mem_total)}` : '' }),
        spark(history('host:mem'), { max: 100 })),
      cell(power(x))),
    h('p.hint.hwhint', 'History: 3 min'),
    kv(rows));
}

// controlLoopPanel is the reconcile loop in a line: state, passes, the
// last pass; failing parts listed; every part on request.
export function controlLoopPanel(st) {
  const rc = st.reconcile;
  const parts = rc.reconcilers || [];
  const failing = parts.filter((r) => r.last_error);
  return panel('Control loop', {},
    h('div.loopline',
      h('span', { class: rc.converged ? 'ok' : 'warn' }, rc.converged ? 'Converged' : 'Applying changes'),
      h('span.faint', ` · ${count(rc.passes)} passes · last ${since(rc.last_pass)} ago (${rc.last_duration}) · `),
      failing.length ? h('span.bad', `${failing.length}/${parts.length} parts failing`) : h('span.faint', `${parts.length}/${parts.length} parts OK`)),
    failing.length ? h('div.looperr', failing.map((r) => h('div', { key: r.name }, h('span.primary.mono', r.name), ': ', h('span.bad', r.last_error)))) : null,
    h('details.loopall', h('summary', 'Details'),
      table({ stack: true, key: (r) => r.name }, [
        { title: 'Part', cell: (r) => h('span.primary', r.name) },
        { title: 'Changes', cls: 'num', cell: (r) => count(r.changes) },
        { title: 'Last change', cls: 'num', cell: (r) => (Date.parse(r.last_changed) > 86400000 ? since(r.last_changed) + ' ago' : h('span.faint', 'never')) },
        { title: 'State', cls: 'wide', wide: true, cell: (r) => (r.last_error ? h('span.bad', r.last_error) : h('span.ok', 'OK')) },
      ], parts)));
}
