package node

import (
	"encoding/binary"
	"fmt"
)

// Codec names as used throughout the bridge.
const (
	CodecNameSBC    = "SBC"
	CodecNameAAC    = "AAC"
	CodecNameAptX   = "aptX"
	CodecNameAptXHD = "aptX HD"
	CodecNameLDAC   = "LDAC"
)

// CodecIDByName maps codec names to the IDs used in CmdSetCodecs.
var CodecIDByName = map[string]CodecID{
	CodecNameSBC: CodecIDSBC, CodecNameAAC: CodecIDAAC, CodecNameAptX: CodecIDAptX,
	CodecNameAptXHD: CodecIDAptXHD, CodecNameLDAC: CodecIDLDAC,
}

type vendorCodec struct {
	name   string
	vendor uint32
	codec  uint16
}

var vendorCodecs = []vendorCodec{
	{CodecNameAptX, 0x0000004F, 0x0001},
	{CodecNameAptXHD, 0x000000D7, 0x0024},
	{CodecNameLDAC, 0x0000012D, 0x00AA},
}

// CodecOf names the codec of raw codec information (capabilities or
// configuration), or returns "" for codecs the bridge does not know.
func CodecOf(info []byte) string {
	if len(info) < 3 || int(info[0])+1 > len(info) {
		return ""
	}
	switch info[2] {
	case 0x00:
		return CodecNameSBC
	case 0x02:
		return CodecNameAAC
	case 0xFF:
		if len(info) < 9 {
			return ""
		}
		vendor := binary.LittleEndian.Uint32(info[3:])
		codec := binary.LittleEndian.Uint16(info[7:])
		for _, v := range vendorCodecs {
			if v.vendor == vendor && v.codec == codec {
				return v.name
			}
		}
		return fmt.Sprintf("vendor %08X/%04X", vendor, codec)
	}
	return fmt.Sprintf("codec type %d", info[2])
}

// CodecConfig is a negotiated codec configuration.
type CodecConfig struct {
	Codec      string
	SampleRate int
	Channels   int
	// ChannelMode: SBC mono/dual/stereo/joint, LDAC mono/dual/stereo.
	ChannelMode string
	// SBC parameters.
	Blocks, Subbands       int
	Loudness               bool
	MinBitpool, MaxBitpool int
	// AAC parameters.
	MPEG2   bool // MPEG-2 AAC LC (else MPEG-4 AAC LC)
	Bitrate int  // AAC bit rate from the configuration (bit/s)
	VBR     bool
	Raw     []byte
}

func oneBit(v byte, table map[byte]int) (int, bool) {
	r, ok := table[v]
	return r, ok
}

// ParseCodecConfig decodes a raw configuration in which exactly one option
// per field is set.
func ParseCodecConfig(info []byte) (CodecConfig, error) {
	c := CodecConfig{Codec: CodecOf(info), Raw: append([]byte(nil), info...)}
	bad := func(what string) (CodecConfig, error) {
		return c, fmt.Errorf("%s configuration: ambiguous or unsupported %s", c.Codec, what)
	}
	switch c.Codec {
	case CodecNameSBC:
		if len(info) < 7 {
			return bad("length")
		}
		var ok bool
		if c.SampleRate, ok = oneBit(info[3]>>4, map[byte]int{8: 16000, 4: 32000, 2: 44100, 1: 48000}); !ok {
			return bad("sample rate")
		}
		modes := map[byte]string{8: "mono", 4: "dual", 2: "stereo", 1: "joint"}
		if c.ChannelMode, ok = modes[info[3]&0x0F]; !ok {
			return bad("channel mode")
		}
		c.Channels = 2
		if c.ChannelMode == "mono" {
			c.Channels = 1
		}
		if c.Blocks, ok = oneBit(info[4]>>4, map[byte]int{8: 4, 4: 8, 2: 12, 1: 16}); !ok {
			return bad("block length")
		}
		if c.Subbands, ok = oneBit((info[4]>>2)&3, map[byte]int{2: 4, 1: 8}); !ok {
			return bad("subbands")
		}
		switch info[4] & 3 {
		case 1:
			c.Loudness = true
		case 2:
		default:
			return bad("allocation")
		}
		c.MinBitpool, c.MaxBitpool = int(info[5]), int(info[6])
	case CodecNameAAC:
		if len(info) < 9 {
			return bad("length")
		}
		switch {
		case info[3]&0x80 != 0:
			c.MPEG2 = true
		case info[3]&0x40 != 0:
		default:
			return bad("object type")
		}
		switch {
		case info[5]&0x80 != 0:
			c.SampleRate = 48000
		case info[4]&0x01 != 0:
			c.SampleRate = 44100
		default:
			return bad("sample rate")
		}
		switch {
		case info[5]&0x04 != 0:
			c.Channels = 2
		case info[5]&0x08 != 0:
			c.Channels = 1
		default:
			return bad("channels")
		}
		c.VBR = info[6]&0x80 != 0
		c.Bitrate = int(info[6]&0x7F)<<16 | int(info[7])<<8 | int(info[8])
	case CodecNameLDAC:
		if len(info) < 11 {
			return bad("length")
		}
		var ok bool
		if c.SampleRate, ok = oneBit(info[9], map[byte]int{0x20: 44100, 0x10: 48000, 0x08: 88200, 0x04: 96000}); !ok {
			return bad("sample rate")
		}
		modes := map[byte]string{0x01: "stereo", 0x02: "dual", 0x04: "mono"}
		if c.ChannelMode, ok = modes[info[10]]; !ok {
			return bad("channel mode")
		}
		c.Channels = 2
		if c.ChannelMode == "mono" {
			c.Channels = 1
		}
	case CodecNameAptX, CodecNameAptXHD:
		if len(info) < 10 {
			return bad("length")
		}
		var ok bool
		if c.SampleRate, ok = oneBit(info[9]>>4, map[byte]int{8: 16000, 4: 32000, 2: 44100, 1: 48000}); !ok {
			return bad("sample rate")
		}
		if info[9]&0x0F != 0x02 {
			return bad("channel mode")
		}
		c.Channels = 2
	default:
		return c, fmt.Errorf("unsupported codec %q", c.Codec)
	}
	return c, nil
}

func (c CodecConfig) String() string {
	switch c.Codec {
	case CodecNameSBC:
		alloc := "snr"
		if c.Loudness {
			alloc = "loudness"
		}
		return fmt.Sprintf("SBC %d Hz %s, %d blocks, %d subbands, %s, bitpool %d..%d",
			c.SampleRate, c.ChannelMode, c.Blocks, c.Subbands, alloc, c.MinBitpool, c.MaxBitpool)
	case CodecNameAAC:
		ot := "MPEG-4"
		if c.MPEG2 {
			ot = "MPEG-2"
		}
		return fmt.Sprintf("AAC %s LC %d Hz %d ch, %d kbit/s", ot, c.SampleRate, c.Channels, c.Bitrate/1000)
	case CodecNameLDAC:
		return fmt.Sprintf("LDAC %d Hz %s", c.SampleRate, c.ChannelMode)
	default:
		return fmt.Sprintf("%s %d Hz", c.Codec, c.SampleRate)
	}
}
