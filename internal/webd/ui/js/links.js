// Mesh links in forms: the band, channel and width of each, for the profile
// editor and the setup wizard. What a band allows comes from the node
// (GET /channels); the node asks for nothing narrower than the hardware and
// the software can do, and reports at run time what a radio or the
// regulatory domain refuses.
import { h } from './dom.js';
import { get } from './api.js';
import { state, redraw } from './state.js';
import { field } from './ui.js';
import { bandName } from './fmt.js';

// Suggestions only: channels most radios have, and where the automatic
// choices go first. Any other channel may be typed.
export const COMMON = { '2g': [1, 6, 11], '5g': [36, 40, 44, 48, 149, 153, 157, 161, 165], '6g': [1, 5, 9, 37, 53, 69, 85] };
const DEFAULT_CHANNEL = { '2g': 6, '5g': 149, '6g': 37, s1g: 26 };
export const DEFAULT_WIDTH = { '2g': 20, '5g': 20, '6g': 20, s1g: 2 };
export const BAND_OPTIONS = [['5g', '5 GHz'], ['2g', '2.4 GHz'], ['6g', '6 GHz'], ['s1g', 'HaLow (below 1 GHz)']];

// Until the node has answered: the widths without HaLow's channel plans.
const FALLBACK = { widths: { '2g': [20, 40], '5g': [20, 40, 80, 160], '6g': [20, 40, 80, 160], s1g: [1, 2, 4, 8] }, max_channel: { '2g': 14, '5g': 196, '6g': 233 }, s1g: null };
let asked = false;

// channels is the node's table, fetched once.
function channels() {
  if (!state.channels && !asked) {
    asked = true;
    get('/channels').then((c) => { state.channels = c; redraw(); }).catch(() => { asked = false; });
  }
  return state.channels || FALLBACK;
}

export const widths = (band) => channels().widths[band] || [];

// s1gChannels is a country's HaLow channels at a width: [] when it has none
// at that width, null when the plans are not known yet.
function s1gChannels(country, width) {
  const t = channels().s1g;
  if (!t) return null;
  return ((t[(country || '').toUpperCase()] || {})[width]) || [];
}

// compact writes a channel list short: "2, 6, 10 … 50" for a long even run.
function compact(list) {
  if (list.length <= 8) return list.join(', ');
  const step = list[1] - list[0];
  if (list.every((c, i) => i === 0 || c - list[i - 1] === step)) return `${list.slice(0, 3).join(', ')} … ${list[list.length - 1]}`;
  return list.join(', ');
}

// channelHint is the line under a channel field.
export function channelHint(band, width, country) {
  if (band !== 's1g') return `Any channel the radios have. Common: ${COMMON[band].join(', ')}`;
  const cc = (country || '').toUpperCase();
  const list = s1gChannels(cc, width);
  if (list === null) return '';
  if (list.length) return `${cc} channels at ${width} MHz: ${compact(list)}`;
  const t = channels().s1g[cc];
  if (!t) return `HaLow has no channel plan for ${cc || 'this country'}.`;
  return `${cc} has no ${width} MHz HaLow channels; it has ${Object.keys(t).join(', ')} MHz.`;
}

// defaultChannel proposes a channel: the usual one when it is free and
// valid, else the first that is.
function defaultChannel(band, width, country, taken = []) {
  const s1g = band === 's1g' ? s1gChannels(country, width) : null;
  const candidates = [DEFAULT_CHANNEL[band], ...(s1g || COMMON[band] || [])];
  const ok = (c) => !taken.includes(c) && (!s1g || s1g.includes(c));
  return candidates.find(ok) ?? DEFAULT_CHANNEL[band];
}

// linkLine describes one mesh link's channel: "5 GHz · channel 149 · 80 MHz".
export function linkLine(l) {
  const w = l.width_mhz ?? l.width;
  const run = l.running_width_mhz && l.running_width_mhz !== w ? ` (running ${l.running_width_mhz})` : '';
  return `${bandName(l.band)} · channel ${l.channel} · ${w} MHz${run}`;
}

const select = (value, options, onchange) => h('select.select', { value: String(value), onchange: (e) => { onchange(e.target.value); redraw(); } },
  options.map(([v, label]) => h('option', { value: String(v) }, label)));

// linkFields are the band, channel and width of one mesh link l (edited in
// place). key is its name in the profile ("radio", "links[0]"), which is how
// the node names it in an error; fields holds those errors; country is the
// profile's, which HaLow's channels depend on; others are the profile's
// other links, whose channels a new choice keeps away from.
export function linkFields(l, key, fields, { title = '', country = 'US', others = [] } = {}) {
  const taken = (b) => others.filter((o) => o.band === b).map((o) => o.channel);
  const setBand = (b) => {
    l.band = b;
    if (!widths(b).includes(l.width)) l.width = DEFAULT_WIDTH[b];
    l.channel = defaultChannel(b, l.width, country, taken(b));
  };
  const setWidth = (w) => {
    l.width = Number(w);
    const s1g = l.band === 's1g' ? s1gChannels(country, l.width) : null;
    if (s1g && s1g.length && !s1g.includes(l.channel)) l.channel = defaultChannel(l.band, l.width, country, taken(l.band));
  };
  return h('div.fields.link-fields',
    field(title ? `${title}: band` : 'Band', select(l.band, BAND_OPTIONS, setBand), { error: fields[key + '.band'] || fields[key] }),
    field('Channel', h('input.input', { type: 'number', min: '1', max: String(channels().max_channel[l.band] || ''), value: l.channel, class: fields[key + '.channel'] ? 'invalid' : '',
      oninput: (e) => { l.channel = Number(e.target.value); redraw(); } }),
    { error: fields[key + '.channel'], hint: channelHint(l.band, l.width, country) }),
    field('Width', select(l.width, widths(l.band).map((w) => [w, w + ' MHz']), setWidth),
      { error: fields[key + '.width'], hint: key === 'radio' ? 'Wider is faster at short range and worse at long range. A radio that cannot run it narrows the link, and the node says so.' : '' }));
}

// nextLink proposes another link for a list of links: the first band none
// uses yet, on its usual channel. bands, when given, are the bands this node
// has a radio for, tried first.
export function nextLink(links, country, bands = []) {
  const used = links.map((l) => l.band);
  // HaLow before 2.4 GHz: a 2.4 GHz radio is the usual home of the client Wi-Fi.
  const usual = ['5g', 's1g', '2g', '6g'];
  const order = [...usual.filter((b) => bands.includes(b)), ...usual];
  const band = order.find((b) => !used.includes(b)) || '5g';
  // A second link in a band needs another channel than the first.
  const taken = links.filter((l) => l.band === band).map((l) => l.channel);
  const width = DEFAULT_WIDTH[band];
  return { band, channel: defaultChannel(band, width, country, taken), width };
}
