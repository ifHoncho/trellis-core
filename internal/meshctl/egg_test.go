package meshctl

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// If it isn't tested, it doesn't work: the easter eggs print what their
// expected output files hold, through meshctl itself.
func TestEggs(t *testing.T) {
	for _, name := range []string{"batman", "trellis"} {
		want, err := os.ReadFile("egg/" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if rc := Main("test", []string{name}, &out, &errOut); rc != 0 {
			t.Fatalf("meshctl %s: exit %d: %s", name, rc, errOut.String())
		}
		if out.String() != string(want) {
			t.Errorf("meshctl %s printed:\n%s\nwant:\n%s", name, out.String(), want)
		}
	}
	// Not advertised.
	var usage bytes.Buffer
	Main("test", nil, &bytes.Buffer{}, &usage)
	if strings.Contains(usage.String(), "\n  batman") || strings.Contains(usage.String(), "\n  trellis") {
		t.Error("the usage gives the easter eggs away")
	}
}

func TestBF(t *testing.T) {
	run := func(src, in string, steps int) (string, error) {
		var out bytes.Buffer
		err := runBF([]byte(src), strings.NewReader(in), &out, steps)
		return out.String(), err
	}
	// Hello World (Wikipedia's), with comments around it.
	hello := "says hi: ++++++++[>++++[>++>+++>+++>+<<<<-]>+>+>->>+[<]<-]>>.>---.+++++++..+++.>>.<-.<.+++.------.--------.>>+.>++. done"
	if got, err := run(hello, "", 1000000); err != nil || got != "Hello World!\n" {
		t.Errorf("hello: %q %v", got, err)
	}
	// Input: echo until the end (0), with cells that wrap.
	if got, err := run(",[.,]", "mesh", 1000); err != nil || got != "mesh" {
		t.Errorf("echo: %q %v", got, err)
	}
	if got, err := run("-.", "", 10); err != nil || got != "\xff" {
		t.Errorf("wrap: %q %v", got, err)
	}
	for src, want := range map[string]string{
		"[":     "unmatched [",
		"]":     "unmatched ]",
		"<":     "off the start",
		"+[]":   "ran too long",
		"+[>+]": "off the end",
	} {
		if _, err := run(src, "", 100000); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", src, err, want)
		}
	}
	if _, err := run("+[]", "", 50); !errors.Is(err, errBFSteps) {
		t.Errorf("step limit: %v", err)
	}
}
