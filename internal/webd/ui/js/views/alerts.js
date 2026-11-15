// Alerts: everything the node wants its operator to know.
import { h } from '../dom.js';
import { state, isIgnored, ignoreAlert } from '../state.js';
import { panel, notice } from '../ui.js';
import { plural } from '../fmt.js';

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
      title: on ? 'Show it on every page again' : 'Leave it out of the overview and the alert count, in this browser. It stays listed here.',
    }, on ? 'Show again' : 'Ignore'));
  return h('div.page',
    h('header.page-head', h('div', h('h1', 'Alerts'),
      h('p.sub', serious.length ? `${plural(serious.length, 'thing')} to look at.` : 'Nothing needs attention.',
        ignored.length ? ` ${plural(ignored.length, 'alert')} ignored.` : '',
        ' Faults are squares, warnings triangles, notes rings: the shapes hold in the night theme too.'))),
    shown.length
      ? h('div.notices', shown.map((w) => row(w, false)))
      : panel('', { class: 'quiet' }, h('div.empty', ignored.length ? 'Nothing else: the rest is ignored.' : 'No faults, warnings or notes.')),
    ignored.length ? h('section.ignored-alerts',
      h('div.label', 'Ignored'),
      h('p.dim', 'Left out of the overview and the alert count in this browser. One that turns more serious, or comes back after an hour gone, shows again.'),
      h('div.notices', ignored.map((w) => row(w, true)))) : null);
}
