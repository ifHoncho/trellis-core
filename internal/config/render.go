package config

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Redacted is what a secret reads as in API output. Sent back unchanged in
// an update, it means "keep the current value".
const Redacted = "<redacted>"

// IsZero reports whether the selection is the default (auto), so that a
// written node.yaml leaves it out.
func (r RoleSelection) IsZero() bool { return r.Mode == "" || r.Mode == RoleAuto }

// MarshalJSON renders the selection in the same short forms as YAML:
// "auto", "disabled", {"radio": fp} or {"port": path}.
func (r RoleSelection) MarshalJSON() ([]byte, error) {
	v, _ := r.MarshalYAML()
	return json.Marshal(v)
}

// UnmarshalJSON accepts the forms MarshalJSON writes.
func (r *RoleSelection) UnmarshalJSON(data []byte) error {
	*r = RoleSelection{}
	var s string
	if json.Unmarshal(data, &s) == nil {
		r.Mode = s
		return nil
	}
	var m struct {
		Radio string `json:"radio"`
		Port  string `json:"port"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return errors.New("expected \"auto\", \"disabled\", {\"radio\": fingerprint} or {\"port\": port path}")
	}
	r.Radio, r.Port = m.Radio, m.Port
	switch {
	case m.Radio != "" && m.Port != "":
		return errors.New("set either radio or port, not both")
	case m.Radio != "":
		r.Mode = "radio"
	case m.Port != "":
		r.Mode = "port"
	default:
		return errors.New("expected radio: <fingerprint> or port: <port path>")
	}
	return nil
}

// MarshalJSON writes every role the config names, automatic ones included:
// {"mesh": "auto", "mesh2": {"radio": "…"}, "ap": "disabled"}.
func (r RoleOverrides) MarshalJSON() ([]byte, error) {
	_, m := r.roleMap(false)
	return json.Marshal(m)
}

// UnmarshalJSON accepts the form MarshalJSON writes.
func (r *RoleOverrides) UnmarshalJSON(data []byte) error {
	var m map[string]RoleSelection
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*r = RoleOverrides{}
	for k, sel := range m {
		if err := r.set(k, sel); err != nil {
			return err
		}
	}
	return nil
}

// Sparse returns a copy with every field that equals its default cleared,
// the inverse of ApplyDefaults. A file written from it stays short and
// follows the defaults of later releases.
func (n Node) Sparse() Node {
	if n.ProfilePath == DefaultProfilePath {
		n.ProfilePath = ""
	}
	roles := RoleOverrides{}
	for i := 0; i <= len(n.Radios.Roles.More); i++ {
		if sel := n.Radios.Roles.Link(i); !sel.IsZero() {
			roles.SetLink(i, sel)
		}
	}
	if !n.Radios.Roles.AP.IsZero() {
		roles.AP = n.Radios.Roles.AP
	}
	n.Radios.Roles = roles
	if n.AP.Enabled != nil && *n.AP.Enabled {
		n.AP.Enabled = nil
	}
	if n.AP.Band == Band5G {
		n.AP.Band = ""
	}
	if n.MeshMTU == 1560 {
		n.MeshMTU = 0
	}
	if n.Firewall.WAN == FirewallOpen {
		n.Firewall.WAN = ""
	}
	if n.Firewall.SSH != nil && *n.Firewall.SSH {
		n.Firewall.SSH = nil
	}
	if n.Gateway.Mode == GatewayAuto {
		n.Gateway.Mode = ""
	}
	if slices.Equal(n.Gateway.ProbeTargets, DefaultProbeTargets) {
		n.Gateway.ProbeTargets = nil
	}
	if n.Gateway.ProbeIntervalMS == 1000 {
		n.Gateway.ProbeIntervalMS = 0
	}
	if n.Gateway.DownMbit == 20 {
		n.Gateway.DownMbit = 0
	}
	if n.Gateway.UpMbit == 5 {
		n.Gateway.UpMbit = 0
	}
	return n
}

// YAML renders the node config as a file: a header, then only the settings
// that differ from the defaults.
func (n Node) YAML(header string) ([]byte, error) {
	body, err := yaml.Marshal(n.Sparse())
	if err != nil {
		return nil, err
	}
	return append(commentBlock(header), body...), nil
}

// YAML renders the profile as a file. Every field is written: a profile is
// what all nodes must agree on, so it never leans on one release's defaults.
func (p Profile) YAML(header string) ([]byte, error) {
	body, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(commentBlock(header), body...), nil
}

func commentBlock(text string) []byte {
	if text == "" {
		return nil
	}
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("# " + l + "\n")
	}
	return []byte(b.String())
}

// ParseNodeJSON decodes a node config sent to the API. Unknown fields are
// rejected; defaults and validation are applied as for a file.
func ParseNodeJSON(data []byte) (*Node, error) {
	var n Node
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return nil, Errors{{Field: "(document)", Msg: jsonProblem(err)}}
	}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return &n, nil
}

func jsonProblem(err error) string {
	return strings.TrimPrefix(err.Error(), "json: ")
}

// Profile codes ---------------------------------------------------------------

// ProfileCodePrefix starts a profile code: the mesh profile as one line of
// text, short enough for a QR code and safe to paste. It holds the mesh
// secret.
const ProfileCodePrefix = "TRP1:"

// maxProfileCode bounds what DecodeProfileCode inflates.
const maxProfileCode = 16 << 10

// profileWire is the profile inside a profile code: every field, under
// short keys, so the QR code stays small enough to scan from a phone
// screen. Nothing is left to defaults: the nodes of a mesh may run
// different releases.
type profileWire struct {
	Schema  int         `json:"s,omitempty"`
	Name    string      `json:"n"`
	MeshID  string      `json:"i"`
	Secret  string      `json:"k"`
	Mode    string      `json:"m"`
	Country string      `json:"c"`
	Radio   radioWire   `json:"r"`
	Radio2  *radioWire  `json:"r2,omitempty"` // older codes: one more link
	Links   []radioWire `json:"ls,omitempty"` // more links, in order
	Prefix  string      `json:"p"`
	Algo    string      `json:"a"`
	OrigMS  int         `json:"o"`
	MinVer  string      `json:"v"`
	PinVer  string      `json:"pv"`
	BLA     *bool       `json:"l,omitempty"`
	Fanout  *int        `json:"f,omitempty"`
	Flood   bool        `json:"mf,omitempty"` // only written when on, so older releases read codes without it
	// NoPos: positions.share is off. Only written when it is, so codes
	// for meshes that share positions still read on older releases.
	NoPos bool `json:"np,omitempty"`
}

type radioWire struct {
	Band    string `json:"b"`
	Channel int    `json:"c"`
	Width   int    `json:"w"`
}

// Code encodes the profile as a profile code: the prefix, then the
// base64url of the deflated compact JSON document.
func (p Profile) Code() (string, error) {
	w := profileWire{
		Schema: p.Schema, Name: p.Name, MeshID: p.MeshID, Secret: p.SAEPassphrase, Mode: p.Regulatory.Mode, Country: p.Regulatory.Country,
		Radio: radioWire(p.Radio), Prefix: p.Prefix, Algo: p.Batman.RoutingAlgo, OrigMS: p.Batman.OrigIntervalMS,
		MinVer: p.Batman.MinVersion, PinVer: p.Batman.PinnedVersion, BLA: p.Batman.BridgeLoopAvoidance, Fanout: p.Batman.MulticastFanout,
		Flood: p.Batman.MulticastFlood, NoPos: !p.Positions.Shared(),
	}
	// One more link travels as "r2", which nodes on older releases read too.
	switch {
	case len(p.Links) == 1:
		r2 := radioWire(p.Links[0])
		w.Radio2 = &r2
	case len(p.Links) > 1:
		for _, l := range p.Links {
			w.Links = append(w.Links, radioWire(l))
		}
	}
	js, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(js); err != nil {
		return "", err
	}
	if err := fw.Close(); err != nil {
		return "", err
	}
	return ProfileCodePrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// IsProfileCode reports whether text looks like a profile code.
func IsProfileCode(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), ProfileCodePrefix)
}

// DecodeProfileCode parses, defaults and validates a profile code.
func DecodeProfileCode(code string) (*Profile, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, ProfileCodePrefix) {
		return nil, fmt.Errorf("not a Trellis profile code (it starts with %q)", ProfileCodePrefix)
	}
	// Whitespace inside the code comes from line wrapping when it is
	// pasted from a message.
	b64 := strings.Join(strings.Fields(code[len(ProfileCodePrefix):]), "")
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(b64, "="))
	if err != nil {
		return nil, errors.New("the profile code is damaged (not valid base64); copy or scan it again")
	}
	js, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), maxProfileCode+1))
	if err != nil || len(js) > maxProfileCode {
		return nil, errors.New("the profile code is damaged or incomplete; copy or scan it again")
	}
	var w profileWire
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, errors.New("the profile code is damaged or from a newer Trellis release than this node runs")
	}
	p := &Profile{
		Schema: w.Schema, Name: w.Name, MeshID: w.MeshID, SAEPassphrase: w.Secret, Regulatory: Regulatory{Mode: w.Mode, Country: w.Country},
		Radio: RadioPlan(w.Radio), Prefix: w.Prefix,
		Batman: BatmanPlan{RoutingAlgo: w.Algo, OrigIntervalMS: w.OrigMS, MinVersion: w.MinVer, PinnedVersion: w.PinVer, BridgeLoopAvoidance: w.BLA, MulticastFanout: w.Fanout, MulticastFlood: w.Flood},
	}
	if w.NoPos {
		no := false
		p.Positions.Share = &no
	}
	if w.Radio2 != nil {
		p.Links = append(p.Links, RadioPlan(*w.Radio2))
	}
	for _, l := range w.Links {
		p.Links = append(p.Links, RadioPlan(l))
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}
