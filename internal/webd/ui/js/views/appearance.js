// Settings → Appearance: which themes the quick switch cycles. Each card is
// drawn in its own theme (the theme blocks in app.css apply to any element
// with data-theme), so what you see is what you get.
import { h } from '../dom.js';
import { state } from '../state.js';
import { panel, dot, icon } from '../ui.js';
import { themes, slotTheme, chooseTheme, setTheme, SLOT_ICONS } from '../themes.js';

const SLOT_TEXT = {
  dark: ['Dark', 'For indoors and dim light. Dusk is Trellis’s own.'],
  light: ['Light', 'For daylight and bright screens. Day is Trellis’s own, made for sunlight.'],
  night: ['Night', 'Red on black, dim, to keep night vision. It has one hue, so status shows by shape alone: squares for faults, triangles for warnings, rings for notes.'],
};

// card: a theme as a small node page, in its own colours.
function card(t) {
  const inSwitch = slotTheme(t.slot) === t.id;
  const showing = state.theme === t.id;
  return h('button.theme-card', {
    'data-theme': t.id, 'aria-pressed': String(inSwitch),
    title: t.slot === 'night' ? 'Show the night theme' : `Use ${t.name} for the ${t.slot} place on the quick switch`,
    onclick: () => (t.slot === 'night' ? setTheme(t.id) : chooseTheme(t.id)),
  },
    h('span.tc-head', h('span.tc-name', t.name), showing || inSwitch ? h('span.tc-tag', [inSwitch ? 'on the switch' : '', showing ? 'showing' : ''].filter(Boolean).join(' · ')) : null),
    h('span.tc-body',
      h('span.tc-line.mono', 'relay-1'),
      h('span.tc-line.sub', '10.73.12.1 · 6 nodes'),
      h('span.tc-bar', h('i', { style: { width: '62%' } })),
      h('span.tc-status', dot('ok'), h('span', 'ok'), dot('warn'), h('span', 'warn'), dot('bad'), h('span', 'fault')),
      h('span.tc-btn', 'Apply')));
}

export function appearanceSection() {
  const all = themes();
  const intro = panel('Quick switch', {},
    h('p.dim', { style: { fontSize: '14px', maxWidth: '80ch' } },
      'The theme button at the foot of the menu (at the top on a phone) cycles three places: a dark theme, a light theme and night. Choose below which theme takes the dark and the light place. The choice is kept in this browser; each device has its own.'),
    h('div.switch-preview', ['dark', 'light', 'night'].map((slot) => {
      const id = slotTheme(slot);
      return h('div', { class: state.theme === id ? 'now' : '', title: `${SLOT_TEXT[slot][0]} place${state.theme === id ? ', showing now' : ''}` },
        icon(SLOT_ICONS[slot]), h('span', all.find((t) => t.id === id)?.name || id));
    })));
  const groups = ['dark', 'light', 'night'].map((slot) => panel(`${SLOT_TEXT[slot][0]} themes`, { meta: slot === 'night' ? 'fixed' : 'pick one' },
    h('p.dim', { style: { fontSize: '13.5px', margin: '0 0 12px' } }, SLOT_TEXT[slot][1]),
    h('div.theme-grid', all.filter((t) => t.slot === slot).map(card))));
  return [intro, ...groups];
}
