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
  if (!p.last_handshake) return p.note ? pill('no answer', 'bad') : pill('connecting', 'plain');
  const age = Date.now() - Date.parse(p.last_handshake);
  return h('span', pill(p.up ? 'up' : 'down', p.up ? 'ok' : 'bad'), h('span.faint', ` ${ago(age)} ago`));
}

function reachedAt(p) {
  const shown = p.current || p.endpoint;
  const second = !p.endpoint ? 'dials in (no endpoint)'
    : p.current && p.current !== p.endpoint && p.current !== `${p.resolved}:${p.endpoint.split(':').pop()}` ? `profile: ${p.endpoint}`
      : p.resolved ? `${p.endpoint}` : '';
  return h('div', h('div.primary.mono', shown || h('span.faint', 'waiting for it')), second ? h('div.secondary', second) : null);
}

async function copyKey(key) {
  toast(await copyText(key) ? 'Key copied.' : 'Copy it by hand: the browser refused.', 'ok', 4000);
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
      toast(swapped ? 'New key made. The members list below now has it: review and send it to put this node back on the link.' : 'New key made. The old one no longer works.', 'ok', 10000);
    } catch (e) {
      toast(e.message, 'bad', 8000);
    }
    ui.busy = false; redraw();
  };
  openDialog({
    title: 'Give this node a new key?',
    body: () => [
      h('p', 'The key is how the profile names this node on the site link. A new one replaces it for good: the old key stops working, and cannot be brought back.'),
      sl.member
        ? notice({ level: 'warning', message: `This node holds the site link for site ${sl.site} under its current key. With a new key it leaves the link at once, and its tunnels close, until the profile lists the new key in its place at every site.` })
        : notice({ level: 'info', message: 'This node is not a member, so nothing is interrupted. A list that names its old key (one not sent yet, or kept in a file) has to be given the new one.' }),
      h('p.dim', { style: { fontSize: '14px' } }, 'Make a new key when the current one may have been copied. A node that is lost or captured is taken off the members list instead.'),
    ],
    actions: [
      { label: 'Cancel', run: () => closeDialog() },
      { label: 'Make a new key', kind: sl.member ? 'danger' : 'primary', run, disabled: () => ui.busy },
    ],
  });
}

// statusPanel: this node's key, and its tunnels when it is a member.
function statusPanel(st) {
  const sl = st.site_link;
  const peers = sl.peers || [];
  const meta = sl.member ? `site ${sl.site} · ${sl.address} · UDP ${sl.port}` : sl.members ? 'this node is not a member' : 'not set up';
  const notes = peers.filter((p) => p.note);
  const sum = siteSummary(st);
  return panel('Site link', { meta, flush: sl.member && peers.length > 0 },
    h('div.sitekey', { style: sl.member && peers.length ? { padding: '14px 16px 4px' } : {} },
      h('div.label', 'This node’s key'),
      h('div.row', h('code.mono.keytext', sl.public_key || '–'),
        sl.public_key ? button('Copy', () => copyKey(sl.public_key), 'small') : null,
        sl.public_key ? button('New key…', () => newKey(st), 'small', { title: 'Replace this node’s key: when the current one may have been copied' }) : null),
      sl.member ? h('div.secondary', { style: { marginTop: '8px' } }, sl.endpoint
        ? ['The other sites reach this node at ', h('span.mono', sl.endpoint), '.']
        : 'This node has no endpoint in the profile: it dials the other sites, they cannot dial it.') : null),
    sl.error ? h('div', { style: { padding: '0 16px' } }, notice({ level: 'error', message: sl.error })) : null,
    sl.member
      ? [table({ stack: true, key: (p) => p.public_key }, [
          { title: 'Peer', cls: 'wide', wide: true, cell: (p) => h('div', h('div.primary', peerName(p)),
            h('div.secondary', `site ${p.site}`, p.callsign && p.callsign !== p.name ? ` · ${p.callsign}` : '')) },
          { title: 'Reached at', cell: reachedAt },
          { title: 'Handshake', cell: handshake },
          { title: 'Traffic', cls: 'num', cell: (p) => h('span', `↓ ${bytes(p.rx_bytes)}`, h('span.faint', ' · '), `↑ ${bytes(p.tx_bytes)}`) },
          { title: 'Tunnel', cell: (p) => h('div', h('div.primary.mono', p.interface), h('div.secondary', p.batman ? `${p.neighbors} neighbour${p.neighbors === 1 ? '' : 's'} over it` : 'not in use')) },
        ], peers, 'The profile lists no member of another site: this node has nothing to link to. Members of one site do not link to each other.'),
        // Why a tunnel is not up, and what to look at: the node's words.
        notes.length ? h('div.notices', { style: { padding: '12px 16px 14px' } },
          notes.map((p) => notice({ level: 'warning', message: `${peerName(p)} (site ${p.site}): ${p.note}.` })),
          h('p.dim', { style: { fontSize: '13px', margin: '2px 0 0' } }, 'A peer stays here for as long as the profile lists it. To take one off, remove it under Members below and send the change.')) : null]
      : [
          // A node that is no member still says how the link stands.
          sum ? h('div', { style: { marginTop: '14px' } }, notice({ level: { ok: 'ok', warn: 'warning', bad: 'error' }[sum.tone], message: `Site link: ${sum.text}. ${sum.note}` })) : null,
          h('p.dim', { style: { margin: '12px 0 0', fontSize: '14px', maxWidth: '70ch' } },
            'Parts of this mesh in other places, each with internet, join into one mesh over the internet. One or two nodes with an uplink at each site hold the link: list them below, with their keys.')]);
}

// clean is a member as the profile holds it: empty fields left out.
const clean = (m) => Object.fromEntries(Object.entries(m).filter(([, v]) => v !== '' && v != null));
const listAnd = (xs) => (xs.length < 2 ? xs.join('') : xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1]);

// notSent says what a draft changes in the list the nodes have. A member
// with the same name and site under another key was given a new key.
function notSent(was, now) {
  const name = (m) => m.name || short(m.public_key) || 'a member with no key yet';
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
    rekeyed.length ? `lists ${listAnd(rekeyed.map(name))} under ${rekeyed.length === 1 ? 'its' : 'their'} new key` : '']
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
  const tunnels = (r) => (!r.reported ? '' : r.peers === 0 ? ' · no other site to link to' : ` · ${r.up} of ${r.peers} tunnel${r.peers === 1 ? '' : 's'} up`);
  const keyHint = (m) => {
    if (!m.public_key) return '';
    const r = roster.get(m.public_key);
    if (m.public_key === mine) return 'this node' + (r ? tunnels(r) : '');
    if (r && r.heard) return `${r.callsign}` + (tunnels(r) || ' · heard, its tunnels not reported');
    if (r) return 'not heard from here: its site is not joined, or it is off';
    return holder.has(m.public_key) ? `${holder.get(m.public_key)}, a node this one hears` : '';
  };
  const words = changed ? notSent(was, ms) : '';
  const secs = pending ? Math.max(0, Math.round((Date.parse(pending.apply_at) - Date.now()) / 1000)) : 0;
  const have = pending ? (pending.nodes || []).filter((n) => n.state === 'has it').length : 0;

  return panel('Members', { meta: 'the same on every node' },
    h('div.form',
      pending ? notice({ level: 'warning', message: changed && sent
        ? `Sent${pending.sender && (pending.nodes || []).length ? `: ${have} of ${pending.nodes.length} other nodes have it` : ''}. Every node switches to this list at ${clock(pending.apply_at)} (in ${secs} s); until then they keep the one they have.`
        : `A change to the profile from ${pending.sender ? 'this node' : pending.from} is on its way: every node switches at ${clock(pending.apply_at)} (in ${secs} s). The members can be changed after that.${changed ? ' The edits below have not been sent.' : ''}` }) : null,
      h('datalist', { id: 'site-names' }, sites.map((s) => h('option', { value: s }))),
      ms.length ? null : h('p.dim', { style: { margin: '0' } }, was.length ? 'No members: sending this takes the site link down on every node.' : 'No members yet: the site link is off.'),
      ms.map((m, i) => h('div.fields.sitemember',
        field('Name', input(m, 'name', i, { placeholder: 'label', max: '32' }), { error: err(i, 'name') }),
        field('Site', input(m, 'site', i, { placeholder: 'hq', max: '32', list: 'site-names' }), { error: err(i, 'site') }),
        field('Public key', input(m, 'public_key', i, { placeholder: 'from meshctl site, or this page on that node', mono: true, max: '44' }),
          { error: err(i, 'public_key'), hint: keyHint(m) }),
        field('Endpoint', input(m, 'endpoint', i, { placeholder: 'host:51873', mono: true, max: '260' }),
          { error: err(i, 'endpoint'), hint: m.endpoint ? '' : 'none: it dials out (behind NAT)' }),
        h('div.field', h('span.label', { 'aria-hidden': 'true' }, ' '),
          h('div', button('Remove', () => { ms.splice(i, 1); ui.fields = {}; redraw(); }, 'small', { disabled: locked, title: `Take ${m.name || 'this member'} off the list` }))))),
      h('div.row', { style: { flexWrap: 'wrap' } },
        !listed && mine ? button('Add this node', () => { ms.push({ name: st.node.callsign, site: sites[0] || '', public_key: mine, endpoint: '' }); redraw(); }, 'small', { disabled: locked }) : null,
        addable.length ? h('select.select', { value: '', disabled: locked || ms.length >= 64, 'aria-label': 'Add a node this one hears', style: { width: 'auto' },
          onchange: (e) => {
            const n = addable.find((x) => x.site_key === e.target.value);
            e.target.value = '';
            if (n) { ms.push({ name: n.callsign, site: mySite || (sites.length === 1 ? sites[0] : ''), public_key: n.site_key, endpoint: '' }); redraw(); }
          } },
          h('option', { value: '' }, 'Add a node this one hears…'),
          addable.map((n) => h('option', { value: n.site_key }, n.callsign))) : null,
        button('Add by key', () => { ms.push({ name: '', site: '', public_key: '', endpoint: '' }); redraw(); }, 'small', { disabled: locked || ms.length >= 64, title: 'A node at another site, not reachable yet: paste the key its own page shows' }),
        was.length && ms.length ? button('Remove all', () => { ms.length = 0; ui.fields = {}; redraw(); }, 'small', { disabled: locked, title: 'Empty the list: sent, it takes the site link down on every node' }) : null),
      h('p.dim', { style: { margin: '0', fontSize: '13px', maxWidth: '75ch' } }, 'Each member links to the members of the other sites, not to those of its own. An endpoint is the member’s public address or name and the UDP port forwarded to it; at least one end of each link needs one.'),
      ms.length && sites.length < 2 ? notice({ level: 'info', message: 'Members of one site do not link to each other: a link needs members at two sites at least.' }) : null,
      h('div.row.between', { style: { flexWrap: 'wrap', gap: '12px' } },
        h('span.dim', { style: { fontSize: '14px', maxWidth: '70ch' } }, changed && !sent ? `Not sent yet: this ${words}. The nodes keep the list they have until you send it.` : ''),
        h('div.row',
          changed && !sent ? button('Discard', () => { ui.draft = null; redraw(); }, '', { disabled: ui.busy, title: 'Drop these edits: back to the list the nodes have' }) : null,
          button(ui.busy ? 'Checking…' : pending ? 'Waiting for the switch…' : others.length ? 'Review and send to every node' : 'Review and save', send, 'primary', { disabled: !changed || locked })))),
    h('p.dim', { style: { marginTop: '14px', fontSize: '13px', maxWidth: '75ch' } },
      'Sent from here, the list reaches the nodes this one can reach: before the link is up, only this site. At each other site, install the same profile on one node: here Settings → Mesh profile → Share with another node → Download file, there Settings → Mesh profile → Install another profile with the file’s contents. The rest of that site takes it from that node. Taking members off works the same way, while the link is still up or at each site.'));
}

export function siteLink(st) {
  if (!st.site_link) return null;
  return [statusPanel(st), membersPanel(st)];
}
