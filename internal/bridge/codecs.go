package bridge

import (
	"strings"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// defaultCodecOrder is used when a headphone has no codec preference: the
// best codec both sides support wins.
var defaultCodecOrder = []string{node.CodecNameLDAC, node.CodecNameAptXHD, node.CodecNameAptX, node.CodecNameAAC, node.CodecNameSBC}

// CodecChoices lists the values accepted as headphone codec preference.
var CodecChoices = append([]string{"auto"}, defaultCodecOrder...)

// codecPrefs builds the node codec preference for a headphone.
func (b *Bridge) codecPrefs(h config.Headphone) node.CodecPrefs {
	order := defaultCodecOrder
	if want := h.Codec; want != "" && want != "auto" {
		order = []string{want}
		if want != node.CodecNameSBC {
			order = append(order, node.CodecNameSBC)
		}
	}
	p := node.CodecPrefs{
		Prefer48k:     true,
		SBCMaxBitpool: uint8(b.cfg.Streaming.SBCMaxBitpool),
		AACMaxBitrate: uint32(b.cfg.Streaming.AACMaxBitrate),
	}
	for _, name := range order {
		if id, ok := node.CodecIDByName[name]; ok {
			p.Order = append(p.Order, id)
		}
	}
	return p
}

// ldacQuality returns the starting quality and whether it adapts to the link.
func ldacQuality(setting string) (ldac.Quality, bool) {
	switch strings.ToLower(setting) {
	case "hq":
		return ldac.HQ, false
	case "sq":
		return ldac.SQ, false
	case "mq":
		return ldac.MQ, false
	}
	return ldac.SQ, true
}
