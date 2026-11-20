// Radios: every radio with its facts and where each fact comes from, and
// which radio does what (architecture §5.2).
import { h } from '../dom.js';
import { state, redraw } from '../state.js';
import { panel, pill, kv, field, toggle, button, notice } from '../ui.js';
import { bandName, roleName, roleKind, roleLink, meshLinks, linkName, linkAt, fewerRadios } from '../fmt.js';
import { ensureConfig, saveBar, identify, cfg } from '../config.js';

const SOURCES = { measured: 'measured', database: 'hardware database', driver: 'reported by the driver', config: 'node config' };

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
    ...ports.map((p) => h('option', { value: 'port:' + p }, `Any radio in ${p}`)),
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

// pcieNote is the state of a PCIe radio's link in a few words: what
// sampling its error status found, else the kernel's counters.
export function pcieNote(p) {
  if (p.health === 'marginal') return 'Marginal · risk of card hang';
  if (p.health === 'ok') return 'No errors';
  if (p.counted) return `AER: ${p.correctable} corrected, ${p.nonfatal + p.fatal} uncorrected`;
  return 'Errors not sampled';
}

// pcieLink is the trained link and, when the card and slot support more,
// the most they do: "Gen1 x1 (max Gen2 x1)".
export function pcieLink(p) {
  if (!p.gen) return '–';
  const max = p.max_gen > p.gen || p.max_width > p.width ? ` (max Gen${Math.max(p.max_gen, p.gen)} x${Math.max(p.max_width || 0, p.width)})` : '';
  return `Gen${p.gen} x${p.width}${max}`;
}

function radioCard(r, st, draft) {
  const d = rule(draft, r, false) || {};
  // What the width of the link this radio carries costs on it.
  const cost = (linkAt(st, roleLink(r.role)) || {}).width_cost;
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
    r.in_use ? h('div', { style: { marginTop: '12px' } }, notice({ level: 'info', message: 'In use by the system: ' + r.in_use })) : null,
    differs ? h('div', { style: { marginTop: '12px' } }, notice({ level: 'info', message: `Recommended role at next start: ${roleName(r.recommended, st)}. Roles do not change at runtime; pin a role to move it now.` })) : null,
    h('div.facts',
      h('div.fact', h('span.label', 'Bands'), h('b', (r.bands || []).map(bandName).join(', ') || '–'),
        (r.bands || []).length > 1 ? h('span.faint', { style: { fontSize: '12.5px' } }, 'one at a time') : null),
      // Spatial streams, not antennas: "2×2" would claim as many transmit as
      // receive chains, and some radios have 2 transmit and 3 receive.
      h('div.fact', h('span.label', 'Spatial streams'), h('b', r.streams ? String(r.streams) : '–')),
      h('div.fact', h('span.label', 'Capabilities'), h('div.caps', { style: { marginTop: '3px' } },
        pill('mesh', r.mesh_point ? 'ok' : 'plain'), pill('Wi-Fi AP', r.ap ? 'ok' : 'plain'), pill('mesh + AP', r.mesh_and_ap ? 'ok' : 'plain'))),
      h('div.fact', h('span.label', 'Bus'), h('b', r.bus + (r.usb_speed_mbps ? ` · ${r.usb_speed_mbps >= 5000 ? 'USB 3' : 'USB 2'}` : '') + (r.device_id ? ` · ${r.device_id}` : ''))),
      Object.entries(r.power || {}).map(([band, p]) => h('div.fact', h('span.label', `Max power, ${bandName(band)}`),
        h('b', `${p.dbm} dBm`), h('span.faint', { style: { fontSize: '12.5px' } }, SOURCES[p.source] || p.source))),
      cost ? h('div.fact', h('span.label', `Link budget at ${cost.width_mhz} MHz`), h('b', `−${cost.total_db} dB`),
        h('span.faint', { style: { fontSize: '12.5px' } }, cost.power_known
          ? `vs ${cost.base_mhz} MHz: ${cost.power_dbm} dBm (vs ${cost.base_power_dbm}), +${cost.noise_db} dB noise`
          : `vs ${cost.base_mhz} MHz: wider-channel noise`)) : null,
      r.thermal ? h('div.fact', h('span.label', 'Temperature'), h('b', { class: r.thermal.throttling ? 'warn' : '' }, `${Math.round(r.thermal.temp_c)} °C`),
        h('span.faint', { style: { fontSize: '12.5px' } }, r.thermal.throttling ? `Throttling · Tx duty ${r.thermal.duty_pct}%`
          : r.thermal.throttle_at_c ? `Throttle at ${Math.round(r.thermal.throttle_at_c)} °C` : '')) : null,
      r.pcie ? h('div.fact', h('span.label', 'PCIe link'),
        h('b', { class: r.pcie.health === 'marginal' ? 'warn' : '' }, pcieLink(r.pcie)),
        h('span.faint', { style: { fontSize: '12.5px' } }, pcieNote(r.pcie))) : null,
      h('div.fact', h('span.label', 'Identity'), h('b', { style: { fontSize: '12.5px', overflowWrap: 'anywhere' } }, r.fingerprint), h('span.faint', { style: { fontSize: '12.5px' } }, 'by ' + (r.fingerprint_source || '').replace('-', ' ')))),
    (r.quirks || []).length ? h('details', { style: { marginTop: '14px' } }, h('summary', `Known issues (${r.quirks.length})`), h('ul.quirks', r.quirks.map((q) => h('li', q)))) : null,
    h('div.fields', { style: { marginTop: '16px', paddingTop: '16px', borderTop: '1px solid var(--line)' } },
      field('Label', h('input.input', { value: d.label || '', placeholder: r.chipset || 'e.g. roof antenna', maxlength: '60', oninput: (e) => set((x) => { x.label = e.target.value; }) })),
      field('Max power for ranking (dBm)', h('input.input', { type: 'number', min: '0', max: '40', value: d.max_power_dbm ?? '', placeholder: 'from the database',
        oninput: (e) => set((x) => { x.max_power_dbm = e.target.value === '' ? null : Number(e.target.value); }) }),
        { hint: 'Used for mesh radio selection only. Does not limit Tx power.' })),
    h('div.row.between', { style: { marginTop: '14px' } },
      toggle('Ignore this radio', 'Not managed by Trellis', !!d.ignore, (v) => set((x) => { x.ignore = v; })),
      r.led ? button('Identify', () => identify(r.fingerprint), 'small', { icon: 'blink', title: 'Blink LED (10 s)' })
        : h('span.faint', { style: { fontSize: '13px' } }, 'no LED')));
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
  else if (!able.length) hint = `No ${bandName(band)} radio on this node; meshing on other links.`;
  else {
    const busy = able.map((r) => `${describe(r)} carries ${roleLink(r.role) ? linkName(linkAt(st, roleLink(r.role)), st) : 'no link'}`).join('; ');
    hint = `No radio assigned: ${busy}. One link per radio; select a radio to move it here.`;
  }
  return roleSelect(`Mesh on ${linkName(l, st)}`, n === 1 ? 'mesh' : 'mesh' + n, st, draft, hint, can, now);
}

export function radios() {
  const st = state.status;
  const c = ensureConfig();
  const links = meshLinks(st);
  const head = h('header.page-head', h('div', h('h1', 'Radios'),
    h('p.sub', 'Automatic: highest-power radio for the mesh, lowest for the client AP. Radios identified by fingerprint, not kernel name.')));
  const meshRadioCount = st.radios.filter((r) => r.mesh_point && !r.ignored).length;
  // A node that cannot carry every link: which one it is on, and how to
  // choose another, before the choices themselves.
  const few = fewerRadios(st);
  const switchHow = few && few.on.length && few.off.length
    ? `To use ${linkName(few.off[0], st)} instead: set “Mesh on ${linkName(few.on[0], st)}” to Off and leave ${linkName(few.off[0], st)} on Automatic (or select its radio). ` +
      'On Automatic, links are assigned in mesh profile order (Settings → Mesh profile).'
    : '';
  if (!c.draft) return h('div.page', head, h('div.empty', c.error || 'Loading configuration…'));
  const draft = c.draft;
  return h('div.page', head,
    (st.plan.notices || []).filter((n) => n.level !== 'info').length
      ? h('div.notices', st.plan.notices.filter((n) => n.level !== 'info').map((n) => notice(n))) : null,
    panel('Role assignment', {},
      few ? h('div', { style: { marginBottom: '14px' } }, notice({ level: 'info', message: few.lead }, switchHow ? h('div', { style: { marginTop: '6px' } }, switchHow) : null)) : null,
      links.length > 1 ? h('p.dim', { style: { fontSize: '14px', maxWidth: '85ch', marginBottom: '14px' } },
        `Mesh profile: ${links.length} links (Settings → Mesh profile). Assign a radio to each. `,
        'One link per radio, including dual-band radios (one channel at a time). ',
        meshRadioCount < links.length ? '' : 'A card with two radios carries two links.') : null,
      h('div.fields',
        links.map((l, i) => linkSelect(l, i + 1, st, draft)),
        roleSelect('Client AP', 'ap', st, draft, 'Shares a mesh radio when no radio is free.', (r) => r.ap && !r.ignored, st.radios.find((r) => (r.role || '').split('+').includes('ap') || r.role === 'ap')))),
    st.radios.length ? st.radios.map((r) => radioCard(r, st, draft)) : h('div.empty', 'No Wi-Fi radios detected.'),
    saveBar());
}
