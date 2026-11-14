package webd

import (
	"regexp"
	"slices"
	"testing"
)

// Every theme theme.js lists has a block in app.css with every colour, and
// app.css has no theme that theme.js does not list.
func TestThemesMatchCSS(t *testing.T) {
	js, err := uiFS.ReadFile("ui/js/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := uiFS.ReadFile("ui/app.css")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, m := range regexp.MustCompile(`\{ id: '([a-z0-9-]+)', name: '[^']+', slot: '(dark|light|night)' \}`).FindAllSubmatch(js, -1) {
		listed = append(listed, string(m[1]))
	}
	if len(listed) < 3 || !slices.Contains(listed, "dusk") || !slices.Contains(listed, "day") || !slices.Contains(listed, "night") {
		t.Fatalf("theme.js lists %v", listed)
	}
	blocks := map[string]string{}
	for _, m := range regexp.MustCompile(`(?s)\[data-theme="([a-z0-9-]+)"\] \{(.*?)\n\}`).FindAllSubmatch(css, -1) {
		blocks[string(m[1])] = string(m[2])
	}
	vars := []string{"--bg", "--bg-2", "--bg-3", "--line", "--line-2", "--ink", "--ink-2", "--ink-3", "--accent", "--accent-2", "--on-accent", "--ok", "--warn", "--bad", "--info", "--lattice", "--shadow", "--qr-bg", "color-scheme"}
	for _, id := range listed {
		b, ok := blocks[id]
		if !ok {
			t.Errorf("theme %q has no [data-theme] block in app.css", id)
			continue
		}
		for _, v := range vars {
			if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(v) + `:`).MatchString(b) {
				t.Errorf("theme %q does not set %s", id, v)
			}
		}
	}
	for id := range blocks {
		if !slices.Contains(listed, id) {
			t.Errorf("app.css has theme %q, which theme.js does not list", id)
		}
	}
}
