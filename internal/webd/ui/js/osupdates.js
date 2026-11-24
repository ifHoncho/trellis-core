// OS package updates (internal/osupdate): Settings → System. The node runs
// apt in a helper; this shows its record and starts a check or an upgrade.
import { h } from './dom.js';
import { get, post } from './api.js';
import { state, redraw, toast } from './state.js';
import { panel, kv, button } from './ui.js';
import { since, plural } from './fmt.js';

const running = (s) => s && (s.phase === 'checking' || s.phase === 'updating');

async function load(ui) {
  try { ui.st = await get('/os'); ui.error = ''; } catch (e) { ui.error = e.message; }
  ui.loading = false;
  // While a run is under way, its record is read again every 2 s; a read
  // that failed (meshd restarting) is tried again the same way.
  clearTimeout(ui.timer);
  if (running(ui.st)) ui.timer = setTimeout(() => load(ui), 2000);
  redraw();
}

export function osUpdatesPanel() {
  const ui = (state.ui.os ||= { st: null, loading: false, busy: false, error: '' });
  if (!ui.st && !ui.loading && !ui.error) { ui.loading = true; load(ui); }
  const start = async (op) => {
    ui.busy = true; redraw();
    try { ui.st = await post('/os/' + op); toast(op === 'check' ? 'OS update check started.' : 'OS update started.'); } catch (e) { toast(e.message, 'bad', 6000); }
    ui.busy = false;
    load(ui);
  };
  const s = ui.st;
  const retry = () => { ui.loading = true; ui.error = ''; redraw(); load(ui); };
  if (!s && ui.error) return panel('OS updates', {}, h('div.form', h('p.bad', ui.error), h('div.row.end', button('Retry', retry))));
  if (!s) return panel('OS updates', {}, h('p.faint', 'Loading…'));
  const state_ = {
    checking: h('span.warn', 'Checking…'),
    updating: h('span.warn', 'Updating…'),
    done: h('span.ok', 'Updated'),
    failed: h('span.bad', `Failed · ${s.error || 'unknown'}`),
  }[s.phase] || h('span', 'Idle');
  const pending = s.pending == null ? h('span.faint', 'Unknown · not checked')
    : s.pending === 0 ? h('span.ok', 'None')
      : h('span', h('span.warn', plural(s.pending, 'package')),
        h('span.faint', `  ·  ${(s.packages || []).slice(0, 6).join(', ')}${s.pending > 6 ? ', …' : ''}`));
  const rows = [
    ['State', state_],
    ['Pending', pending],
    ['Last check', s.checked_at ? `${since(s.checked_at)} ago` : h('span.faint', 'Never')],
  ];
  if (s.phase === 'done' && s.finished_at) rows.push(['Last update', `${since(s.finished_at)} ago · ${plural(s.upgraded || 0, 'package')}`]);
  if (s.reboot_required) rows.push(['Reboot', h('span.warn', 'Required')]);
  return panel('OS updates', { meta: 'apt · kernel held' },
    h('div.form',
      kv(rows),
      s.phase === 'failed' && (s.log || []).length ? h('details', h('summary', 'Log'), h('pre.block', s.log.join('\n'))) : null,
      ui.error ? h('p.bad', ui.error) : null,
      h('div.row.end',
        button('Check', () => start('check'), '', { disabled: ui.busy || running(s) }),
        button('Update now', () => start('upgrade'), 'primary', { disabled: ui.busy || running(s) }))));
}
