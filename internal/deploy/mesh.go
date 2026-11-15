package deploy

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"meridian/trellis/internal/config"
)

// NewMeshDefaults: 2.4 GHz unless asked otherwise, because every Wi-Fi
// radio has it, while meshing on 5 GHz depends on the adapter.
var newMeshChannel = map[string]int{config.Band2G: 6, config.Band5G: 149}

const passphraseAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewMesh makes the profile of a new mesh: its name, a fresh secret, the
// country's rules, one link on band (2g or 5g) at 20 MHz. It is the file a
// run gives every node (--profile).
func NewMesh(name, country, band string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("give the mesh a name")
	}
	ch, ok := newMeshChannel[band]
	if !ok {
		return nil, fmt.Errorf("band %q: 2g or 5g (more links and bands can be added later, in the web UI)", band)
	}
	if len(country) != 2 {
		return nil, fmt.Errorf("--country takes a two-letter country code, as US, GB or DE")
	}
	var secret strings.Builder
	for i := 0; i < 24; i++ {
		if i > 0 && i%6 == 0 {
			secret.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passphraseAlphabet))))
		if err != nil {
			return nil, err
		}
		secret.WriteByte(passphraseAlphabet[n.Int64()])
	}
	p := config.Profile{
		Schema: config.SchemaVersion, Name: name, MeshID: "trellis-" + name, SAEPassphrase: secret.String(),
		Regulatory: config.Regulatory{Mode: config.RegModeCountry, Country: strings.ToUpper(country)},
		Radio:      config.RadioPlan{Band: band, Channel: ch, Width: 20},
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p.YAML("Trellis mesh profile, made by trellis-deploy --new-mesh. Every node of the\nmesh gets it. It holds the mesh's secret: keep it private.")
}
