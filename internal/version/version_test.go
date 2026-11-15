package version

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		numbered bool
		exact    bool
		series   string
	}{
		{"0.3.1", true, true, "0.3"},
		{"v0.3.1", true, true, "0.3"},
		{"0.3.1-4-g1a2b3c4", true, false, "0.3"},
		{"0.3.1-4-g1a2b3c4-dirty+1114.093000", true, false, "0.3"},
		{"2.0.0", true, true, "2"},
		{"2.4.7+hotfix", true, false, "2"},
		{"1a2b3c4-dirty+1114.093000", false, false, ""},
		{"dev", false, false, ""},
		{"", false, false, ""},
		{"1.2", false, false, ""},
		{"1.2.3.4", false, false, ""},
		// A short hash made of digits and a dot-free tail is not a release.
		{"1234567", false, false, ""},
	}
	for _, c := range cases {
		v := Parse(c.in)
		if v.Numbered != c.numbered || v.Exact != c.exact || v.Series() != c.series {
			t.Errorf("Parse(%q) = numbered %v exact %v series %q, want %v %v %q",
				c.in, v.Numbered, v.Exact, v.Series(), c.numbered, c.exact, c.series)
		}
	}
}

func TestCompare(t *testing.T) {
	if Parse("0.3.1").Compare(Parse("0.3.2")) != -1 || Parse("1.0.0").Compare(Parse("0.9.9")) != 1 ||
		Parse("0.3.1-2-gabc").Compare(Parse("0.3.1")) != 0 {
		t.Fatal("Compare orders release numbers wrongly")
	}
}

func TestDebian(t *testing.T) {
	cases := map[string]string{
		"0.3.1":                            "0.3.1",
		"v0.3.1":                           "0.3.1",
		"0.3.1-4-g1a2b3c4":                 "0.3.1+4.g1a2b3c4",
		"0.3.1-4-g1a2b3c4-dirty+1114.0930": "0.3.1+4.g1a2b3c4.dirty+1114.0930",
		"1a2b3c4-dirty+1114.093000":        "0.0.0~1a2b3c4.dirty+1114.093000",
		"dev":                              "0.0.0~dev",
		"":                                 "0.0.0~dev",
		"0.3.1+2026 11 14":                 "0.3.1+2026.11.14",
		"cb5916f":                          "0.0.0~cb5916f",
		"1.0.0-rc1":                        "1.0.0~rc1",
		"release/weird_name":               "0.0.0~release.weird.name",
		"0.3.1-":                           "0.3.1",
		"0.0.0~already+deb":                "0.0.0~already+deb",
		"10.20.30-1-gdeadbee":              "10.20.30+1.gdeadbee",
		"v1.2.3-dirty":                     "1.2.3+dirty",
		"\tv1.2.3\n":                       "1.2.3",
		"1.2.3-4-gabc/def":                 "1.2.3+4.gabc.def",
		"e747932+1114.064919":              "0.0.0~e747932+1114.064919",
		"0.1.0-rc.1+build.5":               "0.1.0~rc.1+build.5",
		"0.1.0.":                           "0.0.0~0.1.0",
		"....":                             "0.0.0~dev",
		"0.1.0-beta_2":                     "0.1.0~beta.2",
		"1a2b3c4-dirty":                    "0.0.0~1a2b3c4.dirty",
		"v0.1.0-0-g1234567":                "0.1.0+0.g1234567",
		"0.1.0~pre":                        "0.1.0~pre",
		"0.1.0+":                           "0.1.0",
		"V1.2.3":                           "0.0.0~V1.2.3",
		"1.2.3-αβγ":                        "1.2.3",
		"1.2.3-a b":                        "1.2.3~a.b",
		"1.2.3--x":                         "1.2.3~x",
	}
	cases["0.3.1-rc1-4-g1a2b3c4"] = "0.3.1~rc1.4.g1a2b3c4"
	for in, want := range cases {
		if got := Debian(in); got != want {
			t.Errorf("Debian(%q) = %q, want %q", in, got, want)
		}
	}
}
