// Alerts: everything the node wants its operator to know.
import { h } from '../dom.js';
import { state } from '../state.js';
import { panel, notice } from '../ui.js';
import { plural } from '../fmt.js';

export function alertsView() {
  const st = state.status;
  const rank = { error: 0, warning: 1, info: 2 };
  const all = [...st.warnings].sort((a, b) => rank[a.level] - rank[b.level]);
  const serious = all.filter((w) => w.level !== 'info');
  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Alerts'),
      h('p.sub', serious.length ? `${plural(serious.length, 'thing')} to look at.` : 'Nothing needs attention.', ' Faults are squares, warnings triangles, notes rings: the shapes hold in the night theme too.'))),
    all.length
      ? h('div.notices', all.map((w) => notice(w)))
      : panel('', { class: 'quiet' }, h('div.empty', 'No faults, warnings or notes.')));
}
