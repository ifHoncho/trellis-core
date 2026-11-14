package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

// Version is the API version in every path: /v1/… on meshd's socket and
// /api/v1/… on webd.
const Version = "v1"

// Channels is what a mesh link's band, channel and width may be, for forms.
// It is the same for every node: a radio may still lack a channel, and the
// regulatory domain may forbid one, which meshd reports at run time.
type Channels struct {
	// Widths in MHz per band.
	Widths map[string][]int `json:"widths"`
	// Wi-Fi channel numbers run from 1 to these, per band, and map to
	// frequencies the way the kernel maps them.
	MaxChannel map[string]int `json:"max_channel"`
	// S1G holds the HaLow channels per country and width in MHz:
	// {"US": {"2": [2, 6, …], …}, …}.
	S1G map[string]map[int][]int `json:"s1g"`
}

// ChannelTable builds the Channels answer.
func ChannelTable() Channels {
	c := Channels{Widths: map[string][]int{}, MaxChannel: map[string]int{config.Band2G: 14, config.Band5G: 196, config.Band6G: 233}, S1G: map[string]map[int][]int{}}
	for _, b := range []string{config.Band2G, config.Band5G, config.Band6G, config.BandS1G} {
		c.Widths[b] = config.ChannelWidths(b)
	}
	for _, cc := range config.S1GCountries() {
		m := map[int][]int{}
		for _, w := range config.S1GWidths {
			if chs := config.S1GChannels(cc, w); len(chs) > 0 {
				m[w] = chs
			}
		}
		c.S1G[cc] = m
	}
	return c
}

// ConfigResult is the configuration in force, with secrets redacted.
type ConfigResult struct {
	Node    config.Node    `json:"node"`
	Profile config.Profile `json:"profile"`
	Setup   status.Setup   `json:"setup"`
}

// ApplyResult answers a configuration change, or a dry run of one.
type ApplyResult struct {
	// Applied is false for a dry run, and when nothing would change.
	Applied bool `json:"applied"`
	// Changed reports whether the request differs from what is in force.
	Changed bool `json:"changed"`
	// Effects is what the change does to a running node, most serious
	// first: an "error" level effect cuts the node off from the mesh or
	// from whoever is configuring it; a "warning" interrupts service.
	Effects []status.Warning `json:"effects"`
	// AP is how to join the client AP once the change is in force, when the
	// change alters that (a new name or passphrase).
	AP *status.APJoin `json:"ap,omitempty"`
}

// ProfileExport is the mesh profile with its secret, in every form another
// node can take it in.
type ProfileExport struct {
	Profile config.Profile `json:"profile"`
	YAML    string         `json:"yaml"`
	Code    string         `json:"code"`
	QR      *status.QR     `json:"qr,omitempty"` // the code as a QR symbol
}

// ProfileInput is a mesh profile to install: exactly one of the three
// forms.
type ProfileInput struct {
	Code    string          `json:"code,omitempty"`    // a profile code (from a QR code or pasted)
	YAML    string          `json:"yaml,omitempty"`    // a profile file's contents
	Profile *config.Profile `json:"profile,omitempty"` // the document itself; sae_passphrase "<redacted>" keeps the current one
	// New asks for a new mesh instead: a profile with a fresh secret.
	New *NewProfile `json:"new,omitempty"`
}

// NewProfile describes a mesh to create. Zero values take the defaults.
type NewProfile struct {
	Name    string `json:"name"`
	Band    string `json:"band,omitempty"`
	Channel int    `json:"channel,omitempty"`
	Width   int    `json:"width,omitempty"`
	Mode    string `json:"mode,omitempty"` // regulatory mode
	Country string `json:"country,omitempty"`
	// Links are further mesh links after the first (band, channel and
	// width above), as in a profile's links.
	Links []config.RadioPlan `json:"links,omitempty"`
}

// SetupInput provisions a node in one step: the mesh profile and the
// node's name (architecture §12.6: "flash, scan profile QR, name it").
type SetupInput struct {
	ProfileInput
	Callsign string `json:"callsign"`
	NodeID   int    `json:"node_id,omitempty"` // 0: automatic
}

// IdentifyInput asks a radio to blink its LED.
type IdentifyInput struct {
	Fingerprint string `json:"fingerprint"`
	Seconds     int    `json:"seconds,omitempty"` // default 10, at most 60
}

// WebInfo is what webd needs to know to serve the UI.
type WebInfo struct {
	Callsign string `json:"callsign"`
	Version  string `json:"version"`
	AdminSet bool   `json:"admin_set"` // an admin password exists
	// AdminSince is when the admin password was last set. When it changes,
	// webd ends every session: they were opened under the old password.
	AdminSince time.Time `json:"admin_since,omitzero"`
	Setup      bool      `json:"setup"`      // the node waits to be provisioned
	WANAccess  bool      `json:"wan_access"` // serve the UI outside the mesh prefix too
	Prefix     string    `json:"prefix"`     // the mesh prefix
}

// LoginInput is a password to check.
type LoginInput struct {
	Password string `json:"password"`
}

// PasswordInput sets the admin password.
type PasswordInput struct {
	Current string `json:"current,omitempty"`
	New     string `json:"new"`
}

// Error is an API failure: an HTTP status, a message, and for rejected
// configuration the fields at fault.
type Error struct {
	Status     int          `json:"-"`
	Message    string       `json:"error"`
	Fields     []FieldError `json:"fields,omitempty"`
	RetryAfter int          `json:"retry_after_s,omitempty"`
}

// FieldError names one rejected field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an Error.
func Errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Invalid is a 400: the request itself is wrong.
func Invalid(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, format, args...)
}

// Forbidden is a 403.
func Forbidden(format string, args ...any) *Error {
	return Errorf(http.StatusForbidden, format, args...)
}

// ErrBadPassword is a failed login.
var ErrBadPassword = &Error{Status: http.StatusUnauthorized, Message: "wrong password"}

// TooMany is a 429 with the time to wait.
func TooMany(wait time.Duration, format string, args ...any) *Error {
	e := Errorf(http.StatusTooManyRequests, format, args...)
	e.RetryAfter = int(wait/time.Second) + 1
	return e
}

// asError turns any error into an *Error. Configuration errors become a
// 422 naming every field.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var es config.Errors
	if errors.As(err, &es) {
		out := &Error{Status: http.StatusUnprocessableEntity, Message: err.Error()}
		for _, fe := range es {
			out.Fields = append(out.Fields, FieldError{Field: fe.Field, Message: fe.Msg})
		}
		return out
	}
	return &Error{Status: http.StatusInternalServerError, Message: err.Error()}
}
