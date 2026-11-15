package deb

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SigMember is the ar member that carries a release signature.
const SigMember = "_trellis-sig"

// Key formats, one line each:
//
//	trellis-release-key v1 <base64 of the 32-byte public key> <comment>
//	trellis-release-secret v1 <base64 of the 32-byte seed>
const (
	pubPrefix    = "trellis-release-key v1 "
	secretPrefix = "trellis-release-secret v1 "
	sigContext   = "trellis release v1\n"
)

// NewKey makes a release key: the secret line to keep, and the public line
// that nodes trust.
func NewKey(comment string) (secret, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	comment = strings.Join(strings.Fields(comment), " ")
	return secretPrefix + base64.StdEncoding.EncodeToString(priv.Seed()) + "\n",
		strings.TrimSpace(pubPrefix+base64.StdEncoding.EncodeToString(pub)+" "+comment) + "\n", nil
}

// ParseSecret reads a secret key line.
func ParseSecret(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, secretPrefix) {
		return nil, errors.New("not a Trellis release secret key")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, secretPrefix))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("release secret key is damaged")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKey is a key a node may trust.
type PublicKey struct {
	Key     ed25519.PublicKey
	Comment string
}

// ID is a short name for the key: the first 8 bytes of its SHA-256.
func (k PublicKey) ID() string { return keyID(k.Key) }

func keyID(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKeys reads public key lines; blank lines and # comments are
// skipped.
func ParsePublicKeys(s string) ([]PublicKey, error) {
	var out []PublicKey
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, pubPrefix) {
			return nil, fmt.Errorf("not a Trellis release key: %.40q", line)
		}
		b64, comment, _ := strings.Cut(strings.TrimPrefix(line, pubPrefix), " ")
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release key is damaged: %.40q", line)
		}
		out = append(out, PublicKey{Key: key, Comment: strings.TrimSpace(comment)})
	}
	return out, nil
}

// Public returns the public line of a secret key.
func Public(priv ed25519.PrivateKey, comment string) string {
	return strings.TrimSpace(pubPrefix+base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))+" "+comment) + "\n"
}

// digest is what a signature is made over: a fixed context line, then the
// SHA-256 of every byte of the package before the signature member.
func digest(signed []byte) []byte {
	sum := sha256.Sum256(signed)
	return append([]byte(sigContext), sum[:]...)
}

// Sign appends a signature member to an unsigned package.
func Sign(pkg []byte, priv ed25519.PrivateKey, when time.Time) ([]byte, error) {
	info, err := Read(pkg)
	if err != nil {
		return nil, err
	}
	if info.sig != "" {
		return nil, errors.New("package is already signed")
	}
	pub := priv.Public().(ed25519.PublicKey)
	body := fmt.Sprintf("trellis-sig v1\nkey: %s\nsig: %s\n", keyID(pub),
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, digest(pkg))))
	out := bytes.NewBuffer(append([]byte(nil), pkg...))
	arMember(out, SigMember, []byte(body), when)
	return out.Bytes(), nil
}

// ErrUnsigned means the package carries no signature.
var ErrUnsigned = errors.New("the package is not signed")

// Verify checks the package's signature against the trusted keys and
// returns the key that made it.
func (i *Info) Verify(trusted []PublicKey) (PublicKey, error) {
	if i.sig == "" {
		return PublicKey{}, ErrUnsigned
	}
	var id, sig string
	lines := strings.Split(strings.TrimSpace(i.sig), "\n")
	if len(lines) == 0 || lines[0] != "trellis-sig v1" {
		return PublicKey{}, errors.New("the package's signature is of an unknown kind")
	}
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l, ": ")
		switch k {
		case "key":
			id = v
		case "sig":
			sig = v
		}
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return PublicKey{}, errors.New("the package's signature is damaged")
	}
	for _, k := range trusted {
		if k.ID() != id {
			continue
		}
		if ed25519.Verify(k.Key, digest(i.signed), raw) {
			return k, nil
		}
		return PublicKey{}, fmt.Errorf("the signature does not match the package (key %s): the file was changed or damaged", id)
	}
	return PublicKey{}, fmt.Errorf("signed with key %s, which this node does not trust", id)
}

// Signed says whether the package carries a signature at all.
func (i *Info) Signed() bool { return i.sig != "" }
