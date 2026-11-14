package nl80211

import (
	"bytes"
	"fmt"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// SurveyInfo is a driver's survey of one channel (NL80211_CMD_GET_SURVEY):
// how long the radio listened there and how much of that time the channel
// was busy. The counters add up from the radio's start; the channel in use
// grows all the time, others only while a scan visits them. Drivers that
// keep no survey (ath9k_htc, rt2800usb) return none.
type SurveyInfo struct {
	FreqMHz  uint32
	NoiseDBm int8
	HasNoise bool
	InUse    bool
	// Times in milliseconds: active (listening), busy (energy or frames
	// above the threshold), receiving, transmitting.
	ActiveMS, BusyMS, RxMS, TxMS uint64
}

// Survey dumps the survey of every channel the radio of an interface keeps.
func (c *Client) Survey(ifindex int) ([]SurveyInfo, error) {
	msgs, err := c.execute(NL80211_CMD_GET_SURVEY, netlink.Dump, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return nil, fmt.Errorf("survey on ifindex %d: %w", ifindex, err)
	}
	return decodeSurvey(msgs)
}

func decodeSurvey(msgs []genetlink.Message) ([]SurveyInfo, error) {
	var out []SurveyInfo
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		for ad.Next() {
			if ad.Type() != NL80211_ATTR_SURVEY_INFO {
				continue
			}
			var s SurveyInfo
			ad.Nested(func(sd *netlink.AttributeDecoder) error {
				for sd.Next() {
					switch sd.Type() {
					case NL80211_SURVEY_INFO_FREQUENCY:
						s.FreqMHz = sd.Uint32()
					case NL80211_SURVEY_INFO_NOISE:
						s.NoiseDBm, s.HasNoise = int8(sd.Uint8()), true
					case NL80211_SURVEY_INFO_IN_USE:
						s.InUse = true
					case NL80211_SURVEY_INFO_TIME:
						s.ActiveMS = sd.Uint64()
					case NL80211_SURVEY_INFO_TIME_BUSY:
						s.BusyMS = sd.Uint64()
					case NL80211_SURVEY_INFO_TIME_RX:
						s.RxMS = sd.Uint64()
					case NL80211_SURVEY_INFO_TIME_TX:
						s.TxMS = sd.Uint64()
					}
				}
				return nil
			})
			if s.FreqMHz != 0 {
				out = append(out, s)
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TriggerScan starts a passive scan of the given frequencies on an
// interface: the radio listens on each in turn without transmitting, then
// returns to its channel. Results arrive as a scan event (DialScanEvents).
func (c *Client) TriggerScan(ifindex int, freqs []uint32) error {
	_, err := c.execute(NL80211_CMD_TRIGGER_SCAN, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
		if len(freqs) > 0 {
			ae.Nested(NL80211_ATTR_SCAN_FREQUENCIES, func(ne *netlink.AttributeEncoder) error {
				for i, f := range freqs {
					ne.Uint32(uint16(i), f)
				}
				return nil
			})
		}
	})
	if err != nil {
		return fmt.Errorf("scan on ifindex %d: %w", ifindex, err)
	}
	return nil
}

// BSS is a network a scan heard.
type BSS struct {
	FreqMHz   uint32
	SignalDBm float64 // 0 when the driver gave none
	SSID      string
	MeshID    string // an 802.11s mesh's ID; empty for other networks
	// SeenMSAgo is how long ago the network was last heard.
	SeenMSAgo uint32
}

// Scan dumps the networks the radio of an interface has heard, from its
// latest scans.
func (c *Client) Scan(ifindex int) ([]BSS, error) {
	msgs, err := c.execute(NL80211_CMD_GET_SCAN, netlink.Dump, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return nil, fmt.Errorf("scan results on ifindex %d: %w", ifindex, err)
	}
	var out []BSS
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		for ad.Next() {
			if ad.Type() != NL80211_ATTR_BSS {
				continue
			}
			var b BSS
			ad.Nested(func(bd *netlink.AttributeDecoder) error {
				for bd.Next() {
					switch bd.Type() {
					case NL80211_BSS_FREQUENCY:
						b.FreqMHz = bd.Uint32()
					case NL80211_BSS_SIGNAL_MBM:
						b.SignalDBm = float64(int32(bd.Uint32())) / 100
					case NL80211_BSS_SEEN_MS_AGO:
						b.SeenMSAgo = bd.Uint32()
					case NL80211_BSS_INFORMATION_ELEMENTS:
						b.SSID, b.MeshID = ssidAndMeshID(bd.Bytes())
					}
				}
				return nil
			})
			if b.FreqMHz != 0 {
				out = append(out, b)
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ssidAndMeshID reads the SSID (element 0) and the mesh ID (element 114)
// from a frame's information elements.
func ssidAndMeshID(ies []byte) (ssid, meshID string) {
	for len(ies) >= 2 {
		id, n := ies[0], int(ies[1])
		if 2+n > len(ies) {
			break
		}
		v := ies[2 : 2+n]
		switch id {
		case 0:
			ssid = string(bytes.ToValidUTF8(v, []byte("?")))
		case 114:
			meshID = string(bytes.ToValidUTF8(v, []byte("?")))
		}
		ies = ies[2+n:]
	}
	return ssid, meshID
}

// DialScanEvents opens a connection subscribed to nl80211's scan group:
// NL80211_CMD_NEW_SCAN_RESULTS when a scan ends, NL80211_CMD_SCAN_ABORTED
// when it does not.
func DialScanEvents() (*EventConn, error) {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return nil, err
	}
	fam, err := c.GetFamily(FamilyName)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	for _, g := range fam.Groups {
		if g.Name == "scan" {
			if err := c.JoinGroup(g.ID); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("join nl80211 scan group: %w", err)
			}
			return &EventConn{c: c}, nil
		}
	}
	_ = c.Close()
	return nil, fmt.Errorf("nl80211 has no scan group")
}

// SetDeadline bounds the next Next.
func (e *EventConn) SetDeadline(t time.Time) error { return e.c.SetDeadline(t) }
