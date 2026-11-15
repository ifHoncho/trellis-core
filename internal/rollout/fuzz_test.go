package rollout

import (
	"encoding/json"
	"testing"
)

// A rollout from another node: whatever passes Validate is safe to act on.
func FuzzDecodeRollout(f *testing.F) {
	f.Add([]byte(`{"id":"0a1b2c","version":"0.4.1","packages":[{"arch":"arm64","sha256":"` +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","size":5}],"order":["a","b"]}`))
	f.Add([]byte(`{"order":["a","a"]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r Rollout
		if json.Unmarshal(b, &r) != nil || r.Validate() != nil {
			return
		}
		if len(r.Order) == 0 || len(r.Packages) == 0 || r.ID == "" {
			t.Fatalf("validated without nodes, packages or id: %+v", r)
		}
		for _, p := range r.Packages {
			if p.Size <= 0 || len(p.SHA256) != 64 {
				t.Fatalf("validated a bad package: %+v", p)
			}
		}
	})
}
