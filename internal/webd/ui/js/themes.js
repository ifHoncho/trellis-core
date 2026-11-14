// Themes: the list is window.trellisThemes (theme.js, which applies the
// saved theme before the first paint). The quick switch cycles three
// places, dark → light → night; Settings → Appearance chooses which theme
// takes the dark and the light place. Choices are kept in this browser.
import { state, redraw } from './state.js';

export const SLOTS = ['dark', 'light', 'night'];
export const SLOT_ICONS = { dark: 'dusk', light: 'day', night: 'night' };

export const themes = () => window.trellisThemes || [{ id: 'dusk', name: 'Dusk', slot: 'dark' }];
const find = (id) => themes().find((t) => t.id === id);
export const themeName = (id) => (find(id) || {}).name || id;
export const slotOf = (id) => (find(id) || {}).slot || 'dark';
export const slotTheme = (slot) => (window.trellisSlotTheme ? window.trellisSlotTheme(slot) : { dark: 'dusk', light: 'day', night: 'night' }[slot]);

function store(k, v) {
  try { localStorage.setItem(k, v); } catch (e) { /* private mode */ }
}

export function setTheme(id) {
  state.theme = id;
  document.documentElement.setAttribute('data-theme', id);
  store('trellis.theme', id);
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', bg);
  redraw();
}

// nextTheme moves the quick switch to its next place.
export function nextTheme() {
  const slot = SLOTS[(SLOTS.indexOf(slotOf(state.theme)) + 1) % SLOTS.length];
  setTheme(slotTheme(slot));
}

// chooseTheme makes id the theme of its place, and shows it.
export function chooseTheme(id) {
  const slot = slotOf(id);
  if (slot !== 'night') store('trellis.theme.' + slot, id);
  setTheme(id);
}
