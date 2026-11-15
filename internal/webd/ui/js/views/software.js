// Settings → System → Software: the release this node runs, and upgrading
// it from a release file. The node checks the file first (its signature,
// its architecture, that it is newer), installs it detached from this
// page, and goes back to the version before by itself when it is not
// healthy afterwards. meshd and webd restart on the way: the page loses
// the node for a minute, and asks to sign in again.
import { h } from '../dom.js';
import { get, post, upload } from '../api.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { panel, button, kv, notice, icon } from '../ui.js';
import { since } from '../fmt.js';

const POLL_MS = 2000;
const WAIT_LIMIT_MS = 8 * 60 * 1000; // an upgrade ends within some five minutes

// Messages from the node start in lower case, as Go errors do.
const sentence = (m) => (m ? m[0].toUpperCase() + m.slice(1) : m);

function sw() {
  // waitingSince: when this page started an upgrade (for its time limit);
  // lastBefore: the last result the node had then, so the new one is told
  // apart without comparing the node's clock with this device's.
  return (state.ui.software ||= { st: null, error: null, file: null, check: null, busy: false, timer: null, waitingSince: 0, lastBefore: null, lostSince: 0 });
}

async function refresh() {
  const s = sw();
  s.timer = null;
  try {
    s.st = await get('/upgrade');
    s.error = null;
    s.lostSince = 0;
    // A run that ended without recording an upgrade (refused before it
    // began, or with nothing to do): its log's last line says how.
    const tail = (s.st.log || [])[(s.st.log || []).length - 1] || '';
    if (s.waitingSince && !s.st.running && !s.st.pending && (!s.st.last || s.st.last.finished === s.lastBefore)
        && (tail.startsWith('error: ') || tail === '== done')) {
      s.waitingSince = 0;
      if (tail.startsWith('error: ')) s.error = tail.slice(7);
    }
    if (s.waitingSince && !s.st.running && !s.st.pending && s.st.last && s.st.last.finished !== s.lastBefore) {
      s.waitingSince = 0;
      const l = s.st.last;
      if (l.result === 'upgraded') toast(`Upgraded to ${l.to}.`, 'ok', 6000);
      else if (l.result === 'rolled-back' && l.reason === 'asked for by the operator') toast(`Back on ${l.from}.`, 'ok', 6000);
      else toast(`The node went back to ${l.from}: ${l.reason}`, 'bad', 10000);
    }
  } catch (e) {
    // While meshd and webd restart, the node does not answer for a while.
    if (s.waitingSince) s.lostSince ||= Date.now();
    else s.error = e.message;
  }
  if (s.waitingSince && Date.now() - s.waitingSince > WAIT_LIMIT_MS) s.waitingSince = 0;
  redraw();
}

function schedule() {
  const s = sw();
  if (s.timer) return;
  const busy = s.waitingSince || (s.st && (s.st.running || s.st.pending));
  if (!s.st && !s.error) s.timer = setTimeout(refresh, 0);
  else if (busy) s.timer = setTimeout(refresh, POLL_MS);
}

async function checkFile(file) {
  const s = sw();
  s.file = file; s.check = null; s.error = null; s.busy = true; redraw();
  try {
    s.check = await upload('/upgrade?dry_run=1', file);
  } catch (e) {
    s.file = null;
    s.error = e.message;
  }
  s.busy = false; redraw();
}

async function start() {
  const s = sw();
  s.busy = true; redraw();
  try {
    s.lastBefore = s.st && s.st.last ? s.st.last.finished : null;
    // The node kept the file it checked: name it, do not send it again.
    await post('/upgrade?sha256=' + encodeURIComponent(s.check.sha256));
    s.waitingSince = Date.now();
    s.file = null; s.check = null;
    toast('The upgrade has started. The node restarts on the way: this page loses it for a minute.', 'ok', 8000);
  } catch (e) {
    s.error = e.message;
  }
  s.busy = false;
  refresh();
}

function rollback(l) {
  openDialog({
    title: `Go back to ${l.from}?`,
    body: () => [
      h('p', `The node installs ${l.from} again and restarts on it. Its configuration, mesh profile and identity stay as they are now, if ${l.from} can read them.`),
      h('p.dim', { style: { fontSize: '14px' } }, `If you changed a setting since the upgrade that ${l.from} does not know, the settings from before the upgrade come back instead, and the changes since are undone; the log says so. Every node of a mesh should run the same release.`),
    ],
    actions: [
      { label: 'Cancel', run: closeDialog },
      { label: `Go back to ${l.from}`, kind: 'primary', run: async () => {
        closeDialog();
        const s = sw();
        try {
          s.lastBefore = s.st && s.st.last ? s.st.last.finished : null;
          await post('/upgrade/rollback');
          s.waitingSince = Date.now();
          toast('Going back. The node restarts on the way.', 'ok', 6000);
        } catch (e) { toast(e.message, 'bad', 8000); }
        refresh();
      } },
    ],
  });
}

const PHASES = { installing: 'Installing', checking: 'Checking the node’s health', 'rolling-back': 'Going back' };

function lastLine(l) {
  const when = since(l.finished) + ' ago';
  switch (l.result) {
    case 'upgraded': return h('span', h('span.ok', `${l.from} → ${l.to}`), h('span.faint', `  ·  ${when}`));
    case 'unchanged': return h('span', h('span.faint', `${l.from} → ${l.to}: not installed (interrupted before)  ·  ${when}`));
    case 'rolled-back':
      return l.reason === 'asked for by the operator'
        ? h('span', `back to ${l.from} from ${l.to}, as asked`, h('span.faint', `  ·  ${when}`))
        : h('span', h('span.warn', `${l.from} → ${l.to}: went back to ${l.from}`), h('span.faint', `  ·  ${l.reason}  ·  ${when}`));
  }
  return h('span.bad', `${l.from} → ${l.to}: failed, not undone: ${l.reason}`);
}

export function softwarePanel(st) {
  const s = sw();
  schedule();
  const u = s.st;
  const rows = [];
  if (u) {
    rows.push(['Package', u.package ? u.package : h('span.faint', 'not installed from a package')]);
    rows.push(['Release keys', u.trusted_keys ? `${u.trusted_keys} trusted` : h('span.faint', 'none trusted')]);
    if (u.last) rows.push(['Last upgrade', lastLine(u.last)]);
  }
  const body = [kv(rows)];
  const tools = [];
  const busy = u && (u.running || u.pending);

  if (s.error) body.push(notice({ level: 'error', message: sentence(s.error) }));
  if (!u) {
    body.push(h('p.dim', 'Asking the node…'));
  } else if (busy || s.waitingSince) {
    const p = u.pending;
    body.push(h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px', marginTop: '12px' } },
      h('p', h('strong', !p ? 'Starting the upgrade…' : p.phase === 'rolling-back' ? `Going back: ${p.to} → ${p.from}` : `${PHASES[p.phase] || p.phase}: ${p.from} → ${p.to}`),
        p ? h('span.faint', `  ·  for ${since(p.started)}`) : null),
      s.lostSince ? notice({ level: 'info', message: 'The node is restarting and does not answer for the moment. This page keeps asking; you may have to sign in again.' }) : null,
      (u.log || []).length ? h('pre.block', { style: { maxHeight: '260px' } }, u.log.join('\n')) : null));
  } else if (!u.package) {
    body.push(notice({ level: 'info', message: 'Trellis was installed on this node by the install scripts, not from a package, so there is no version to go back to. Install the package on the node once (sudo apt install ./trellis_VERSION_ARCH.deb); later upgrades can then be made here.' }));
  } else if (!u.trusted_keys) {
    body.push(notice({ level: 'info', message: 'This node trusts no release key yet, so it takes no package from here. On the node, put the release’s public key in /etc/trellis/release-keys/NAME.pub.' }));
  } else if (s.check) {
    const c = s.check;
    body.push(h('div.notice.info', { style: { marginTop: '12px' } },
      h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } },
        h('p', h('strong', `${c.from} → ${c.to}`), h('span.faint', `  ·  ${c.arch}  ·  signed by ${c.signed_by}`)),
        h('p.dim', { style: { fontSize: '14px' } },
          `The node installs it, restarts on it and checks itself for up to three minutes: Trellis on ${c.to}, nothing failing that worked before, its mesh links back. If it is not healthy, it goes back to ${c.from} and its configuration by itself. This page loses the node for a minute while it restarts; you may have to sign in again.`),
        h('div.row', button(s.busy ? 'Starting…' : `Upgrade to ${c.to}`, start, 'primary', { disabled: s.busy }),
          button('Cancel', () => { s.file = null; s.check = null; redraw(); }, '', { disabled: s.busy })))));
  } else {
    body.push(h('div.row', { style: { marginTop: '12px', alignItems: 'center' } },
      h('label.btn', { class: s.busy ? 'disabled' : '' }, icon('download'), s.busy ? 'Checking the file…' : 'Choose a release file',
        h('input', { type: 'file', accept: '.deb,application/vnd.debian.binary-package', hidden: true, disabled: s.busy,
          onchange: (e) => { const f = e.target.files[0]; e.target.value = ''; if (f) checkFile(f); } })),
      h('span.faint', { style: { fontSize: '14px' } }, `The trellis package for this node (${u.arch || 'its architecture'}), signed by a release key it trusts. Nothing is installed before you confirm.`)));
    if (u.last && u.last.can_roll_back) tools.push(button(`Go back to ${u.last.from}`, () => rollback(u.last), 'small'));
  }
  tools.push(button('', () => { s.error = null; refresh(); }, 'small', { icon: 'refresh', title: 'Ask again' }));
  return panel('Software', { meta: u && u.package ? 'from a package' : '', tools }, body);
}
