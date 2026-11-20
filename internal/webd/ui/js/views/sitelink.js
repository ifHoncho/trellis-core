// Site link: parts of this mesh in other places, joined over the internet.
// This node's key and its tunnels, and the members every node shares.
import { h } from '../dom.js';
import { state, redraw, toast, openDialog, closeDialog } from '../state.js';
import { post } from '../api.js';
import { siteSummary } from './overview.js';
import { panel, field, button, table, notice, pill, copyText } from '../ui.js';
import { bytes, ago } from '../fmt.js';
import { ensureConfig, saveMeshProfile, changeLock, editProfile, sentPending, noteUnsaved } from '../config.js';
import { clock } from '../time.js';

// How long after it is sent a change to the members is in force. Nothing
// on the air changes with it, so the shortest wait the Settings page
// offers is enough for every node to have it.
const SWITCH_IN = 30;

const short = (key) => (key ? key.slice(0, 8) + '…' : '');
const peerName = (p) => p.name || p.callsign || short(p.public_key);

function handshake(p) {
  // No word from the node yet on why: the tunnel was set up seconds ago.
  if (!p.last_handshake) return p.note ? pill('no handshake', 'bad') : pill('connecting', 'plain');
  const age = Date.now() - Date.parse(p.last_handshake);
  return h('span', pill(p.up ? 'up' : 'down', p.up ? 'ok' : 'bad'), h('span.faint', ` ${ago(age)} ago`));
}

function reachedAt(p) {
  const shown = p.current || p.endpoint;
  const second = !p.endpoint ? 'inbound only (no endpoint)'
    : p.current && p.current !== p.endpoint && p.current !== `${p.resolved}:${p.endpoint.split(':').pop()}` ? `profile: ${p.endpoint}`
      : p.resolved ? `${p.endpoint}` : '';
  return h('div', h('div.primary.mono', shown || h('span.faint', 'waiting')), second ? h('div.secondary', second) : null);
}

async function copyKey(key) {
  toast(await copyText(key) ? 'Key copied.' : 'Copy failed. Copy manually.', 'ok', 4000);
}

// newKey gives this node a new site link key, after saying what that
// does. A member leaves the link until the profile lists the new key: the
// members list below is given it, ready to send.
function newKey(st) {
  const sl = st.site_link;
  const ui = { busy: false };
  const run = async () => {
    ui.busy = true; redraw();
    try {
      const k = await post('/site-link/key', { force: sl.member });
      const ed = state.ui.siteMembers;
      // Kept for as long as the page is open: the old key is dead, so a
      // draft made again from a list that still names it (after a change
      // on its way switched) is given the new key too.
      (state.ui.siteRekey ||= []).push({ was: k.was, now: k.public_key });
      let swapped = false;
      if (ed && ed.draft) for (const m of ed.draft) if (m.public_key === k.was) { m.public_key = k.public_key; swapped = true; }
      closeDialog();
      toast(swapped ? 'New key generated and added to the member list. Review and send to restore the link.' : 'New key generated. Old key revoked.', 'ok', 10000);
    } catch (e) {
      toast(e.message, 'bad', 8000);
    }
    ui.busy = false; redraw();
  };
  openDialog({
    title: 'Generate new key?',
    body: () => [
      h('p', 'The key identifies this node on the site link. Replacement is permanent: the old key is revoked.'),
      sl.member
        ? notice({ level: 'warning', message: `This node is the site link for site ${sl.site}. A new key drops its tunnels until the profile at every site lists the new key.` })
        : notice({ level: 'info', message: 'Not a member: no interruption. Any member list with the old key (unsent or in a file) must be updated.' }),
      h('p.dim', { style: { fontSize: '14px' } }, 'Use if the key may be compromised. For a lost or captured node, remove it from the member list instead.'),
    ],
    actions: [
      { label: 'Cancel', run: () => closeDialog() },
      { label: 'Generate new key', kind: sl.member ? 'danger' : 'primary', run, disabled: () => ui.busy },
    ],
  });
}

// statusPanel: this node's key, and its tunnels when it is a member.
function statusPanel(st) {
  const sl = st.site_link;
  const peers = sl.peers || [];
  const meta = sl.member ? `site ${sl.site} · ${sl.address} · UDP ${sl.port}` : sl.members ? 'not a member' : 'not configured';
  const notes = peers.filter((p) => p.note);
  const sum = siteSummary(st);
  return panel('Site link', { meta, flush: sl.member && peers.length > 0 },
    h('div.sitekey', { style: sl.member && peers.length ? { padding: '14px 16px 4px' } : {} },
      h('div.label', 'Node key'),
      h('div.row', h('code.mono.keytext', sl.public_key || '–'),
        sl.public_key ? button('Copy', () => copyKey(sl.public_key), 'small') : null,
        sl.public_key ? button('New key…', () => newKey(st), 'small', { title: 'Replace node key (if compromised)' }) : null),
      sl.member ? h('div.secondary', { style: { marginTop: '8px' } }, sl.endpoint
        ? ['Endpoint ', h('span.mono', sl.endpoint), '.']
        : 'No endpoint in the profile: outbound connections only.') : null),
    sl.error ? h('div', { style: { padding: '0 16px' } }, notice({ level: 'error', message: sl.error })) : null,
    sl.member
      ? [table({ stack: true, key: (p) => p.public_key }, [
          { title: 'Peer', cls: 'wide', wide: true, cell: (p) => h('div', h('div.primary', peerName(p)),
            h('div.secondary', `site ${p.site}`, p.callsign && p.callsign !== p.name ? ` · ${p.callsign}` : '')) },
          { title: 'Endpoint', cell: reachedAt },
          { title: 'Handshake', cell: handshake },
          { title: 'Traffic', cls: 'num', cell: (p) => h('span', `↓ ${bytes(p.rx_bytes)}`, h('span.faint', ' · '), `↑ ${bytes(p.tx_bytes)}`) },
          { title: 'Tunnel', cell: (p) => h('div', h('div.primary.mono', p.interface), h('div.secondary', p.batman ? `${p.neighbors} neighbour${p.neighbors === 1 ? '' : 's'}` : 'not in use')) },
        ], peers, 'No members at other sites: nothing to link. Members of the same site do not link.'),
        // Why a tunnel is not up, and what to look at: the node's words.
        notes.length ? h('div.notices', { style: { padding: '12px 16px 14px' } },
          notes.map((p) => notice({ level: 'warning', message: `${peerName(p)} (site ${p.site}): ${p.note}.` })),
          h('p.dim', { style: { fontSize: '13px', margin: '2px 0 0' } }, 'Peers listed per the profile. To remove one, delete it under Members and send.')) : null]
      : [
          // A node that is no member still says how the link stands.
          sum ? h('div', { style: { marginTop: '14px' } }, notice({ level: { ok: 'ok', warn: 'warning', bad: 'error' }[sum.tone], message: `Site link: ${sum.text}. ${sum.note}` })) : null,
          h('p.dim', { style: { margin: '12px 0 0', fontSize: '14px', maxWidth: '70ch' } },
            'Joins remote parts of the mesh over the internet. One or two nodes with an uplink per site carry the link; list them below with their keys.')]);
}

// clean is a member as the profile holds it: empty fields left out.
const clean = (m) => Object.fromEntries(Object.entries(m).filter(([, v]) => v !== '' && v != null));
const listAnd = (xs) => (xs.length < 2 ? xs.join('') : xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1]);

// notSent says what a draft changes in the list the nodes have. A member
// with the same name and site under another key was given a new key.
function notSent(was, now) {
  const name = (m) => m.name || short(m.public_key) || 'member, no key';
  const before = new Map(was.map((m) => [m.public_key, m]));
  const after = new Set(now.map((m) => m.public_key));
  const gone = was.filter((m) => !after.has(m.public_key));
  const fresh = now.filter((m) => !before.has(m.public_key));
  const rekeyed = fresh.filter((m) => m.name && gone.some((o) => o.name === m.name && o.site === m.site));
  const same = (m) => rekeyed.some((r) => r.name === m.name && r.site === m.site);
  const adds = fresh.filter((m) => !same(m)).map(name);
  const removes = gone.filter((m) => !same(m)).map(name);
  const edits = now.filter((m) => before.has(m.public_key) && JSON.stringify(clean(before.get(m.public_key))) !== JSON.stringify(clean(m))).map(name);
  return [adds.length ? `adds ${listAnd(adds)}` : '', removes.length ? `removes ${listAnd(removes)}` : '', edits.length ? `changes ${listAnd(edits)}` : '',
    rekeyed.length ? `re-keys ${listAnd(rekeyed.map(name))}` : '']
    .filter(Boolean).join('; ');
}

// membersPanel edits the profile's site link members and sends them to
// every node.
function membersPanel(st) {
  const c = ensureConfig();
  if (!state.config) return panel('Members', {}, h('p.dim', c.error || 'Loading…'));
  const ui = (state.ui.siteMembers ||= { draft: null, for: '', fields: {}, busy: false });
  const cur = editProfile();
  const curMembers = JSON.stringify((cur.site_link && cur.site_link.members) || []);
  // The list the nodes have changed (a change reached its moment, or came
  // from elsewhere): the draft starts again from it.
  if (!ui.draft || ui.for !== curMembers) { ui.draft = JSON.parse(curMembers); ui.for = curMembers; ui.fields = {}; }
  const ms = ui.draft;
  const was = JSON.parse(ui.for);
  // A new key made on this page replaces the old one wherever a draft
  // names it (in order: a key may have been replaced twice).
  for (const rk of state.ui.siteRekey || []) {
    if (rk.was) for (const m of ms) if (m.public_key === rk.was) m.public_key = rk.now;
  }
  const changed = JSON.stringify(ms.map(clean)) !== JSON.stringify(was.map(clean));
  // The change on its way is this draft only when this editor sent it.
  const sent = sentPending('members', st);
  noteUnsaved('members', changed && !sent);
  const others = st.nodes || [];
  const mine = st.site_link.public_key;
  const listed = ms.some((m) => m.public_key === mine);
  // A change on its way: until every node has switched, nothing more is
  // sent, and the list stays as it was sent.
  const pending = changeLock(st);
  const locked = !!pending || ui.busy;
  // Who holds a key: this node, or a node this one hears.
  const holder = new Map(others.filter((n) => n.site_key).map((n) => [n.site_key, n.callsign]));
  if (mine) holder.set(mine, st.node.callsign);
  const send = async () => {
    ui.busy = true; ui.fields = {}; redraw();
    try {
      const p = JSON.parse(JSON.stringify(cur));
      p.site_link = { ...(p.site_link || {}), members: ms.map(clean) };
      if (!p.site_link.members.length) delete p.site_link.members;
      await saveMeshProfile({ profile: p }, SWITCH_IN, 'members');
    } catch (e) {
      for (const f of e.fields || []) ui.fields[f.field] = f.message;
      toast(e.message.split('\n')[0], 'bad', 7000);
    }
    ui.busy = false; redraw();
  };
  const err = (i, k) => ui.fields[`site_link.members[${i}].${k}`];
  const input = (m, k, i, attrs) => h('input.input', { value: m[k] || '', class: (attrs.mono ? 'mono ' : '') + (err(i, k) ? 'invalid' : ''), placeholder: attrs.placeholder, maxlength: attrs.max, spellcheck: 'false',
    list: attrs.list, disabled: locked, 'aria-label': attrs.label,
    oninput: (e) => { m[k] = e.target.value.trim(); redraw(); } });
  const sites = [...new Set(ms.map((m) => m.site).filter(Boolean))];
  const mySite = (ms.find((m) => m.public_key === mine) || {}).site || '';
  // Nodes this one hears that are not listed yet: added by name, no key
  // to copy. Before a link is up they are all at this node's site.
  const addable = others.filter((n) => n.site_key && !ms.some((m) => m.public_key === n.site_key))
    .sort((a, b) => a.callsign.localeCompare(b.callsign));
  // What is known of a member the nodes already have: who holds its key,
  // and how its tunnels stand, as that node reports them.
  const roster = new Map((st.site_link.roster || []).map((r) => [r.public_key, r]));
  const tunnels = (r) => (!r.reported ? '' : r.peers === 0 ? ' · no other site' : ` · ${r.up} of ${r.peers} tunnel${r.peers === 1 ? '' : 's'} up`);
  const keyHint = (m) => {
    if (!m.public_key) return '';
    const r = roster.get(m.public_key);
    if (m.public_key === mine) return 'this node' + (r ? tunnels(r) : '');
    if (r && r.heard) return `${r.callsign}` + (tunnels(r) || ' · heard, tunnels not reported');
    if (r) return 'not heard: site not joined, or node off';
    return holder.has(m.public_key) ? `${holder.get(m.public_key)} (heard)` : '';
  };
  const words = changed ? notSent(was, ms) : '';
  const secs = pending ? Math.max(0, Math.round((Date.parse(pending.apply_at) - Date.now()) / 1000)) : 0;
  const have = pending ? (pending.nodes || []).filter((n) => n.state === 'has it').length : 0;

  return panel('Members', { meta: 'same on all nodes' },
    h('div.form',
      pending ? notice({ level: 'warning', message: changed && sent
        ? `Sent${pending.sender && (pending.nodes || []).length ? `: ${have}/${pending.nodes.length} nodes received` : ''}. Switchover at ${clock(pending.apply_at)} (in ${secs} s).`
        : `Profile change from ${pending.sender ? 'this node' : pending.from} pending: switchover at ${clock(pending.apply_at)} (in ${secs} s). Members editable after switchover.${changed ? ' Edits below not sent.' : ''}` }) : null,
      h('datalist', { id: 'site-names' }, sites.map((s) => h('option', { value: s }))),
      ms.length ? null : h('p.dim', { style: { margin: '0' } }, was.length ? 'No members: sending disables the site link on all nodes.' : 'No members: site link off.'),
      ms.map((m, i) => h('div.fields.sitemember',
        field('Name', input(m, 'name', i, { placeholder: 'label', max: '32' }), { error: err(i, 'name') }),
        field('Site', input(m, 'site', i, { placeholder: 'hq', max: '32', list: 'site-names' }), { error: err(i, 'site') }),
        field('Public key', input(m, 'public_key', i, { placeholder: 'from meshctl site, or this page on that node', mono: true, max: '44' }),
          { error: err(i, 'public_key'), hint: keyHint(m) }),
        field('Endpoint', input(m, 'endpoint', i, { placeholder: 'host:51873', mono: true, max: '260' }),
          { error: err(i, 'endpoint'), hint: m.endpoint ? '' : 'none: outbound only (behind NAT)' }),
        h('div.field', h('span.label', { 'aria-hidden': 'true' }, ' '),
          h('div', button('Remove', () => { ms.splice(i, 1); ui.fields = {}; redraw(); }, 'small', { disabled: locked, title: `Remove ${m.name || 'member'}` }))))),
      h('div.row', { style: { flexWrap: 'wrap' } },
        !listed && mine ? button('Add this node', () => { ms.push({ name: st.node.callsign, site: sites[0] || '', public_key: mine, endpoint: '' }); redraw(); }, 'small', { disabled: locked }) : null,
        addable.length ? h('select.select', { value: '', disabled: locked || ms.length >= 64, 'aria-label': 'Add a heard node', style: { width: 'auto' },
          onchange: (e) => {
            const n = addable.find((x) => x.site_key === e.target.value);
            e.target.value = '';
            if (n) { ms.push({ name: n.callsign, site: mySite || (sites.length === 1 ? sites[0] : ''), public_key: n.site_key, endpoint: '' }); redraw(); }
          } },
          h('option', { value: '' }, 'Add a heard node…'),
          addable.map((n) => h('option', { value: n.site_key }, n.callsign))) : null,
        button('Add by key', () => { ms.push({ name: '', site: '', public_key: '', endpoint: '' }); redraw(); }, 'small', { disabled: locked || ms.length >= 64, title: 'Remote node not yet reachable: paste its key' }),
        was.length && ms.length ? button('Remove all', () => { ms.length = 0; ui.fields = {}; redraw(); }, 'small', { disabled: locked, title: 'Clear the list (once sent, disables the site link on all nodes)' }) : null),
      h('p.dim', { style: { margin: '0', fontSize: '13px', maxWidth: '75ch' } }, 'Members link to members at other sites only. Endpoint: public address or name, and the forwarded UDP port. At least one end of each link needs an endpoint.'),
      ms.length && sites.length < 2 ? notice({ level: 'info', message: 'Members of one site do not link: at least two sites required.' }) : null,
      h('div.row.between', { style: { flexWrap: 'wrap', gap: '12px' } },
        h('span.dim', { style: { fontSize: '14px', maxWidth: '70ch' } }, changed && !sent ? `Not sent: ${words}. Nodes keep the current list until sent.` : ''),
        h('div.row',
          changed && !sent ? button('Discard', () => { ui.draft = null; redraw(); }, '', { disabled: ui.busy, title: 'Discard edits' }) : null,
          button(ui.busy ? 'Checking…' : pending ? 'Waiting for switchover…' : others.length ? 'Review and send to all nodes' : 'Review and save', send, 'primary', { disabled: !changed || locked })))),
    h('p.dim', { style: { marginTop: '14px', fontSize: '13px', maxWidth: '75ch' } },
      'The list reaches only nodes reachable from here; before the link is up, this site only. At each other site, install the profile on one node (here: Settings → Mesh profile → Share profile → Download file; there: Settings → Mesh profile → Install another profile). That site’s other nodes receive it from that node. Removals work the same way, over the link or per site.'));
}

export function siteLink(st) {
  if (!st.site_link) return null;
  return [statusPanel(st), membersPanel(st)];
}
