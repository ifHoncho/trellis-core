package nl80211

import (
	"testing"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

func surveyMsg(t *testing.T, enc func(*netlink.AttributeEncoder)) genetlink.Message {
	t.Helper()
	ae := netlink.NewAttributeEncoder()
	ae.Uint32(NL80211_ATTR_IFINDEX, 7)
	ae.Nested(NL80211_ATTR_SURVEY_INFO, func(ne *netlink.AttributeEncoder) error { enc(ne); return nil })
	data, err := ae.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return genetlink.Message{Data: data}
}

// As mt7915e reports it: the channel in use with noise and times, and a
// channel a scan visited.
func TestDecodeSurvey(t *testing.T) {
	msgs := []genetlink.Message{
		surveyMsg(t, func(e *netlink.AttributeEncoder) {
			e.Uint32(NL80211_SURVEY_INFO_FREQUENCY, 2437)
			e.Uint8(NL80211_SURVEY_INFO_NOISE, uint8(0x100-92))
			e.Flag(NL80211_SURVEY_INFO_IN_USE, true)
			e.Uint64(NL80211_SURVEY_INFO_TIME, 2353187)
			e.Uint64(NL80211_SURVEY_INFO_TIME_BUSY, 296673)
			e.Uint64(NL80211_SURVEY_INFO_TIME_RX, 227168)
			e.Uint64(NL80211_SURVEY_INFO_TIME_TX, 67999)
		}),
		surveyMsg(t, func(e *netlink.AttributeEncoder) {
			e.Uint32(NL80211_SURVEY_INFO_FREQUENCY, 2442)
			e.Uint64(NL80211_SURVEY_INFO_TIME, 64)
			e.Uint64(NL80211_SURVEY_INFO_TIME_BUSY, 25)
		}),
	}
	got, err := decodeSurvey(msgs)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if s := got[0]; s.FreqMHz != 2437 || !s.InUse || !s.HasNoise || s.NoiseDBm != -92 || s.ActiveMS != 2353187 || s.BusyMS != 296673 || s.RxMS != 227168 || s.TxMS != 67999 {
		t.Errorf("in use: %+v", s)
	}
	if s := got[1]; s.FreqMHz != 2442 || s.InUse || s.HasNoise || s.ActiveMS != 64 || s.BusyMS != 25 {
		t.Errorf("scanned: %+v", s)
	}
}

func TestSSIDAndMeshID(t *testing.T) {
	ies := []byte{0, 4, 'c', 'a', 'f', 'e', 1, 2, 0x82, 0x84, 114, 13}
	ies = append(ies, "trellis-alpha"...)
	if ssid, mesh := ssidAndMeshID(ies); ssid != "cafe" || mesh != "trellis-alpha" {
		t.Errorf("%q %q", ssid, mesh)
	}
	if ssid, mesh := ssidAndMeshID([]byte{0, 9, 'x'}); ssid != "" || mesh != "" {
		t.Errorf("truncated: %q %q", ssid, mesh)
	}
}
