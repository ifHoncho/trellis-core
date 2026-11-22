// The mesh as a picture: nodes on a lattice of links.
//
// Layout is a small force simulation in which a better link pulls its two
// nodes closer, so the picture resembles how the nodes hear each other.
// Positions are kept between draws (a node does not jump when a number
// changes) and nodes can be dragged where the operator wants them; those
// places are remembered in this browser.
import { h } from './dom.js';
import { mbps, linkQuality, plural, linkName, linkAt, bandName, meshLinks } from './fmt.js';
import { state, redraw, shortMac } from './state.js';

// One coordinate space for every drawing of the mesh, so the overview and
// the mesh page show the same picture: wide on a desk, tall on a phone.
let W = 1000, HGT = 600;
const narrow = window.matchMedia('(max-width: 820px)');
function shape() {
  const [w, hgt] = narrow.matches ? [600, 720] : [1000, 600];
  if (w !== W) {
    W = w; HGT = hgt;
    pos.clear();
    known = '';
  }
}
narrow.addEventListener('change', () => redraw());
const pinKey = (id) => (narrow.matches ? 'p:' : 'w:') + id;
const PIN_KEY = 'trellis.topo.pins';

const pos = new Map();   // node id → {x, y, pinned}
let known = '';          // the node and link set the layout was made for
let pins = loadPins();
let drag = null;

function loadPins() {
  try { return JSON.parse(localStorage.getItem(PIN_KEY) || '{}'); } catch (e) { return {}; }
}
function savePins() {
  try { localStorage.setItem(PIN_KEY, JSON.stringify(pins)); } catch (e) { /* private mode */ }
}
export function resetLayout() {
  pins = {};
  savePins();
  pos.clear();
  known = '';
  redraw();
}
export const hasPins = () => Object.keys(pins).length > 0;

// Views of the map: every link, or only one band's, the site link's or the
// cables'. Nodes stay where they are; only the links drawn change. The
// choice is kept in this browser. Each band the profile has a mesh link on
// is a view even with no link on the map: an empty view says that band is
// down everywhere.
const VIEW_KEY = 'trellis.topo.view';
const KIND_ORDER = ['all', '2g', '5g', '6g', 's1g', 'site', 'wired'];
let view = (() => { try { return localStorage.getItem(VIEW_KEY) || 'all'; } catch (e) { return 'all'; } })();

// bandOf is a mesh link's band; the first link's is the profile's.
const bandOf = (st, link) => (link && (link.band || (link.index === 1 && st.profile ? st.profile.band : ''))) || '';

// linkKind is the band of the mesh link an edge is on, or site or wired.
function linkKind(l) {
  if (l.medium === 'site') return 'site';
  if (l.medium === 'wired') return 'wired';
  const st = state.status;
  return (st && bandOf(st, linkAt(st, l.mesh_link))) || 'other';
}
const viewName = (k) => (k === 'all' ? 'All links' : k === 'site' ? 'Site link' : k === 'wired' ? 'Wired' : bandName(k));

// views are the map's views: all, then each band of the profile's mesh
// links and each kind of link the map has.
function views(topo) {
  const kinds = new Set((topo && topo.links || []).map(linkKind));
  const st = state.status;
  if (st) for (const l of meshLinks(st)) { const b = bandOf(st, l); if (b) kinds.add(b); }
  return KIND_ORDER.filter((k) => k === 'all' || kinds.has(k));
}
// currentView is the saved view while the map offers it (a button shows
// with three views or more), otherwise all links.
const currentView = (topo) => {
  const vs = views(topo);
  return vs.length > 2 && vs.includes(view) ? view : 'all';
};

// viewButton cycles the views; none when there is one kind of link (a
// one-band profile, no site link, no cable).
export function viewButton(topo, button) {
  const vs = views(topo);
  if (vs.length <= 2) return null;
  const cur = currentView(topo);
  return button(`View: ${viewName(cur)}`, () => {
    view = vs[(vs.indexOf(cur) + 1) % vs.length];
    try { localStorage.setItem(VIEW_KEY, view); } catch (e) { /* private mode */ }
    redraw();
  }, 'small', { title: 'Cycle link view: ' + vs.map(viewName).join(' · ') });
}

// rate is the figure a link is judged and labelled by. Each end estimates
// the throughput it can send to the other, and the two often differ a lot
// (by a factor of four is common). The slower direction is what
// the link reliably carries, so that is the figure; a direction an end has
// not reported (0) is left out. The node tooltip shows both.
// A site link is the exception: its figure is the profile's
// (site_link.mbit), told to batman-adv because a tunnel reports no speed,
// not a measurement, so the map neither prints nor judges it (isSite).
const isSite = (l) => l.medium === 'site';
const rate = (l) => {
  const ab = l.ab_mbps || 0, ba = l.ba_mbps || 0;
  return ab > 0 && ba > 0 ? Math.min(ab, ba) : Math.max(ab, ba);
};

// widest finds the path from src to dst whose slowest link is fastest
// (fewer hops on a tie), not through the nodes in banned: how BATMAN_V
// ranks paths, less its hop penalty. null when there is none.
function widest(topo, src, dst, banned) {
  const best = new Map([[src, { cap: Infinity, hops: 0, prev: null, link: null }]]);
  const done = new Set();
  for (;;) {
    let u = null, bu = null;
    for (const [id, b] of best) {
      if (!done.has(id) && (!bu || b.cap > bu.cap || (b.cap === bu.cap && b.hops < bu.hops))) { u = id; bu = b; }
    }
    if (u === null) return null;
    if (u === dst) break;
    done.add(u);
    for (const l of topo.links) {
      if (l.a !== u && l.b !== u) continue;
      const v = l.a === u ? l.b : l.a;
      if (banned.has(v) || done.has(v)) continue;
      const cap = Math.min(bu.cap, rate(l) || 0.001);
      const cur = best.get(v);
      if (!cur || cap > cur.cap || (cap === cur.cap && bu.hops + 1 < cur.hops)) best.set(v, { cap, hops: bu.hops + 1, prev: u, link: l });
    }
  }
  const out = [];
  for (let v = dst; v !== src; v = best.get(v).prev) {
    const b = best.get(v);
    out.unshift({ from: b.prev, to: v, link: b.link });
  }
  return out;
}

// routeTo is the path from this node to node dest: the first hop is
// batman-adv's own (this node's next hop towards it), the rest the widest
// path on the map from there; total is batman-adv's figure for the whole
// path. { none: true } when batman-adv has no route; { partial: true,
// total } when it has one the map cannot draw (a node on the way does not
// report its links).
function routeTo(topo, dest) {
  const st = state.status;
  const self = topo.nodes.find((n) => n.self);
  if (!self || !st || dest === self.id) return null;
  // Its originators from gossip; a node not heard in gossip yet by its
  // own address, which batman-adv may know it by.
  const node = (st.nodes || []).find((x) => x.mac === dest);
  const origs = node && (node.originators || []).length ? node.originators : [dest];
  const o = (st.batman.originators || []).find((r) => r.best && origs.includes(r.mac));
  if (!o) return { none: true };
  const partial = { partial: true, total: o.throughput_mbps };
  // The link batman-adv sends on: to its next hop, and with two links to
  // that neighbour (dual backhaul), the one on its outgoing interface.
  const toHop = topo.links.filter((l) => (l.a === self.id && l.b_mac === o.next_hop) || (l.b === self.id && l.a_mac === o.next_hop));
  // One link to it: that one. Several: the one on the outgoing interface,
  // and if none is (a wired or site next hop the map shows otherwise), the
  // route only, not a link batman-adv's figure is not for.
  const first = toHop.length === 1 ? toHop[0]
    : toHop.find((l) => { const ml = linkAt(st, l.mesh_link); return ml && ml.interface === o.via; });
  if (!first) return partial;
  const n1 = first.a === self.id ? first.b : first.a;
  const rest = n1 === dest ? [] : widest(topo, n1, dest, new Set([self.id]));
  if (rest === null) return partial;
  const hops = [{ from: self.id, to: n1, link: first }, ...rest];
  return { hops, links: new Set(hops.map((x) => x.link)), nodes: new Set([self.id, ...hops.map((x) => x.to)]),
    total: o.throughput_mbps, estimated: rest.length > 0 };
}

function layout(topo, H) {
  const ids = topo.nodes.map((n) => n.id);
  const sig = ids.join(',') + '|' + topo.links.map((l) => l.a + l.b).join(',');
  for (const id of [...pos.keys()]) if (!ids.includes(id)) pos.delete(id);
  const fresh = ids.filter((id) => !pos.has(id));
  if (sig === known && !fresh.length) return;
  const cold = pos.size === 0;
  known = sig;

  // New nodes start on a ring (the whole mesh at first), by position in
  // the list, so the same mesh always starts the same way.
  ids.forEach((id, i) => {
    if (pos.has(id)) return;
    const pin = pins[pinKey(id)];
    if (pin) { pos.set(id, { x: pin.x, y: pin.y, pinned: true }); return; }
    const a = (i / ids.length) * Math.PI * 2 - Math.PI / 2;
    const r = cold ? 0.3 : 0.38;
    pos.set(id, { x: W / 2 + Math.cos(a) * W * r, y: H / 2 + Math.sin(a) * H * r });
  });
  if (ids.length < 2) {
    const p = pos.get(ids[0]);
    if (p && !p.pinned) { p.x = W / 2; p.y = H / 2; }
    return;
  }

  const springs = topo.links.filter((l) => pos.has(l.a) && pos.has(l.b)).map((l) => ({
    a: pos.get(l.a), b: pos.get(l.b),
    // 130 for an excellent link, up to 330 for a marginal one.
    // A site link at the longest: its sites are apart.
    len: isSite(l) ? 330 : 130 + 200 * (1 - Math.min(rate(l), 100) / 100),
  }));
  const pts = ids.map((id) => pos.get(id));
  const steps = cold ? 420 : 160;
  for (let s = 0; s < steps; s++) {
    const heat = 1 - s / steps;
    const f = pts.map(() => [0, 0]);
    for (let i = 0; i < pts.length; i++) {
      for (let j = i + 1; j < pts.length; j++) {
        let dx = pts[i].x - pts[j].x, dy = pts[i].y - pts[j].y;
        let d2 = dx * dx + dy * dy;
        if (d2 < 1) { dx = (i - j) || 1; dy = 1; d2 = dx * dx + dy * dy; }
        const d = Math.sqrt(d2);
        const push = 52000 / d2;
        f[i][0] += dx / d * push; f[i][1] += dy / d * push;
        f[j][0] -= dx / d * push; f[j][1] -= dy / d * push;
      }
      // A weak pull to the middle keeps unlinked nodes on the page.
      f[i][0] += (W / 2 - pts[i].x) * 0.012;
      f[i][1] += (H / 2 - pts[i].y) * 0.02;
    }
    // A node must not sit on a link between two others: it would look
    // like a relay on that link. Push it off, and the link the other way.
    for (let i = 0; i < pts.length; i++) {
      const p = pts[i];
      for (const sp of springs) {
        if (sp.a === p || sp.b === p) continue;
        const ex = sp.b.x - sp.a.x, ey = sp.b.y - sp.a.y;
        const l2 = ex * ex + ey * ey || 1;
        const t = Math.max(0.08, Math.min(0.92, ((p.x - sp.a.x) * ex + (p.y - sp.a.y) * ey) / l2));
        let nx = p.x - (sp.a.x + ex * t), ny = p.y - (sp.a.y + ey * t);
        let d = Math.sqrt(nx * nx + ny * ny);
        if (d > 70) continue;
        if (d < 0.5) { nx = -ey; ny = ex; d = Math.sqrt(l2); }
        const push = (70 - d) * 0.22;
        f[i][0] += nx / d * push; f[i][1] += ny / d * push;
        const ia = pts.indexOf(sp.a), ib = pts.indexOf(sp.b);
        f[ia][0] -= nx / d * push * 0.4; f[ia][1] -= ny / d * push * 0.4;
        f[ib][0] -= nx / d * push * 0.4; f[ib][1] -= ny / d * push * 0.4;
      }
    }
    for (const sp of springs) {
      const dx = sp.b.x - sp.a.x, dy = sp.b.y - sp.a.y;
      const d = Math.sqrt(dx * dx + dy * dy) || 1;
      const pull = (d - sp.len) * 0.06;
      const i = pts.indexOf(sp.a), j = pts.indexOf(sp.b);
      f[i][0] += dx / d * pull; f[i][1] += dy / d * pull;
      f[j][0] -= dx / d * pull; f[j][1] -= dy / d * pull;
    }
    pts.forEach((p, i) => {
      if (p.pinned) return;
      const cap = 26 * heat + 1;
      p.x += Math.max(-cap, Math.min(cap, f[i][0]));
      p.y += Math.max(-cap, Math.min(cap, f[i][1]));
    });
  }
  fit(pts, H);
}

// fit scales and centres the free nodes into the drawing, leaving room for
// labels. Pinned nodes stay where the operator put them.
function fit(pts, H) {
  const free = pts.filter((p) => !p.pinned);
  if (!free.length) return;
  const mx = W < 800 ? 62 : 90, top = 60, bottom = 70;
  let x0 = Infinity, x1 = -Infinity, y0 = Infinity, y1 = -Infinity;
  for (const p of free) {
    x0 = Math.min(x0, p.x); x1 = Math.max(x1, p.x);
    y0 = Math.min(y0, p.y); y1 = Math.max(y1, p.y);
  }
  const sw = x1 - x0 || 1, sh = y1 - y0 || 1;
  // Fill the drawing in both directions, but never stretch one more than
  // 1.6 times the other: distances should still mean something.
  let kx = Math.min((W - 2 * mx) / sw, 2.2), ky = Math.min((H - top - bottom) / sh, 2.2);
  kx = Math.min(kx, ky * 1.6);
  ky = Math.min(ky, kx * 1.6);
  const ox = (W - sw * kx) / 2, oy = top + (H - top - bottom - sh * ky) / 2;
  for (const p of free) {
    p.x = ox + (p.x - x0) * kx;
    p.y = oy + (p.y - y0) * ky;
  }
}

function svgPoint(svg, e) {
  const m = svg.getScreenCTM();
  if (!m) return null;
  const p = new DOMPoint(e.clientX, e.clientY).matrixTransform(m.inverse());
  return { x: p.x, y: p.y };
}

// topology(topo, {small, height}) → the drawing.
export function topology(topo, opts = {}) {
  shape();
  const H = HGT;
  // On a small drawing everything is drawn larger, to stay legible.
  const big = narrow.matches ? 1.5 : opts.small ? 1.35 : 1;
  const ui = (state.ui.topo ||= { hover: null, sel: null });
  if (!topo || !topo.nodes || !topo.nodes.length) {
    return h('div.topo', { class: opts.small ? 'small' : '' }, h('div.topo-empty', 'Waiting for topology…'));
  }
  layout(topo, H);
  const byId = new Map(topo.nodes.map((n) => [n.id, n]));
  const byNodeId = new Map(topo.nodes.filter((n) => n.node_id).map((n) => [n.node_id, n]));
  // The view (Mesh page only): which links are drawn.
  const shownView = opts.small ? 'all' : currentView(topo);
  const visible = visibleIn(shownView);
  // A selected node shows its path from this node; hovering another shows
  // that one's links.
  const path = !opts.small && ui.sel && byId.has(ui.sel) ? routeTo(topo, ui.sel) : null;
  const onPath = path && path.links && (!ui.hover || path.nodes.has(ui.hover));
  const focus = onPath ? null : ui.hover || ui.sel;
  const touches = (l) => l.a === focus || l.b === focus;
  const near = new Set([focus]);
  if (focus) for (const l of topo.links) if (touches(l) && visible(l)) { near.add(l.a); near.add(l.b); }

  // Which links carry a node's traffic to its gateway: the direct link to
  // the gateway where there is one; for this node, otherwise its first hop.
  const flows = new Map();
  for (const n of topo.nodes) {
    const g = byNodeId.get(n.selected_gateway);
    if (!g || g.id === n.id) continue;
    const direct = topo.links.find((l) => (l.a === n.id && l.b === g.id) || (l.b === n.id && l.a === g.id));
    if (direct) flows.set(direct, direct.a === n.id ? 'fwd' : 'rev');
  }
  const self = topo.nodes.find((n) => n.self);
  if (self && state.status) {
    const g = byNodeId.get(self.selected_gateway);
    if (g && g.id !== self.id && ![...flows.keys()].some((l) => (l.a === self.id && l.b === g.id) || (l.b === self.id && l.a === g.id))) {
      const gwNode = (state.status.nodes || []).find((x) => x.mac === g.id);
      const route = gwNode && (state.status.batman.originators || []).find((o) => o.best && (gwNode.originators || []).includes(o.mac));
      const hop = route && topo.links.find((l) => (l.a === self.id && l.b_mac === route.next_hop) || (l.b === self.id && l.a_mac === route.next_hop));
      if (hop) flows.set(hop, hop.a === self.id ? 'fwd' : 'rev');
    }
  }

  const showLabels = !opts.small || topo.links.length <= 8;
  const pairSeen = new Map();
  const linkEls = [], flowEls = [], labelEls = [];
  for (const l of topo.links) {
    const a = pos.get(l.a), b = pos.get(l.b);
    if (!a || !b) continue;
    const inPath = onPath && path.links.has(l);
    if (!visible(l) && !inPath) continue;
    const site = isSite(l);
    const t = rate(l);
    const q = linkQuality(t, Math.min(l.a_signal_dbm || 0, l.b_signal_dbm || 0));
    const half = !site && !(l.ab_mbps > 0 && l.ba_mbps > 0);
    const dim = onPath ? !inPath : focus && !touches(l);
    // A second link between the same two nodes (dual backhaul) bows out.
    const pair = [l.a, l.b].sort().join('|');
    const nth = pairSeen.get(pair) || 0;
    pairSeen.set(pair, nth + 1);
    const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
    const dx = b.x - a.x, dy = b.y - a.y, len = Math.sqrt(dx * dx + dy * dy) || 1;
    const bow = nth === 0 ? 0 : (nth % 2 ? 1 : -1) * 26 * Math.ceil(nth / 2);
    const cx = mx - dy / len * bow, cy = my + dx / len * bow;
    const d = `M${a.x.toFixed(1)} ${a.y.toFixed(1)} Q${cx.toFixed(1)} ${cy.toFixed(1)} ${b.x.toFixed(1)} ${b.y.toFixed(1)}`;
    const width = (site ? 2.4 : 1.6 + 5 * Math.sqrt(Math.min(t, 150) / 150)) * (big > 1 ? 1.25 : 1);
    const key = l.a_mac + l.b_mac;
    linkEls.push(h('path', { class: `link ${site ? 'site' : 'q' + q}${half ? ' half' : ''}${dim ? ' dimmed' : ''}${inPath ? ' path' : ''}`, d, 'stroke-width': width.toFixed(1), key: 'l' + key }));
    const flow = flows.get(l);
    if (flow && !dim) flowEls.push(h('path', { class: 'flow' + (flow === 'rev' ? ' rev' : ''), d, key: 'f' + key }));
    if (showLabels || (focus && touches(l)) || inPath) {
      labelEls.push(h('text', { class: 'linklabel' + (dim ? ' dimmed' : ''), x: ((mx + cx) / 2).toFixed(1), y: ((my + cy) / 2 + 3.5).toFixed(1), key: 't' + key }, site ? 'internet' : mbps(t)));
    }
  }

  // A node's name goes on the side its links do not: above when most of
  // them leave downwards.
  const pull = new Map();
  for (const l of topo.links) {
    const a = pos.get(l.a), b = pos.get(l.b);
    if (!a || !b) continue;
    const len = Math.hypot(b.x - a.x, b.y - a.y) || 1;
    pull.set(l.a, (pull.get(l.a) || 0) + (b.y - a.y) / len);
    pull.set(l.b, (pull.get(l.b) || 0) + (a.y - b.y) / len);
  }

  const nodeEls = topo.nodes.map((n) => {
    const p = pos.get(n.id);
    const above = (pull.get(n.id) || 0) > 0.35;
    const r = (n.self ? 17 : 13) * big;
    const cls = ['node', n.self ? 'self' : '', n.gateway ? 'gw' : '', !n.reported ? 'silent' : '', !n.callsign ? 'unknown' : '',
      (onPath ? !path.nodes.has(n.id) : focus && !near.has(n.id)) ? 'dimmed' : '', focus === n.id || (onPath && ui.sel === n.id) ? 'hover' : ''].filter(Boolean).join(' ');
    const leaves = [];
    const shown = Math.min(n.clients || 0, 7);
    for (let i = 0; i < shown; i++) {
      // End-user devices: leaves on an arc to the upper right.
      const a = (above ? Math.PI / 2 - 0.5 - i * 0.36 : -Math.PI / 2 + 0.5 + i * 0.36);
      leaves.push(h('circle.leaf', { cx: (Math.cos(a) * (r + 10)).toFixed(1), cy: (Math.sin(a) * (r + 10)).toFixed(1), r: (2.4 * big).toFixed(1) }));
    }
    const start = (e) => {
      const svg = e.currentTarget.ownerSVGElement;
      const at = svgPoint(svg, e);
      if (!at) return;
      drag = { id: n.id, svg, dx: p.x - at.x, dy: p.y - at.y, moved: false, x0: e.clientX, y0: e.clientY };
      try { e.currentTarget.setPointerCapture(e.pointerId); } catch (err) { /* a pointer the browser does not track */ }
    };
    const move = (e) => {
      if (!drag || drag.id !== n.id) return;
      if (!drag.moved && Math.hypot(e.clientX - drag.x0, e.clientY - drag.y0) < 5) return;
      const at = svgPoint(drag.svg, e);
      if (!at) return;
      drag.moved = true;
      p.x = Math.max(30, Math.min(W - 30, at.x + drag.dx));
      p.y = Math.max(30, Math.min(H - 40, at.y + drag.dy));
      p.pinned = true;
      redraw();
    };
    const end = () => {
      if (!drag || drag.id !== n.id) return;
      if (drag.moved) {
        pins[pinKey(n.id)] = { x: Math.round(p.x), y: Math.round(p.y) };
        savePins();
      } else {
        ui.sel = ui.sel === n.id ? null : n.id; // a tap selects
      }
      drag = null;
      redraw();
    };
    return h('g', { class: cls, key: 'n' + n.id, transform: `translate(${p.x.toFixed(1)} ${p.y.toFixed(1)})`,
      onpointerdown: start, onpointermove: move, onpointerup: end, onpointercancel: end,
      onpointerenter: () => { if (!drag) { ui.hover = n.id; redraw(); } },
      onpointerleave: () => { if (ui.hover === n.id) { ui.hover = null; redraw(); } } },
      n.self ? h('path.ring', { d: `M0 ${-r - 7} ${r + 7} 0 0 ${r + 7} ${-r - 7} 0Z` }) : null,
      h('path.body', { d: `M0 ${-r} ${r} 0 0 ${r} ${-r} 0Z` }),
      n.gateway ? h('path', { class: 'uplink glyph' + (n.wan_healthy ? '' : ' down'), transform: big > 1 ? `scale(${big})` : null, d: n.wan_healthy ? 'M0 5V-5M-4 -1 0 -5 4 -1' : 'M-4 -4 4 4M4 -4 -4 4' }) : null,
      leaves,
      (n.clients || 0) > shown ? h('text.sub', { x: r + 24, y: above ? r + 2 : -r + 2 }, `+${n.clients - shown}`) : null,
      h('text.name', { y: above ? -(r + (opts.small ? 13 : 20 * big)) : r + (opts.small ? 24 : 18 * big) }, n.callsign || shortMac(n.id)),
      opts.small ? null : h('text.sub', { y: above ? -(r + 8 * big) : r + 31 * big }, !n.callsign ? 'unnamed' : !n.reported ? 'no link report' : n.node_id ? `#${n.node_id}` : ''));
  });

  // The node under the pointer, beside it. On the Mesh page the selected
  // node's card is under the map (selection), not over the path it lists.
  let tip = null;
  const tipId = opts.small ? focus : ui.hover && ui.hover !== ui.sel ? ui.hover : null;
  const fn = tipId && byId.get(tipId);
  if (fn && !drag) {
    const p = pos.get(fn.id);
    // Below a node in the upper part of the map, so it is not cut off.
    tip = h('div.topo-tip', { class: p.y < H * 0.45 ? 'below' : '', style: { left: (p.x / W * 100) + '%', top: (p.y / H * 100) + '%' } },
      nodeInfo(topo, fn, byId, visible));
  }
  // A view with no link drawn says so.
  const empty = shownView !== 'all' && !topo.links.some((l) => linkKind(l) === shownView)
    ? h('div.topo-note', `${viewName(shownView)}: no links`) : null;

  return h('div.topo', { class: [opts.small ? 'small' : '', big > 1 ? 'big' : ''].join(' ') },
    h('svg', { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'xMidYMid meet', role: 'img', 'aria-label': `Mesh topology: ${topo.nodes.length} nodes, ${topo.links.length} links`,
      onpointerdown: (e) => { if (e.target === e.currentTarget && ui.sel) { ui.sel = null; redraw(); } } },
      h('g', linkEls), h('g', flowEls), h('g', labelEls), h('g', nodeEls)),
    empty, tip);
}

// visibleIn tells which links a view draws. A link of no known band
// ('other') shows in every view: hiding it would hide a link the view may
// be about.
const visibleIn = (view) => (l) => view === 'all' || linkKind(l) === view || linkKind(l) === 'other';

// nodeInfo is what the map says of a node: its address, its links in the
// view both ways, its way to the internet.
function nodeInfo(topo, fn, byId, visible) {
  const byNodeId = new Map(topo.nodes.filter((n) => n.node_id).map((n) => [n.node_id, n]));
  const gw = byNodeId.get(fn.selected_gateway);
  const mine = topo.links.filter((l) => (l.a === fn.id || l.b === fn.id) && visible(l));
  return [
    h('b', fn.callsign || shortMac(fn.id)), fn.self ? h('span.accent', 'this node') : null,
    h('div', fn.address || ''),
    h('div.dim', plural(mine.length, 'link'), fn.reported ? ` · ${plural(fn.clients || 0, 'device')}` : ''),
    // Each link both ways, as each end measures what it can send.
    mine.length ? h('div.tip-links', [...mine].sort((x, y) => isSite(x) - isSite(y) || rate(y) - rate(x)).map((l) => {
      const out = l.a === fn.id ? l.ab_mbps : l.ba_mbps, back = l.a === fn.id ? l.ba_mbps : l.ab_mbps;
      const other = byId.get(l.a === fn.id ? l.b : l.a);
      const name = h('span', (other && other.callsign) || shortMac(l.a === fn.id ? l.b : l.a));
      if (isSite(l)) return h('div', name, h('span.faint', edgeName(l)), h('span.faint.tip-span', 'via internet, not measured'));
      return h('div', name, h('span.faint', edgeName(l)),
        h('span.num', out > 0 ? `→ ${mbps(out)}` : '→ –'), h('span.num', back > 0 ? `← ${mbps(back)}` : '← –'));
    })) : null,
    fn.gateway ? h('div', { class: fn.wan_healthy ? 'ok' : 'bad' }, fn.wan_healthy ? 'gateway' : 'uplink down') : null,
    fn.reported && gw && gw.id !== fn.id ? h('div.dim', `internet via ${gw.callsign}`) : null,
    fn.reported && !gw && !fn.gateway ? h('div.dim', 'no internet') : null,
    fn.version ? h('div.faint', fn.version) : null,
  ];
}

// selection is the selected node's card, under the map on the Mesh page:
// the node, and its path from this node. null when none is selected.
export function selection(topo) {
  const ui = state.ui.topo;
  if (!ui || !ui.sel || !topo || !topo.nodes) return null;
  const byId = new Map(topo.nodes.map((n) => [n.id, n]));
  const fn = byId.get(ui.sel);
  if (!fn) return null;
  const path = routeTo(topo, fn.id);
  return h('div.topo-sel',
    h('div.sel-node', nodeInfo(topo, fn, byId, visibleIn(currentView(topo)))),
    path ? pathTip(path, byId) : null,
    h('button.btn.small.sel-clear', { type: 'button', title: 'Clear selection', onclick: () => { ui.sel = null; redraw(); } }, 'Clear'));
}

// pathTip lists a selected node's path from this node, hop by hop.
function pathTip(path, byId) {
  const name = (id) => { const n = byId.get(id); return (n && n.callsign) || shortMac(id); };
  if (path.none) return h('div.tip-path', h('div.dim', 'Path from this node: no route'));
  if (path.partial) {
    return h('div.tip-path', h('div.dim', 'Path from this node'),
      h('div', 'Path: ', h('strong', path.total ? mbps(path.total) : '–'), h('span.faint', ' Mbit/s (batman-adv)')),
      h('div.faint', 'Hops: not all on the map'));
  }
  return h('div.tip-path',
    h('div.dim', 'Path from this node'),
    h('div.tip-links', path.hops.map((x) => h('div', h('span', name(x.to)), h('span.faint', edgeName(x.link)),
      h('span.num', isSite(x.link) ? 'internet' : mbps(rate(x.link)))))),
    path.hops.some((x) => isSite(x.link))
      ? h('div', 'Path: ', h('span.faint', 'over the internet, not measured'))
      : h('div', 'Path: ', h('strong', path.total ? mbps(path.total) : '–'), h('span.faint', ' Mbit/s (batman-adv)')),
    path.estimated ? h('div.faint', 'Hops after the first: best path by link figures') : null);
}

// edgeName names the link an edge of the map is on, by its band and
// channel as everywhere in the UI ("5 GHz · ch 149"), from the mesh link
// the nodes report it on; "wired" for a cable, "site link" for a tunnel
// over the internet, empty when neither end says.
function edgeName(l) {
  if (l.medium === 'wired') return 'wired';
  if (l.medium === 'site') return 'site link';
  const link = state.status && linkAt(state.status, l.mesh_link);
  return link ? linkName(link, state.status) : '';
}

// acrossSites gives the node IDs this map shows on the far side of a site
// link from this node: on no link at all from here but through a site
// link. null when the map has no site link, or does not show this node.
// A node with no link on the map is on neither side.
export function acrossSites(topo) {
  if (!topo || !topo.links.some(isSite)) return null;
  const self = topo.nodes.find((n) => n.self);
  if (!self) return null;
  const near = new Set([self.id]), todo = [self.id];
  while (todo.length) {
    const id = todo.pop();
    for (const l of topo.links) {
      if (isSite(l) || (l.a !== id && l.b !== id)) continue;
      const o = l.a === id ? l.b : l.a;
      if (!near.has(o)) { near.add(o); todo.push(o); }
    }
  }
  const linked = new Set(topo.links.flatMap((l) => [l.a, l.b]));
  return new Set(topo.nodes.filter((n) => n.node_id && linked.has(n.id) && !near.has(n.id)).map((n) => n.node_id));
}

// The site link's entry only where the map has one.
export function legend(topo) {
  const sites = !!topo && topo.links.some(isSite);
  return h('div.topo-legend',
    h('span', h('i'), 'strong link'), h('span', h('i.q1'), 'fair'), h('span', h('i.q0'), 'weak'),
    h('span', h('i.half'), 'one end reporting'),
    sites ? h('span', { title: 'Tunnel via internet. Throughput not measured (batman-adv uses the profile figure; not shown).' }, h('i.site'), 'site link (not measured)') : null,
    h('span', '◆ this node'), h('span', '↑ gateway'), h('span', '• device'), h('span', 'Mbit/s: slower direction'));
}
