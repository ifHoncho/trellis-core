// Alerts: everything the node wants its operator to know.
import { h } from '../dom.js';
import { state, isIgnored, ignoreAlert } from '../state.js';
import { panel, notice, table, asSentence } from '../ui.js';
import { plural, duration } from '../fmt.js';
import { stamp } from '../time.js';

// What a reset's verdict is called.
const VERDICTS = {
  'pcie-link': 'PCIe link', 'card-stopped': 'Card hang', sysrq: 'SysRq reset',
  panic: 'Kernel panic', 'power-loss': 'Power loss', unrecorded: 'Unknown',
};

export function alertsView() {
  const st = state.status;
  const rank = { error: 0, warning: 1, info: 2 };
  const all = [...st.warnings].sort((a, b) => rank[a.level] - rank[b.level]);
  const shown = all.filter((w) => !isIgnored(w));
  const ignored = all.filter((w) => isIgnored(w));
  const serious = shown.filter((w) => w.level !== 'info');
  const row = (w, on) => h('div.alert', { class: on ? 'ignored' : '' },
    notice(w),
    h('button.btn.small.ghost', {
      type: 'button', onclick: () => ignoreAlert(w, !on),
      title: on ? 'Restore' : 'Hide from Overview and alert count (this browser)',
    }, on ? 'Show again' : 'Ignore'));
  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Alerts'),
      h('p.sub', serious.length ? `${serious.length} active` : 'No active faults or warnings',
        ignored.length ? ` · ${ignored.length} ignored` : '',
        ' · ■ fault  ▲ warning  ○ note'))),
    shown.length
      ? h('div.notices', shown.map((w) => row(w, false)))
      : panel('', { class: 'quiet' }, h('div.empty', ignored.length ? 'All other alerts ignored' : 'No alerts')),
    ignored.length ? h('section.ignored-alerts',
      h('div.label', 'Ignored'),
      h('p.dim', 'Hidden from Overview and alert count in this browser. Shown again on higher severity, or on return after 1 h clear.'),
      h('div.notices', ignored.map((w) => row(w, true)))) : null,
    // The times the node went down other than by a shutdown: the kernel's
    // crash record read at the next boot, or the lack of one.
    (st.resets || []).length ? panel('Unplanned resets', { flush: true, meta: 'newest first' },
      table({ stack: true }, [
        { title: 'When', cell: (r) => (r.time_unknown ? h('span.faint.nowrap', { title: 'Clock not set at the time' }, 'Unknown') : h('span.mono.nowrap', stamp(r.time))) },
        { title: 'Cause', cell: (r) => h('span.nowrap', VERDICTS[r.verdict] || r.verdict) },
        { title: 'Detail', cls: 'wide', wide: true, cell: (r) => h('div', h('div', asSentence(r.summary)), r.detail ? h('div.faint.mono', { style: { fontSize: '12px' } }, r.detail) : null) },
        { title: 'Uptime', cls: 'num', cell: (r) => (r.uptime_s ? h('span.nowrap', duration(r.uptime_s)) : h('span.faint', '–')) },
      ], st.resets)) : null);
}
