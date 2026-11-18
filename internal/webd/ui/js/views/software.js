// Settings → System → Software: the release this node runs, and upgrading
// it from a release file. The node checks the file first (its signature,
// its architecture, that it is newer), installs it detached from this
// page, and goes back to the version before by itself when it is not
// healthy afterwards. meshd and webd restart on the way: the page loses
// the node for a minute, and asks to sign in again.
import { h } from '../dom.js';
import { get, post, upload } from '../api.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { panel, button, kv, notice, icon, table, pill } from '../ui.js';
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

// Every node: a mesh-wide upgrade from this node. The operator gives it
// one release file per architecture in the mesh; the node shows the plan
// (the order, farthest first and itself last, and who is left out and
// why), then sends the release through the mesh. Each node upgrades in
// turn and checks its own health; the first that goes back halts the
// rest. Progress comes with the live status (st.rollout).

function msw() {
  return (state.ui.meshUpgrade ||= { pkgs: [], plan: null, busy: '', error: null });
}

async function addFiles(files) {
  const m = msw();
  m.error = null; m.plan = null;
  for (const f of files) {
    m.busy = `Sending ${f.name} to this node…`; redraw();
    try {
      const p = await upload('/upgrade/mesh/package', f);
      m.pkgs = m.pkgs.filter((k) => k.arch !== p.arch).concat([{ ...p, name: f.name }]);
    } catch (e) {
      m.error = `${f.name}: ${e.message}`;
    }
  }
  await plan();
}

async function plan() {
  const m = msw();
  if (!m.pkgs.length) { m.busy = ''; redraw(); return; }
  m.busy = 'Asking the mesh…'; redraw();
  try {
    m.plan = await post('/upgrade/mesh?dry_run=1', { packages: m.pkgs.map((p) => p.sha256) });
  } catch (e) {
    m.plan = null;
    m.error = e.message;
  }
  m.busy = ''; redraw();
}

async function startRollout() {
  const m = msw();
  m.busy = 'Starting…'; m.error = null; redraw();
  try {
    await post('/upgrade/mesh', { packages: m.pkgs.map((p) => p.sha256) });
    m.pkgs = []; m.plan = null;
    toast('The mesh-wide upgrade has started.', 'ok', 6000);
  } catch (e) {
    m.error = e.message;
  }
  m.busy = ''; redraw();
}

function cancelRollout(r) {
  openDialog({
    title: `Cancel the upgrade to ${r.version}?`,
    body: () => [h('p', 'Nodes that have not begun upgrading stop and keep the release they run. A node upgrading now finishes, and goes back by itself if it is not healthy.')],
    actions: [
      { label: 'Keep going', run: closeDialog },
      { label: 'Cancel the upgrade', kind: 'danger', run: async () => {
        closeDialog();
        try { await post('/upgrade/mesh/cancel'); toast('Cancelled.', 'ok', 5000); } catch (e) { toast(e.message, 'bad', 8000); }
      } },
    ],
  });
}

const PHASE_TONE = { done: 'ok', upgrading: 'solid', 'rolled-back': 'bad', failed: 'bad', halted: 'warn', skipped: 'plain', ready: 'plain', fetching: 'plain' };
const PHASE_TEXT = { fetching: 'getting its package', ready: 'ready', upgrading: 'upgrading', done: 'done', skipped: 'skipped', 'rolled-back': 'went back', failed: 'failed', halted: 'halted' };

// rolloutSummary says where a rollout stands, in one line, with a tone.
export function rolloutSummary(r) {
  const nodes = r.nodes || [];
  const done = nodes.filter((n) => n.phase === 'done').length;
  const stop = nodes.find((n) => n.phase === 'rolled-back' || n.phase === 'failed');
  const halt = nodes.find((n) => n.phase === 'halted' && n.reason);
  const up = nodes.find((n) => n.phase === 'upgrading');
  const from = r.sender ? 'this node' : r.origin;
  if (stop) return { tone: 'error', text: `Upgrade to ${r.version} from ${from} halted: ${stop.callsign} ${stop.phase === 'failed' ? 'failed' : 'went back'}${stop.reason ? ` (${stop.reason})` : ''}. ${done} of ${nodes.length} upgraded.` };
  if (halt && !r.cancelled) return { tone: 'error', text: `Upgrade to ${r.version} from ${from} halted: ${halt.reason}. ${done} of ${nodes.length} upgraded.` };
  if (r.cancelled) return { tone: 'warning', text: `Upgrade to ${r.version} from ${from} cancelled. ${done} of ${nodes.length} upgraded${up ? `; ${up.callsign} is finishing` : ''}.` };
  if (r.ended) return { tone: 'ok', text: `Upgrade to ${r.version} from ${from} is over: ${done} of ${nodes.length} upgraded.` };
  return { tone: 'warning', text: `Upgrading the mesh to ${r.version} from ${from}: ${done} of ${nodes.length} done${up ? `; ${up.callsign} upgrading now` : ''}.` };
}

function progress(r) {
  const sum = rolloutSummary(r);
  return [
    notice({ level: sum.tone, message: sum.text }),
    h('div', { style: { marginTop: '12px' } }, table({ stack: true, key: (n) => n.mac + n.position }, [
      { title: '#', cls: 'num', cell: (n) => String(n.position) },
      { title: 'Node', cell: (n) => h('span', { style: { whiteSpace: 'nowrap' } }, h('span.mono', n.callsign), n.self ? h('span.faint', ' · this node') : null) },
      { title: 'State', cell: (n) => n.phase ? pill(PHASE_TEXT[n.phase] || n.phase, PHASE_TONE[n.phase] || 'plain') : h('span.faint', 'not heard yet') },
      { title: 'Detail', cell: (n) => {
        if (n.reason) return h('span.dim', n.reason);
        if (n.self && r.waiting && (n.phase === 'ready' || n.phase === 'fetching')) return h('span.faint', r.waiting);
        if (n.phase && n.since) return h('span.faint', `for ${since(n.since)}`);
        return null;
      } },
    ], r.nodes || [])),
  ];
}

function planView(m) {
  const p = m.plan;
  const order = p.nodes.filter((n) => n.position);
  const out = order.length;
  return h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px', marginTop: '12px' } },
    h('p', h('strong', `Release ${p.version}`), h('span.faint', '  ·  ' + p.packages.map((k) => `${k.arch}, signed by ${k.signed_by}`).join('  ·  '))),
    table({ stack: true, key: (n) => `${n.node_id}-${n.callsign}` }, [
      { title: '#', cls: 'num', cell: (n) => (n.position ? String(n.position) : '–') },
      { title: 'Node', cell: (n) => h('span', { style: { whiteSpace: 'nowrap' } }, h('span.mono', n.callsign), n.self ? h('span.faint', ' · this node') : null) },
      { title: 'Runs', cell: (n) => h('span.mono', n.version || '–') },
      { title: 'Arch', cell: (n) => n.arch || '–' },
      { title: 'Hops', cls: 'num', cell: (n) => (n.hops >= 0 ? String(n.hops) : '?') },
      { title: '', cell: (n) => (n.skip ? h('span.dim', 'left out: ' + n.skip) : pill('upgrades', 'plain')) },
    ], p.nodes),
    ...(p.warnings || []).map((w) => notice({ level: 'warning', message: w })),
    h('p.dim', { style: { fontSize: '14px' } },
      'One node at a time, farthest from this one first and this node last: each installs the release, restarts on it and checks its own health, and goes back by itself if it is not healthy. The first node that goes back halts the rest. A node’s links drop for a minute while it restarts.'),
    h('div.row',
      button(m.busy || `Upgrade ${out} ${out === 1 ? 'node' : 'nodes'} to ${p.version}`, startRollout, 'primary', { disabled: !!m.busy || !out }),
      button('Cancel', () => { m.pkgs = []; m.plan = null; m.error = null; redraw(); }, '', { disabled: !!m.busy })));
}

export function meshUpgradePanel(st) {
  const m = msw();
  const r = st.rollout;
  const running = r && !r.ended && !r.cancelled;
  const body = [];
  const tools = [];
  if (r) body.push(...progress(r));
  if (r && r.sender && !r.ended && !r.cancelled) tools.push(button('Cancel the upgrade', () => cancelRollout(r), 'small'));
  if (m.error) body.push(h('div', { style: { marginTop: '12px' } }, notice({ level: 'error', message: sentence(m.error) })));
  if (!running) {
    if (m.plan) {
      body.push(planView(m));
    } else {
      const others = (st.nodes || []).length;
      body.push(h('div.row', { style: { marginTop: '12px', alignItems: 'center' } },
        h('label.btn', { class: m.busy ? 'disabled' : '' }, icon('download'), m.busy || 'Choose release files',
          h('input', { type: 'file', multiple: true, accept: '.deb,application/vnd.debian.binary-package', hidden: true, disabled: !!m.busy,
            onchange: (e) => { const fs = [...e.target.files]; e.target.value = ''; if (fs.length) addFiles(fs); } })),
        h('span.faint', { style: { fontSize: '14px' } },
          `The trellis package for each architecture in the mesh (${others} other ${others === 1 ? 'node' : 'nodes'} known), signed by a release key this node trusts. Nothing starts before you see the plan and confirm.`)));
    }
  }
  return panel('Every node', { meta: r ? `rollout ${r.id}` : 'upgrade the whole mesh', tools }, body);
}
