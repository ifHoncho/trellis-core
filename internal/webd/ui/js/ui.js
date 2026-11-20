// Building blocks shared by the views.
import { h } from './dom.js';
import { signal } from './fmt.js';

const ICONS = {
  overview: 'M12 3l9 9-9 9-9-9zM7.5 7.5l9 9M16.5 7.5l-9 9',
  mesh: 'M12 7.2v0M5.5 17.5l5.2-8.4M18.5 17.5l-5.2-8.4M7.6 18.5h8.8M12 4.5a2 2 0 100 4 2 2 0 000-4zM5 16.5a2 2 0 100 4 2 2 0 000-4zM19 16.5a2 2 0 100 4 2 2 0 000-4z',
  internet: 'M12 3a9 9 0 100 18 9 9 0 000-18zM3 12h18M12 3c2.6 2.4 4 5.6 4 9s-1.4 6.6-4 9c-2.6-2.4-4-5.6-4-9s1.4-6.6 4-9z',
  devices: 'M8 2.5h8a1.5 1.5 0 011.5 1.5v16a1.5 1.5 0 01-1.5 1.5H8A1.5 1.5 0 016.5 20V4A1.5 1.5 0 018 2.5zM10.5 18.5h3',
  radios: 'M12 21V11M12 11a1.6 1.6 0 100-3.2 1.6 1.6 0 000 3.2zM7.8 13.6a6 6 0 010-8.4M16.2 5.2a6 6 0 010 8.4M4.9 16.5a10 10 0 010-14.2M19.1 2.3a10 10 0 010 14.2',
  multicast: 'M5 19a1.5 1.5 0 100-3 1.5 1.5 0 000 3zM5 11.5a6 6 0 016 6M5 6.5a11 11 0 0111 11M5 1.8a15.7 15.7 0 0115.7 15.7',
  settings: 'M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1M15 4v4M9 10v4M17 16v4',
  alerts: 'M12 3.5l9.5 16.5h-19zM12 10v4.5M12 17.4v.2',
  dusk: 'M20 14.5A8.5 8.5 0 019.5 4a8.5 8.5 0 1010.5 10.5z',
  day: 'M12 7.5a4.5 4.5 0 100 9 4.5 4.5 0 000-9zM12 2v2.5M12 19.5V22M2 12h2.5M19.5 12H22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8',
  night: 'M2.5 12s3.5-6.5 9.5-6.5S21.5 12 21.5 12s-3.5 6.5-9.5 6.5S2.5 12 2.5 12zM12 9.2a2.8 2.8 0 100 5.6 2.8 2.8 0 000-5.6z',
  out: 'M14 4h4.5A1.5 1.5 0 0120 5.5v13a1.5 1.5 0 01-1.5 1.5H14M10 16l-4-4 4-4M6 12h10',
  copy: 'M9 9h10v11H9zM5 15V4h10',
  eye: 'M2.5 12s3.5-6.5 9.5-6.5S21.5 12 21.5 12s-3.5 6.5-9.5 6.5S2.5 12 2.5 12zM12 9.2a2.8 2.8 0 100 5.6 2.8 2.8 0 000-5.6z',
  download: 'M12 4v11M7.5 11l4.5 4.5 4.5-4.5M5 19.5h14',
  camera: 'M4 8h3l1.5-2.5h7L17 8h3v11H4zM12 10.5a3 3 0 100 6 3 3 0 000-6z',
  blink: 'M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1M12 9.5a2.5 2.5 0 100 5 2.5 2.5 0 000-5z',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  refresh: 'M19.5 12a7.5 7.5 0 11-2.2-5.3M19.5 4v4h-4',
  voice: 'M5 13v-1a7 7 0 0114 0v1M5 13h2.5v6H5a1 1 0 01-1-1v-4a1 1 0 011-1zM19 13h-2.5v6H19a1 1 0 001-1v-4a1 1 0 00-1-1zM16.5 19c0 1.5-1.8 2.5-4.5 2.5',
};

export function icon(name, cls = 'ico') {
  return h('svg', { class: cls, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.6', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' },
    h('path', { d: ICONS[name] || '' }));
}

// The mark: a lattice with a node at its centre and two on the vine.
export function brandMark() {
  return h('svg', { class: 'brand-mark', viewBox: '0 0 40 40', fill: 'none', stroke: 'currentColor', 'aria-hidden': 'true' },
    h('path', { d: 'M20 2.5 37.5 20 20 37.5 2.5 20Z', 'stroke-width': '2', 'stroke-linejoin': 'round' }),
    h('path', { d: 'M11.25 11.25 28.75 28.75M28.75 11.25 11.25 28.75', 'stroke-width': '1', opacity: '0.55' }),
    h('circle', { cx: '20', cy: '20', r: '3.4', fill: 'currentColor', stroke: 'none' }),
    h('circle', { cx: '28.75', cy: '11.25', r: '2.1', fill: 'currentColor', stroke: 'none' }),
    h('circle', { cx: '11.25', cy: '28.75', r: '2.1', fill: 'currentColor', stroke: 'none' }));
}

export function panel(title, opts, ...body) {
  if (opts instanceof Node || Array.isArray(opts) || typeof opts !== 'object' || opts === null) {
    body.unshift(opts);
    opts = {};
  }
  return h('section.panel', { class: opts.class || '' },
    (title || opts.meta || opts.tools) && h('header.panel-head',
      h('h2.label', title || ''),
      opts.tools ? h('div.row', opts.tools) : opts.meta ? h('div.meta', opts.meta) : null),
    h('div.panel-body', { class: opts.flush ? 'flush' : '' }, body),
    opts.foot && h('footer.panel-foot', opts.foot));
}

export function stat(label, value, unit, opts = {}) {
  return h('div.stat',
    h('span.label', label),
    h('span.value', { class: [opts.text ? 'text' : '', opts.tone || ''].join(' ') }, value, unit ? h('small', unit) : null),
    opts.note ? h('span.note', opts.note) : null);
}

export const dot = (tone = 'ok') => h('i.dot', { class: tone });
export const pill = (text, tone = 'plain') => h('span.pill', { class: tone }, text);

export function bars(dbm) {
  const s = signal(dbm);
  return h('span.bars', { class: s.tone, title: dbm ? `${dbm} dBm, ${s.word}` : 'signal unknown' },
    [1, 2, 3, 4].map((i) => h('i', { class: i <= s.bars ? 'on' : '' })));
}

// spark draws a history as a line over a faint area. Values share one
// scale that starts at zero, so a flat high line and a flat low line look
// different.
export function spark(values, opts = {}) {
  const w = 100, hgt = 34;
  if (!values || values.length < 2) return h('svg.spark', { viewBox: `0 0 ${w} ${hgt}`, preserveAspectRatio: 'none' });
  const max = Math.max(opts.max || 0, ...values) || 1;
  const step = w / (values.length - 1);
  const pts = values.map((v, i) => `${(i * step).toFixed(2)} ${(hgt - 2 - (Math.max(0, v) / max) * (hgt - 6)).toFixed(2)}`);
  return h('svg.spark', { viewBox: `0 0 ${w} ${hgt}`, preserveAspectRatio: 'none' },
    h('path.area', { d: `M0 ${hgt} L${pts.join(' L')} L${w} ${hgt} Z` }),
    h('path.line', { d: `M${pts.join(' L')}` }));
}

// qr draws a QR symbol from the API's rows, always dark on light with a
// quiet zone: scanners need it that way whatever the theme.
export function qr(code, label) {
  if (!code || !code.rows) return h('div.qr');
  const quiet = 3, n = code.size + quiet * 2;
  let d = '';
  code.rows.forEach((row, y) => {
    for (let x = 0; x < row.length; x++) {
      if (row[x] !== '1') continue;
      let run = 1;
      while (x + run < row.length && row[x + run] === '1') run++;
      d += `M${x + quiet} ${y + quiet}h${run}v1h-${run}z`;
      x += run - 1;
    }
  });
  return h('div.qr', { role: 'img', 'aria-label': label || 'QR code' },
    h('svg', { viewBox: `0 0 ${n} ${n}`, 'shape-rendering': 'crispEdges' }, h('path', { d })));
}

export function kv(pairs) {
  return h('dl.kv', pairs.filter(Boolean).flatMap(([k, v]) => [h('dt', k), h('dd', v)]));
}

// table({stack: true}, [{title, cls, cell(row)}], rows, emptyText)
export function table(opts, cols, rows, empty) {
  if (!rows.length) return h('div.empty', empty || 'None');
  return h('table.table', { class: opts.stack ? 'stack' : '' },
    h('thead', h('tr', cols.map((c) => h('th', { class: c.cls || '' }, c.title)))),
    h('tbody', rows.map((r) => h('tr', { class: opts.rowClass ? opts.rowClass(r) : '', key: opts.key ? opts.key(r) : null },
      cols.map((c) => h('td', { class: c.cls || '', 'data-label': c.wide ? null : c.title }, c.cell(r)))))));
}

// asSentence starts a message with a capital and ends it with a stop: the
// node writes some the way Go writes errors, in lower case.
// A name at the start ("scout-7 runs…", "mt7915e-power 2 was…") keeps its
// case: only a plain word is capitalised.
export function asSentence(m) {
  if (!m) return m;
  if (/^[a-z]+(\s|$)/.test(m)) m = m[0].toUpperCase() + m.slice(1);
  return /[.!?)]$/.test(m) ? m : m + '.';
}

export function notice(w, extra) {
  const tone = { error: 'bad', warning: 'warn', info: 'info', ok: 'ok' }[w.level] || 'info';
  return h('div.notice', { class: tone },
    dot(tone),
    h('div', w.code ? h('span.code', [w.source, w.code].filter(Boolean).join(' · ')) : null, asSentence(w.message), extra || null));
}

export function field(label, control, opts = {}) {
  return h('label.field',
    h('span.label', label),
    control,
    opts.error ? h('span.error', opts.error) : null,
    opts.hint ? h('span.hint', opts.hint) : null);
}

export function toggle(label, hint, checked, onchange, opts = {}) {
  return h('label.check',
    h('input', { type: 'checkbox', checked, onchange: (e) => onchange(e.target.checked), disabled: opts.disabled }),
    h('span.text', h('span', label), hint ? h('small', hint) : null));
}

export function button(label, onclick, kind = '', opts = {}) {
  return h('button.btn', { class: kind, type: opts.type || 'button', onclick, disabled: opts.disabled, title: opts.title },
    opts.icon ? icon(opts.icon) : null, label);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (e) {
    // Clipboard access needs a secure context; fall back to a selection.
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (err) { ok = false; }
    ta.remove();
    return ok;
  }
}

export function download(filename, text, type = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
