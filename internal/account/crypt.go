package account

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"strings"
)

// SHA-512 crypt ("$6$", Ulrich Drepper's specification, as glibc and
// libxcrypt implement it): a password hash any Linux system's chpasswd -e
// takes. Written here so that trellis-deploy can make one on Windows and
// macOS, which have no crypt(3) of their own.

const (
	cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	cryptRounds   = 5000 // the default: no "rounds=" in the hash
	maxSalt       = 16
)

// HashPassword returns a SHA-512 crypt hash of password with a random salt.
func HashPassword(password string) (string, error) {
	if err := CheckPassword(password); err != nil {
		return "", err
	}
	b := make([]byte, maxSalt)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	salt := make([]byte, maxSalt)
	for i, v := range b {
		salt[i] = cryptAlphabet[int(v)%len(cryptAlphabet)]
	}
	return sha512Crypt([]byte(password), salt), nil
}

// IsHash says whether s is a crypt hash chpasswd -e can take: "$id$...".
// Only the shape is checked; the system checks the rest.
func IsHash(s string) bool {
	return strings.HasPrefix(s, "$") && strings.Count(s, "$") >= 3 && !strings.ContainsAny(s, " \t\n:")
}

// sha512Crypt is the specification's algorithm with the default rounds.
func sha512Crypt(pw, salt []byte) string {
	if len(salt) > maxSalt {
		salt = salt[:maxSalt]
	}
	// B: password, salt, password.
	h := sha512.New()
	h.Write(pw)
	h.Write(salt)
	h.Write(pw)
	b := h.Sum(nil)

	// A: password, salt, then B for the password's length, then B or the
	// password for each bit of the length.
	a := sha512.New()
	a.Write(pw)
	a.Write(salt)
	i := len(pw)
	for ; i > 64; i -= 64 {
		a.Write(b)
	}
	a.Write(b[:i])
	for i = len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(b)
		} else {
			a.Write(pw)
		}
	}
	da := a.Sum(nil)

	// P: the password hashed as many times as it is long.
	h = sha512.New()
	for range len(pw) {
		h.Write(pw)
	}
	p := stretch(h.Sum(nil), len(pw))

	// S: the salt hashed 16 + DA[0] times.
	h = sha512.New()
	for range 16 + int(da[0]) {
		h.Write(salt)
	}
	s := stretch(h.Sum(nil), len(salt))

	c := da
	for r := range cryptRounds {
		h = sha512.New()
		if r&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if r%3 != 0 {
			h.Write(s)
		}
		if r%7 != 0 {
			h.Write(p)
		}
		if r&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	out.Write(salt)
	out.WriteByte('$')
	for _, t := range [...][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	} {
		b64(&out, c[t[0]], c[t[1]], c[t[2]], 4)
	}
	b64(&out, 0, 0, c[63], 2)
	return out.String()
}

// stretch repeats d to n bytes.
func stretch(d []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, d[:min(len(d), n-len(out))]...)
	}
	return out
}

func b64(out *strings.Builder, b2, b1, b0 byte, n int) {
	w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
	for range n {
		out.WriteByte(cryptAlphabet[w&0x3f])
		w >>= 6
	}
}

// CheckPassword applies the same limits as the web UI's admin password.
func CheckPassword(pw string) error {
	if l := len(pw); l < MinPassword || l > MaxPassword {
		return errors.New("password: 8 to 128 characters")
	}
	if strings.ContainsAny(pw, "\n\r") {
		return errors.New("password: no line breaks")
	}
	return nil
}

// Password limits (the web UI admin password's).
const (
	MinPassword = 8
	MaxPassword = 128
)
