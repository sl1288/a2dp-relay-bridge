package stream

import (
	"fmt"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/sbc"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// Packet is one A2DP media packet ready for a node.
type Packet struct {
	Timestamp uint32 // RTP timestamp: samples per channel since stream start
	Duration  time.Duration
	Frames    uint16
	Data      []byte
}

// SBCPacketizer encodes PCM into SBC and groups frames into packets that fit
// the link's audio MTU.
type SBCPacketizer struct {
	enc       *sbc.Encoder
	perPacket int
	pending   []byte // PCM not yet encoded
	timestamp uint32
	frameDur  time.Duration
}

// SBCEncoderConfig derives encoder settings from the negotiated configuration.
// The bitpool is the negotiated maximum, capped at maxBitpool.
func SBCEncoderConfig(cfg node.CodecConfig, maxBitpool int) sbc.Config {
	mode := map[string]sbc.Mode{"mono": sbc.Mono, "dual": sbc.DualChannel, "stereo": sbc.Stereo, "joint": sbc.JointStereo}[cfg.ChannelMode]
	bitpool := min(cfg.MaxBitpool, maxBitpool)
	return sbc.Config{SampleRate: cfg.SampleRate, Mode: mode, Blocks: cfg.Blocks, Subbands: cfg.Subbands,
		Loudness: cfg.Loudness, Bitpool: max(bitpool, cfg.MinBitpool)}
}

// NewSBCPacketizer creates a packetizer for the negotiated configuration.
func NewSBCPacketizer(cfg sbc.Config, mtu int) (*SBCPacketizer, error) {
	enc, err := sbc.NewEncoder(cfg)
	if err != nil {
		return nil, err
	}
	// The SBC media payload header counts frames in 4 bits.
	perPacket := min(mtu/enc.FrameLength(), 15)
	if perPacket < 1 {
		enc.Close()
		return nil, fmt.Errorf("stream: MTU %d too small for %d-byte SBC frames", mtu, enc.FrameLength())
	}
	return &SBCPacketizer{
		enc:       enc,
		perPacket: perPacket,
		frameDur:  time.Duration(enc.SamplesPerFrame()) * time.Second / time.Duration(cfg.SampleRate),
	}, nil
}

// Close releases the encoder.
func (s *SBCPacketizer) Close() { s.enc.Close() }

// Encoder returns the underlying encoder.
func (s *SBCPacketizer) Encoder() *sbc.Encoder { return s.enc }

// FramesPerPacket returns how many SBC frames go into one packet.
func (s *SBCPacketizer) FramesPerPacket() int { return s.perPacket }

// PacketDuration returns the audio duration of a full packet.
func (s *SBCPacketizer) PacketDuration() time.Duration {
	return time.Duration(s.perPacket) * s.frameDur
}

// Bitrate returns the encoded bit rate in bit/s.
func (s *SBCPacketizer) Bitrate() int { return s.enc.Bitrate() }

// Describe names the codec parameters.
func (s *SBCPacketizer) Describe() string {
	c := s.enc.Config()
	return fmt.Sprintf("SBC %d Hz bitpool %d, %d frames/packet", c.SampleRate, c.Bitpool, s.perPacket)
}

// Write adds interleaved s16le PCM and returns the packets that became complete.
func (s *SBCPacketizer) Write(pcm []byte) ([]Packet, error) {
	s.pending = append(s.pending, pcm...)
	code := s.enc.CodeSize()
	var out []Packet
	for len(s.pending) >= code*s.perPacket {
		data := make([]byte, 0, s.perPacket*s.enc.FrameLength())
		var err error
		for f := range s.perPacket {
			data, err = s.enc.EncodeFrame(data, s.pending[f*code:(f+1)*code])
			if err != nil {
				return out, err
			}
		}
		s.pending = s.pending[code*s.perPacket:]
		out = append(out, Packet{
			Timestamp: s.timestamp,
			Duration:  time.Duration(s.perPacket) * s.frameDur,
			Frames:    uint16(s.perPacket),
			Data:      data,
		})
		s.timestamp += uint32(s.perPacket * s.enc.SamplesPerFrame())
	}
	s.pending = compact(s.pending)
	return out, nil
}

// Reset drops buffered PCM, e.g. on a seek or track change.
func (s *SBCPacketizer) Reset() { s.pending = s.pending[:0] }
