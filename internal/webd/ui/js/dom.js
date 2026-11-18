// A small DOM builder and differ. The UI redraws whole views from state
// whenever the node reports (every two seconds); morph() then changes only
// what differs, so focus, text selections, open <details> and running CSS
// transitions survive a redraw.

const SVG_NS = 'http://www.w3.org/2000/svg';
const SVG_TAGS = new Set(['svg', 'g', 'path', 'circle', 'line', 'rect', 'text', 'tspan', 'polyline', 'polygon',
  'defs', 'title', 'ellipse', 'marker', 'clipPath', 'pattern', 'linearGradient', 'stop', 'use', 'symbol']);

// h('div.panel.wide', {onclick: fn, hidden: true}, child, [children], 'text')
// Tag classes and attrs.class are merged. Handlers (on*) are listeners.
// style is an object applied through the CSSOM (the page's CSP forbids
// style attributes). null, undefined and false children are skipped.
export function h(spec, attrs, ...children) {
  if (attrs != null && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) {
    children.unshift(attrs);
    attrs = null;
  }
  const [tag, ...classes] = spec.split('.');
  const el = SVG_TAGS.has(tag) ? document.createElementNS(SVG_NS, tag) : document.createElement(tag);
  if (attrs && attrs.class) classes.push(...String(attrs.class).split(/\s+/));
  const cls = classes.filter(Boolean).join(' ');
  if (cls) el.setAttribute('class', cls);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class' || v == null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') {
      (el.__on ||= {})[k.slice(2)] = v;
      el.addEventListener(k.slice(2), v);
    } else if (k === 'style') {
      for (const [p, val] of Object.entries(v)) {
        if (val == null) continue;
        if (p.startsWith('--')) el.style.setProperty(p, val); else el.style[p] = val;
      }
    } else if (k === 'value') {
      el.__value = String(v);
      if (tag === 'textarea') el.textContent = el.__value;
      else if (tag !== 'select') el.setAttribute('value', el.__value);
    } else if (k === 'checked') {
      el.__checked = !!v;
      if (v) el.setAttribute('checked', '');
    } else if (k === 'key') {
      el.__key = String(v);
    } else if (v === true) {
      el.setAttribute(k, '');
    } else {
      el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  if (tag === 'select' && el.__value != null) el.value = el.__value;
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

const sameKind = (a, b) => a.nodeType === b.nodeType && a.nodeName === b.nodeName && (a.__key ?? null) === (b.__key ?? null);

// morph makes the live node `from` equal to the freshly built `to`, and
// returns the node now in the document.
export function morph(from, to) {
  if (!sameKind(from, to)) {
    from.replaceWith(to);
    return to;
  }
  if (from.nodeType === Node.TEXT_NODE) {
    if (from.data !== to.data) from.data = to.data;
    return from;
  }
  if (from.nodeType !== Node.ELEMENT_NODE) return from;

  for (const a of [...from.attributes]) {
    if (a.name !== 'style' && !to.hasAttribute(a.name)) from.removeAttribute(a.name);
  }
  for (const a of to.attributes) {
    if (a.name !== 'style' && from.getAttribute(a.name) !== a.value) from.setAttribute(a.name, a.value);
  }
  if (from.style && to.style && from.style.cssText !== to.style.cssText) from.style.cssText = to.style.cssText;

  if (from.__on || to.__on) {
    for (const [ev, fn] of Object.entries(from.__on || {})) from.removeEventListener(ev, fn);
    for (const [ev, fn] of Object.entries(to.__on || {})) from.addEventListener(ev, fn);
    from.__on = to.__on;
  }
  // Form controls: what the user typed stays, unless the model behind the
  // control changed since the last draw.
  if (to.__value !== undefined && from.__value !== to.__value) {
    from.__value = to.__value;
    if (from.value !== to.__value) from.value = to.__value;
  }
  if (to.__checked !== undefined && from.__checked !== to.__checked) {
    from.__checked = to.__checked;
    from.checked = to.__checked;
  }
  if (from.hasAttribute('data-static')) return from; // owned by other code
  morphChildren(from, to);
  if (from.nodeName === 'SELECT' && to.__value !== undefined && from.value !== to.__value && document.activeElement !== from) {
    from.value = to.__value;
  }
  return from;
}

// Children are matched by key where they have one (lists that reorder,
// views that replace each other) and by position otherwise.
function morphChildren(from, to) {
  const next = [...to.childNodes];
  const nextKeys = new Set();
  for (const n of next) if (n.__key != null) nextKeys.add(n.__key);
  const old = new Map();
  for (const c of from.childNodes) if (c.__key != null) old.set(c.__key, c);

  let at = from.firstChild;
  const dropStale = () => {
    while (at && at.__key != null && !nextKeys.has(at.__key)) {
      const gone = at;
      at = at.nextSibling;
      gone.remove();
    }
  };
  for (const n of next) {
    dropStale();
    if (n.__key != null) {
      const match = old.get(n.__key);
      if (match && sameKind(match, n)) {
        old.delete(n.__key);
        morph(match, n);
        if (match !== at) from.insertBefore(match, at);
        else at = at.nextSibling;
      } else {
        from.insertBefore(n, at);
      }
    } else if (at && at.__key == null && sameKind(at, n)) {
      const keep = at;
      at = at.nextSibling;
      morph(keep, n);
    } else {
      from.insertBefore(n, at);
    }
  }
  while (at) {
    const gone = at;
    at = at.nextSibling;
    gone.remove();
  }
}

// mount draws view() into root now and returns a function that redraws.
export function mount(root, view) {
  let live = null;
  let queued = false;
  const draw = () => {
    queued = false;
    const fresh = view();
    if (live && live.isConnected) live = morph(live, fresh);
    else {
      root.replaceChildren(fresh);
      live = fresh;
    }
  };
  draw();
  return () => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(draw);
  };
}
