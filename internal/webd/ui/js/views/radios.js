// Radios: every radio with its facts and where each fact comes from, and
// which radio does what (architecture §5.2).
import { h } from '../dom.js';
import { state, redraw } from '../state.js';
import { panel, pill, kv, field, toggle, button, notice } from '../ui.js';
import { bandName, roleName, roleKind, roleLink, meshLinks, linkName, linkAt, fewerRadios } from '../fmt.js';
import { ensureConfig, saveBar, identify, cfg } from '../config.js';

const SOURCES = { measured: 'measured', database: 'hardware database', driver: 'reported by the driver', config: 'set in this node’s config' };

// describe names a radio for a person: its label, or its chipset with the
// bands, since a dual-band card shows up as two radios of one chipset.
function describe(r) {
  if (r.label) return r.label;
  const bands = (r.bands || []).map((b) => bandName(b).replace(' GHz', '')).join('/');
  return (r.chipset || r.driver) + (bands ? ` · ${bands}${bands === 'HaLow' ? '' : ' GHz'}` : '');
}

// roleValue and setRole translate between a role's config form and one
// string for a <select>.
function roleValue(sel) {
  if (sel && typeof sel === 'object') return sel.radio ? 'radio:' + sel.radio : 'port:' + sel.port;
  return sel || 'auto';
}
function parseRole(v) {
  if (v.startsWith('radio:')) return { radio: v.slice(6) };
  if (v.startsWith('port:')) return { port: v.slice(5) };
  return v;
}

// roleSelect is the choice of radio for one role. now is the radio that has
// the role at present; can says which radios could take it.
function roleSelect(title, key, st, draft, hint, can, now) {
  const cur = roleValue(draft.radios.roles[key]);
  const able = st.radios.filter(can);
  // A card with two radios sits in one port: offer the port once.
  const ports = [...new Set(able.map((r) => r.port))];
  const options = [
    h('option', { value: 'auto' }, 'Automatic' + (now ? ` (now: ${describe(now)})` : ' (now: no radio)')),
    ...able.map((r) => h('option', { value: 'radio:' + r.fingerprint }, `Always ${describe(r)} (${r.phy})`)),
    ...ports.map((p) => h('option', { value: 'port:' + p }, `Whatever is in ${p}`)),
    h('option', { value: 'disabled' }, 'Off'),
  ];
  // A pin to a radio that is not plugged in still has to be shown.
  if (cur !== 'auto' && cur !== 'disabled' && !options.some((o) => o.value === cur)) {
    options.splice(1, 0, h('option', { value: cur }, `${cur.replace(':', ' ')} (not present)`));
  }
  return field(title, h('select.select', { value: cur, onchange: (e) => { draft.radios.roles[key] = parseRole(e.target.value); redraw(); } }, options),
    { hint, error: cfg().fields['radios.roles.' + key] });
}

// rule finds (or with make, creates) the per-radio rule for a radio.
// Looking one up leaves the draft alone: this runs on every draw.
function rule(draft, r, make) {
  const devs = draft.radios.devices || [];
  let d = devs.find((x) => x.match.fingerprint === r.fingerprint) || devs.find((x) => x.match.port === r.port);
  if (!d && make) {
    d = { match: { fingerprint: r.fingerprint, port: '' }, ignore: false, label: '', max_power_dbm: null };
    (draft.radios.devices ||= []).push(d);
  }
  return d;
}
function prune(draft) {
  draft.radios.devices = (draft.radios.devices || []).filter((d) => d.ignore || d.label || d.max_power_dbm != null);
}

function radioCard(r, st, draft) {
  const d = rule(draft, r, false) || {};
  const set = (fn) => { fn(rule(draft, r, true)); prune(draft); redraw(); };
  const differs = r.recommended && r.recommended !== r.role;
  return panel('', { class: 'radio-card' },
    h('div.row.between', { style: { alignItems: 'flex-start' } },
      h('div',
        h('div.title', describe(r)),
        h('div.faint.mono', { style: { fontSize: '12.5px', marginTop: '3px' } }, [r.label ? r.chipset : null, r.phy, r.driver, r.port].filter(Boolean).join(' · '))),
      h('div.row',
        // What the radio does, and on which link, as on the overview.
        linkAt(st, roleLink(r.role)) ? h('span.mono.dim', { style: { fontSize: '12.5px' } }, linkName(linkAt(st, roleLink(r.role)), st)) : null,
        pill(r.role === 'unused' ? 'no role' : roleKind(r.role), r.role === 'unused' ? 'plain' : 'solid'),
        r.ignored ? pill('ignored', 'warn') : null,
        r.hard_blocked ? pill('rf-kill switch', 'bad') : null)),
    r.broken ? h('div', { style: { marginTop: '12px' } }, notice({ level: 'warning', message: r.broken })) : null,
    r.in_use ? h('div', { style: { marginTop: '12px' } }, notice({ level: 'info', message: 'The system uses this radio: ' + r.in_use })) : null,
    differs ? h('div', { style: { marginTop: '12px' } }, notice({ level: 'info', message: `On the next start this radio would take this role: ${roleName(r.recommended, st)}. Roles do not move while the node runs, so a link never drops by surprise; pin a role above to move it now.` })) : null,
    h('div.facts',
      h('div.fact', h('span.label', 'Bands'), h('b', (r.bands || []).map(bandName).join(', ') || '–'),
        (r.bands || []).length > 1 ? h('span.faint', { style: { fontSize: '12.5px' } }, 'one at a time') : null),
      // Spatial streams, not antennas: "2×2" would claim as many transmit as
      // receive chains, and some radios have 2 transmit and 3 receive.
      h('div.fact', h('span.label', 'Spatial streams'), h('b', r.streams ? String(r.streams) : '–')),
      h('div.fact', h('span.label', 'Can do'), h('div.caps', { style: { marginTop: '3px' } },
        pill('mesh', r.mesh_point ? 'ok' : 'plain'), pill('Wi-Fi AP', r.ap ? 'ok' : 'plain'), pill('both at once', r.mesh_and_ap ? 'ok' : 'plain'))),
      h('div.fact', h('span.label', 'Bus'), h('b', r.bus + (r.usb_speed_mbps ? ` · ${r.usb_speed_mbps >= 5000 ? 'USB 3' : 'USB 2'}` : '') + (r.device_id ? ` · ${r.device_id}` : ''))),
      Object.entries(r.power || {}).map(([band, p]) => h('div.fact', h('span.label', `Max power, ${bandName(band)}`),
        h('b', `${p.dbm} dBm`), h('span.faint', { style: { fontSize: '12.5px' } }, SOURCES[p.source] || p.source))),
      h('div.fact', h('span.label', 'Identity'), h('b', { style: { fontSize: '12.5px', overflowWrap: 'anywhere' } }, r.fingerprint), h('span.faint', { style: { fontSize: '12.5px' } }, 'by ' + (r.fingerprint_source || '').replace('-', ' ')))),
    (r.quirks || []).length ? h('details', { style: { marginTop: '14px' } }, h('summary', `${r.quirks.length} thing${r.quirks.length === 1 ? '' : 's'} known about this adapter`), h('ul.quirks', r.quirks.map((q) => h('li', q)))) : null,
    h('div.fields', { style: { marginTop: '16px', paddingTop: '16px', borderTop: '1px solid var(--line)' } },
      field('Label', h('input.input', { value: d.label || '', placeholder: r.chipset || 'e.g. roof antenna', maxlength: '60', oninput: (e) => set((x) => { x.label = e.target.value; }) })),
      field('Max power for ranking, dBm', h('input.input', { type: 'number', min: '0', max: '40', value: d.max_power_dbm ?? '', placeholder: 'from the database',
        oninput: (e) => set((x) => { x.max_power_dbm = e.target.value === '' ? null : Number(e.target.value); }) }),
        { hint: 'Only used to choose the mesh radio. It does not limit transmit power.' })),
    h('div.row.between', { style: { marginTop: '14px' } },
      toggle('Ignore this radio', 'Trellis leaves it alone', !!d.ignore, (v) => set((x) => { x.ignore = v; })),
      r.led ? button('Identify', () => identify(r.fingerprint), 'small', { icon: 'blink', title: 'Blink this radio’s LED for 10 seconds' })
        : h('span.faint', { style: { fontSize: '13px' } }, 'no LED to blink')));
}

// linkSelect is the choice of radio for mesh link n (from 1), named by its
// band and channel: only radios that have the link's band, and a hint that
// says where the link is now, or why this node has no radio on it.
function linkSelect(l, n, st, draft) {
  const band = l.band || st.profile.band;
  const can = (r) => r.mesh_point && !r.ignored && (r.bands || []).includes(band);
  const now = st.radios.find((r) => roleLink(r.role) === n);
  const able = st.radios.filter(can);
  let hint;
  if (now) hint = `On ${describe(now)}. Moving it drops the link for a few seconds.`;
  else if (!able.length) hint = `No radio on this node has ${bandName(band)}: it meshes over its other links.`;
  else {
    const busy = able.map((r) => `${describe(r)} carries ${roleLink(r.role) ? linkName(linkAt(st, roleLink(r.role)), st) : 'no link'}`).join('; ');
    hint = `No radio carries it here: ${busy}. A radio is on one link at a time; choose it here to move it to this one.`;
  }
  return roleSelect(`Mesh on ${linkName(l, st)}`, n === 1 ? 'mesh' : 'mesh' + n, st, draft, hint, can, now);
}

export function radios() {
  const st = state.status;
  const c = ensureConfig();
  const links = meshLinks(st);
  const head = h('header.page-head', h('div', h('h1', 'Radios'),
    h('p.sub', 'The radio with the most transmit power carries the mesh; the weakest serves the node’s own Wi-Fi, which only has to reach its owner’s phone. Radios are told apart by a fingerprint, never by the name the kernel gives them.')));
  const meshRadioCount = st.radios.filter((r) => r.mesh_point && !r.ignored).length;
  // A node that cannot carry every link: which one it is on, and how to
  // choose another, before the choices themselves.
  const few = fewerRadios(st);
  const switchHow = few && few.on.length && few.off.length
    ? `To use ${linkName(few.off[0], st)} instead, set “Mesh on ${linkName(few.on[0], st)}” to Off below and leave ${linkName(few.off[0], st)} on Automatic (or choose the radio for it). ` +
      'Left on Automatic, a node like this one takes the links in the profile’s order (Settings → Mesh profile), which every node of the mesh shares.'
    : '';
  if (!c.draft) return h('div.page', head, h('div.empty', c.error || 'Loading the configuration…'));
  const draft = c.draft;
  return h('div.page', head,
    (st.plan.notices || []).filter((n) => n.level !== 'info').length
      ? h('div.notices', st.plan.notices.filter((n) => n.level !== 'info').map((n) => notice(n))) : null,
    panel('Which radio does what', {},
      few ? h('div', { style: { marginBottom: '14px' } }, notice({ level: 'info', message: few.lead }, switchHow ? h('div', { style: { marginTop: '6px' } }, switchHow) : null)) : null,
      links.length > 1 ? h('p.dim', { style: { fontSize: '14px', maxWidth: '85ch', marginBottom: '14px' } },
        `The mesh profile has ${links.length} links, each with its band and channel (Settings → Mesh profile). Here each link gets one of this node’s radios. `,
        'A radio carries one link at a time, a dual-band radio too: it is on one channel. ',
        meshRadioCount < links.length ? '' : 'A card with two radios, one per band, carries two links at once.') : null,
      h('div.fields',
        links.map((l, i) => linkSelect(l, i + 1, st, draft)),
        roleSelect('Client Wi-Fi', 'ap', st, draft, 'Shares a mesh radio when no other radio is free.', (r) => r.ap && !r.ignored, st.radios.find((r) => (r.role || '').split('+').includes('ap') || r.role === 'ap')))),
    st.radios.length ? st.radios.map((r) => radioCard(r, st, draft)) : h('div.empty', 'No Wi-Fi radios found on this node.'),
    saveBar());
}
