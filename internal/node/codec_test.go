package node

import "testing"

// Configurations exactly as the node's codecs.cpp builds them.
func TestParseCodecConfig(t *testing.T) {
	tests := []struct {
		name  string
		info  []byte
		check func(CodecConfig) bool
	}{
		{"SBC 48k joint", []byte{6, 0x00, 0x00, 0x11, 0x15, 2, 53},
			func(c CodecConfig) bool {
				return c.Codec == CodecNameSBC && c.SampleRate == 48000 && c.ChannelMode == "joint" &&
					c.Blocks == 16 && c.Subbands == 8 && c.Loudness && c.MaxBitpool == 53 && c.Channels == 2
			}},
		{"AAC MPEG-2 48k stereo 320k", []byte{8, 0x00, 0x02, 0x80, 0x00, 0x84, 0x04, 0xE2, 0x00},
			func(c CodecConfig) bool {
				return c.Codec == CodecNameAAC && c.MPEG2 && c.SampleRate == 48000 && c.Channels == 2 &&
					c.Bitrate == 320000 && !c.VBR
			}},
		{"LDAC 48k stereo", []byte{10, 0x00, 0xFF, 0x2D, 0x01, 0x00, 0x00, 0xAA, 0x00, 0x10, 0x01},
			func(c CodecConfig) bool {
				return c.Codec == CodecNameLDAC && c.SampleRate == 48000 && c.ChannelMode == "stereo"
			}},
		{"aptX HD 44.1k", []byte{13, 0x00, 0xFF, 0xD7, 0x00, 0x00, 0x00, 0x24, 0x00, 0x22, 0, 0, 0, 0},
			func(c CodecConfig) bool { return c.Codec == CodecNameAptXHD && c.SampleRate == 44100 }},
		{"aptX 48k", []byte{9, 0x00, 0xFF, 0x4F, 0x00, 0x00, 0x00, 0x01, 0x00, 0x12},
			func(c CodecConfig) bool { return c.Codec == CodecNameAptX && c.SampleRate == 48000 }},
	}
	for _, tt := range tests {
		c, err := ParseCodecConfig(tt.info)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if !tt.check(c) {
			t.Errorf("%s: unexpected %+v", tt.name, c)
		}
	}
}

func TestParseCodecConfigRejectsCapabilities(t *testing.T) {
	// Capabilities have several bits per field and are not a configuration.
	if _, err := ParseCodecConfig([]byte{10, 0x00, 0xFF, 0x2D, 0x01, 0x00, 0x00, 0xAA, 0x00, 0x30, 0x07}); err == nil {
		t.Fatal("LDAC capabilities accepted as configuration")
	}
}

func TestCodecOf(t *testing.T) {
	if got := CodecOf([]byte{8, 0x00, 0xFF, 0x4F, 0x00, 0x00, 0x00, 0x01, 0x00}); got != CodecNameAptX {
		t.Errorf("got %q", got)
	}
	if got := CodecOf([]byte{8, 0x00, 0xFF, 0x75, 0x00, 0x00, 0x00, 0x02, 0x01}); got != "vendor 00000075/0102" {
		t.Errorf("unknown vendor codec named %q", got)
	}
	if got := CodecOf([]byte{9, 0x00}); got != "" {
		t.Errorf("truncated info named %q", got)
	}
}
