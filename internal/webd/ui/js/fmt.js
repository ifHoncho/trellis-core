// Formatting. Numbers are written the way an operator reads them aloud.

export function mbps(v) {
  if (v == null || isNaN(v)) return '–';
  if (v >= 100) return v.toFixed(0);
  if (v >= 10) return v.toFixed(1);
  return v.toFixed(v > 0 && v < 1 ? 2 : 1);
}

export function duration(seconds) {
  seconds = Math.max(0, Math.floor(seconds));
  const d = Math.floor(seconds / 86400), hrs = Math.floor(seconds % 86400 / 3600), m = Math.floor(seconds % 3600 / 60), s = seconds % 60;
  if (d > 0) return `${d}d ${hrs}h`;
  if (hrs > 0) return `${hrs}h ${String(m).padStart(2, '0')}m`;
  if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

export function since(iso, now = Date.now()) {
  const t = Date.parse(iso);
  if (!t || t < 86400000) return '–';
  return duration((now - t) / 1000);
}

export function ago(ms) {
  if (ms == null) return '–';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return duration(ms / 1000);
}

export function bytes(n) {
  if (n == null) return '–';
  const units = ['B', 'kB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${units[i]}`;
}

export function count(n) {
  if (n == null) return '–';
  return Number(n).toLocaleString('en-US');
}

export const BANDS = { '2g': '2.4 GHz', '5g': '5 GHz', '6g': '6 GHz', s1g: 'HaLow' };
export const bandName = (b) => BANDS[b] || b || '–';

// Signal quality from dBm: 0 (poor) to 4 (excellent) bars, and a tone.
export function signal(dbm) {
  if (!dbm) return { bars: 0, tone: 'faint', word: 'unknown' };
  if (dbm >= -55) return { bars: 4, tone: 'ok', word: 'excellent' };
  if (dbm >= -67) return { bars: 3, tone: 'ok', word: 'good' };
  if (dbm >= -76) return { bars: 2, tone: 'warn', word: 'fair' };
  return { bars: 1, tone: 'bad', word: 'weak' };
}

// Link quality class 0–3 from throughput (Mbit/s) and signal.
export function linkQuality(tput, dbm) {
  let q = tput >= 40 ? 3 : tput >= 15 ? 2 : tput >= 5 ? 1 : 0;
  if (dbm && dbm < -80) q = Math.min(q, 1);
  return q;
}

// roleLink is the mesh link a radio role carries, counted from 1 ("mesh" is
// 1, "mesh3+ap" is 3), or 0 for a role without a mesh link.
export function roleLink(r) {
  const key = (r || '').split('+')[0];
  if (key === 'mesh') return 1;
  const m = /^mesh(\d+)$/.exec(key);
  return m ? Number(m[1]) : 0;
}

// meshLinks lists a status's mesh links in order: the first, then the
// profile's further ones (each has index, band, channel, width_mhz, peers).
export const meshLinks = (st) => [st.mesh_link, ...(st.more_links || [])];

// linkName is what the UI calls a mesh link, on every page: its band and
// channel, "5 GHz · ch 149", which is the same on every node of the mesh.
// Interface names (mesh0, mesh1) and link numbers are not shown.
export function linkName(l, st) {
  if (!l) return '–';
  const band = l.band || (l.index === 1 && st ? st.profile.band : '');
  const ch = l.channel || (l.index === 1 && st ? st.profile.channel : 0);
  if (!band) return l.transport === 'wired' ? 'wired' : '–';
  return `${bandName(band)} · ch ${ch}`;
}

// linkWidth is a link's width in MHz as the profile sets it.
export const linkWidth = (l, st) => l.width_mhz || (l.index === 1 ? st.profile.width_mhz : 0);

// linkAt is mesh link n (from 1, as roles and the topology count them).
export const linkAt = (st, n) => (n > 0 ? meshLinks(st)[n - 1] || null : null);

// linkOn is the mesh link whose interface is ifname, or null (a wired port).
export const linkOn = (st, ifname) => meshLinks(st).find((l) => l.interface && l.interface === ifname) || null;

// fewerRadios describes a node with fewer mesh radios than the profile has
// links (one dual-band USB adapter, typically): it meshes on the links its
// radios are on and nowhere else. Null when every link has a radio, or
// before the node has decided.
export function fewerRadios(st) {
  const links = meshLinks(st);
  const radios = (st.radios || []).filter((r) => r.mesh_point && !r.ignored);
  if (links.length < 2 || radios.length >= links.length || st.mesh_link.transport === 'not yet determined') return null;
  const on = links.filter(carried), off = links.filter((l) => !carried(l));
  const and = (xs) => xs.length < 2 ? xs.join('') : xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1];
  const one = radios.length === 1;
  return {
    on, off, radios,
    lead: `${one ? 'This node has one mesh radio' : `This node has ${radios.length} mesh radios`} for ${links.length} links` +
      (one && radios[0].bands && radios[0].bands.length > 1 ? ', and it works on one band at a time' : '') + '. ' +
      (on.length ? `It is on ${and(on.map((l) => linkName(l, st)))} now, and connects directly only to nodes on ${on.length === 1 ? 'that channel' : 'those channels'}; ` +
        `it reaches nodes on ${and(off.map((l) => linkName(l, st)))} only through a node that carries both.` : 'It is on no link yet.'),
  };
}

// roleKind is a radio role in one or two words, for a badge: "mesh",
// "mesh + Wi-Fi", "Wi-Fi", "idle". Which link is told by linkName.
export function roleKind(r) {
  if (!r || r === 'unused') return 'idle';
  if (r === 'ap') return 'Wi-Fi';
  return roleLink(r) ? (r.endsWith('+ap') ? 'mesh + Wi-Fi' : 'mesh') : r;
}

// roleName is a radio role in words, naming its link: "Mesh on 5 GHz · ch
// 149", "Mesh on 2.4 GHz · ch 6 + client Wi-Fi", "Client Wi-Fi", "Unused".
export function roleName(r, st) {
  if (!r) return '–';
  if (r === 'unused') return 'Unused';
  if (r === 'ap') return 'Client Wi-Fi';
  const n = roleLink(r);
  if (!n) return r;
  const on = st && linkAt(st, n) ? ` on ${linkName(linkAt(st, n), st)}` : '';
  return `Mesh${on}${r.endsWith('+ap') ? ' + client Wi-Fi' : ''}`;
}

// radioBands names one radio's bands: "2.4/5 GHz", "HaLow".
export function radioBands(bands) {
  const names = (bands || []).map(bandName);
  if (names.length > 1 && names.every((n) => n.endsWith(' GHz'))) return names.map((n) => n.replace(' GHz', '')).join('/') + ' GHz';
  return names.join('/');
}

// meshRadios names a node's mesh radios as it reports them, one group of
// bands per radio: "2.4/5 GHz" (one dual-band radio), "2.4 GHz + 5 GHz"
// (two radios). Null when the node does not report them.
export const meshRadios = (n) => (n.mesh_radios ? n.mesh_radios.map(radioBands).join(' + ') || 'none' : null);

// carried reports whether this node has a radio (an interface) for a link
// of its status. A node meshes over the links it has radios for.
export const carried = (l) => !!(l && l.interface);

export const capitalize = (s) => (s ? s[0].toUpperCase() + s.slice(1) : s);

export function plural(n, one, many) {
  return `${n} ${n === 1 ? one : (many || one + 's')}`;
}
