// Settings → System → Driver trials: new driver modules, firmware or board
// files being tried on this node, and the last trial of each. A trial is
// kept once the node has run well on it for a while, and undone if the
// node restarts first; one that was undone stays off until it is re-armed
// (meshctl trial). Shown only on a node that has had one.
import { h } from '../dom.js';
import { panel, table, pill } from '../ui.js';
import { duration } from '../fmt.js';
import { stamp } from '../time.js';

const STATES = {
  armed: ['info', 'armed'], pending: ['warn', 'on trial'], confirmed: ['ok', 'confirmed'], reverted: ['plain', 'reverted'],
};

function note(t) {
  if (t.state === 'armed') return 'Loads at next boot.';
  if (t.state === 'pending') {
    return t.confirm_after_s ? `Confirms in ${duration(t.confirm_after_s)} if healthy. Reverts on reboot.`
      : 'Not healthy. Reverts if not healthy by the deadline.';
  }
  if (t.state === 'confirmed') return `Previous version kept. Revert: sudo meshctl trial revert ${t.name}`;
  // The reason is the node's own sentence, without its capital or stop.
  const why = t.reason ? `Reverted: ${t.reason}.` : 'Reverted.';
  return why + (t.latched ? ` Locked out. Re-arm: sudo meshctl trial rearm ${t.name}` : '')
    + (t.reboot_needed ? ' Reboot to load the previous version.' : '');
}

export function trialsPanel(st) {
  const trials = st.trials || [];
  if (!trials.length) return null;
  return panel('Driver trials', { flush: true, meta: 'drivers, firmware, board files' },
    table({ stack: true, key: (t) => t.name }, [
      { title: 'What', cell: (t) => h('div', h('div.primary', t.name), h('div.secondary', [t.kind, t.version, t.module].filter(Boolean).join(' · '))) },
      { title: 'State', cell: (t) => { const [tone, word] = STATES[t.state] || ['plain', t.state]; return pill(t.latched ? 'reverted, locked' : word, t.latched ? 'bad' : tone); } },
      { title: 'Since', cell: (t) => h('span.mono.nowrap', stamp(Date.parse(t.ended) > 86400000 ? t.ended : t.started)) },
      { title: 'Note', cls: 'wide', wide: true, cell: (t) => note(t) },
    ], trials));
}
