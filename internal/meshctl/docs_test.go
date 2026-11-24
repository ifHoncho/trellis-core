package meshctl

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReferenceListsCommands: every command in the usage text is in
// docs/reference.md, "Command line (meshctl)", so the two cannot drift.
func TestReferenceListsCommands(t *testing.T) {
	var usage bytes.Buffer
	Main("test", nil, &bytes.Buffer{}, &usage)
	ref, err := os.ReadFile("../../docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(ref)
	start := strings.Index(doc, "\n## Command line (meshctl)\n")
	if start < 0 {
		t.Fatal(`docs/reference.md: no section "Command line (meshctl)"`)
	}
	section := doc[start+1:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	word := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	n := 0
	for _, line := range strings.Split(usage.String(), "\n") {
		if !strings.HasPrefix(line, "  ") || len(line) < 3 || line[2] < 'a' || line[2] > 'z' {
			continue
		}
		// The command ends where the description starts, after two spaces.
		spec := strings.SplitN(strings.TrimSpace(line), "  ", 2)[0]
		var cmd []string
		for _, f := range strings.Fields(spec) {
			if !word.MatchString(f) {
				break
			}
			cmd = append(cmd, f)
		}
		n++
		if !strings.Contains(section, "`meshctl "+strings.Join(cmd, " ")) {
			t.Errorf("docs/reference.md, Command line (meshctl): no row for `meshctl %s`", strings.Join(cmd, " "))
		}
	}
	if n < 20 {
		t.Fatalf("read %d commands from the usage text: its layout changed", n)
	}
}
