// Applied before the first paint, so the page never flashes the wrong
// theme. Kept out of the module graph for that reason; app.js reads the
// list from window.trellisThemes.
//
// The quick switch cycles three places: dark, light and night. Which theme
// takes the dark and the light place is chosen in Settings → Appearance
// and kept in this browser; night is always the red night-vision theme.
// Every id here has a [data-theme="…"] block in app.css.
(function () {
  var themes = [
    { id: 'dusk', name: 'Dusk', slot: 'dark' },
    { id: 'nord', name: 'Nord', slot: 'dark' },
    { id: 'gruvbox-dark', name: 'Gruvbox Dark', slot: 'dark' },
    { id: 'dracula', name: 'Dracula', slot: 'dark' },
    { id: 'tokyo-night', name: 'Tokyo Night', slot: 'dark' },
    { id: 'catppuccin-mocha', name: 'Catppuccin Mocha', slot: 'dark' },
    { id: 'one-dark', name: 'One Dark', slot: 'dark' },
    { id: 'solarized-dark', name: 'Solarized Dark', slot: 'dark' },
    { id: 'everforest-dark', name: 'Everforest Dark', slot: 'dark' },
    { id: 'rose-pine', name: 'Rosé Pine', slot: 'dark' },
    { id: 'day', name: 'Day', slot: 'light' },
    { id: 'gruvbox-light', name: 'Gruvbox Light', slot: 'light' },
    { id: 'solarized-light', name: 'Solarized Light', slot: 'light' },
    { id: 'catppuccin-latte', name: 'Catppuccin Latte', slot: 'light' },
    { id: 'everforest-light', name: 'Everforest Light', slot: 'light' },
    { id: 'rose-pine-dawn', name: 'Rosé Pine Dawn', slot: 'light' },
    { id: 'night', name: 'Night (red)', slot: 'night' },
  ];
  window.trellisThemes = themes;
  var known = function (id, slot) {
    for (var i = 0; i < themes.length; i++) if (themes[i].id === id && (!slot || themes[i].slot === slot)) return true;
    return false;
  };
  var get = function (k) { try { return localStorage.getItem(k); } catch (e) { return null; } };
  // The theme chosen for a place on the quick switch.
  window.trellisSlotTheme = function (slot) {
    if (slot === 'night') return 'night';
    var t = get('trellis.theme.' + slot);
    return known(t, slot) ? t : (slot === 'light' ? 'day' : 'dusk');
  };
  var t = get('trellis.theme');
  if (!known(t)) {
    t = window.trellisSlotTheme(window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  }
  document.documentElement.setAttribute('data-theme', t);
})();
