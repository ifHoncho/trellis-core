package account

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// Key is one line of authorized_keys: options (rare, kept as written),
// type, the key and its comment.
type Key struct {
	Options     string `json:"options,omitempty"`
	Type        string `json:"type"`
	Data        string `json:"data"` // base64, as in the file
	Comment     string `json:"comment,omitempty"`
	Fingerprint string `json:"fingerprint"` // SHA256:…, as ssh-keygen -l shows it
}

// Line is the key as an authorized_keys line.
func (k Key) Line() string {
	parts := []string{}
	if k.Options != "" {
		parts = append(parts, k.Options)
	}
	parts = append(parts, k.Type, k.Data)
	if k.Comment != "" {
		parts = append(parts, k.Comment)
	}
	return strings.Join(parts, " ")
}

// The key types OpenSSH takes for login today (DSA is gone).
var keyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// ParseKey reads one public key line, as in a .pub file or authorized_keys.
func ParseKey(line string) (Key, error) {
	line = strings.TrimSpace(line)
	toks := tokens(line)
	at := -1
	for i, t := range toks {
		if keyTypes[t.text] {
			at = i
			break
		}
	}
	if at < 0 || at+1 >= len(toks) {
		return Key{}, fmt.Errorf("not an SSH public key (ssh-ed25519, ssh-rsa or ecdsa-sha2-… expected)")
	}
	k := Key{Type: toks[at].text, Data: toks[at+1].text}
	if at > 0 {
		k.Options = strings.TrimSpace(line[:toks[at].start])
	}
	if at+2 < len(toks) {
		k.Comment = strings.TrimSpace(line[toks[at+2].start:])
	}
	blob, err := base64.StdEncoding.DecodeString(k.Data)
	if err != nil {
		return Key{}, fmt.Errorf("%s key: not base64", k.Type)
	}
	// The blob starts with its own type: a string with a 4-byte length.
	if len(blob) < 4 {
		return Key{}, fmt.Errorf("%s key: too short", k.Type)
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n)+4 > uint64(len(blob)) || string(blob[4:4+n]) != k.Type {
		return Key{}, fmt.Errorf("%s key: its data is not a %s key", k.Type, k.Type)
	}
	sum := sha256.Sum256(blob)
	k.Fingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	return k, nil
}

// ParseKeys reads public keys, one per line; blank lines and # comments
// are skipped. An error names the line.
func ParseKeys(text string) ([]Key, error) {
	var keys []Key
	for i, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, err := ParseKey(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// MergeKeys adds keys not already in list (by key data), in order.
func MergeKeys(list []Key, add ...Key) []Key {
	have := map[string]bool{}
	for _, k := range list {
		have[k.Data] = true
	}
	for _, k := range add {
		if !have[k.Data] {
			list = append(list, k)
			have[k.Data] = true
		}
	}
	return list
}

type token struct {
	text  string
	start int
}

// tokens splits on spaces outside double quotes (authorized_keys options
// may hold quoted spaces: command="…").
func tokens(s string) []token {
	var out []token
	start, quoted := -1, false
	for i, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			if start < 0 {
				start = i
			}
		case (r == ' ' || r == '\t') && !quoted:
			if start >= 0 {
				out = append(out, token{s[start:i], start})
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		out = append(out, token{s[start:], start})
	}
	return out
}
