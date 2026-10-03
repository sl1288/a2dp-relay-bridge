package stream

import (
	"fmt"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/aac"
	"github.com/sl1288/a2dp-relay-bridge/internal/codec/aptx"
	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// Packetizer turns PCM into A2DP media packets for one negotiated codec.
type Packetizer interface {
	// Write adds interleaved s16le PCM and returns the packets that became complete.
	Write(pcm []byte) ([]Packet, error)
	// Reset drops buffered PCM, e.g. on a seek or track change.
	Reset()
	Close()
	// Bitrate returns the current bit rate in bit/s.
	Bitrate() int
	// Describe names codec and parameters for logs and the web interface.
	Describe() string
}

// Options tune the encoders.
type Options struct {
	SBCMaxBitpool int
	AACMaxBitrate int
	LDACQuality   ldac.Quality
}

// New creates the packetizer for a negotiated configuration. mtu is the
// largest media payload the node accepts (RTP header excluded).
func New(cfg node.CodecConfig, mtu int, opt Options) (Packetizer, error) {
	switch cfg.Codec {
	case node.CodecNameSBC:
		return NewSBCPacketizer(SBCEncoderConfig(cfg, opt.SBCMaxBitpool), mtu)
	case node.CodecNameAAC:
		return newAACPacketizer(cfg, mtu, opt)
	case node.CodecNameLDAC:
		return newLDACPacketizer(cfg, mtu, opt)
	case node.CodecNameAptX, node.CodecNameAptXHD:
		return newAptXPacketizer(cfg, mtu)
	}
	return nil, fmt.Errorf("stream: no encoder for %s", cfg.Codec)
}

func samplesDuration(samples, rate int) time.Duration {
	return time.Duration(samples) * time.Second / time.Duration(rate)
}

// --- AAC ---------------------------------------------------------------------

type aacPacketizer struct {
	enc     *aac.Encoder
	cfg     node.CodecConfig
	bitrate int
	pending []byte
	ts      uint32
	mtu     int
}

func newAACPacketizer(cfg node.CodecConfig, mtu int, opt Options) (*aacPacketizer, error) {
	bitrate := opt.AACMaxBitrate
	if cfg.Bitrate > 0 && cfg.Bitrate < bitrate {
		bitrate = cfg.Bitrate
	}
	// One AudioMuxElement per packet: cap each frame below the MTU, keeping
	// room for the LATM framing around the raw AAC data.
	peak := (mtu - 16) * 8 * cfg.SampleRate / 1024
	bitrate = min(bitrate, peak)
	enc, err := aac.NewEncoder(aac.Config{SampleRate: cfg.SampleRate, Channels: cfg.Channels, Bitrate: bitrate,
		PeakBitrate: peak, MPEG2: cfg.MPEG2})
	if err != nil {
		return nil, err
	}
	return &aacPacketizer{enc: enc, cfg: cfg, bitrate: bitrate, mtu: mtu}, nil
}

func (p *aacPacketizer) Write(pcm []byte) ([]Packet, error) {
	p.pending = append(p.pending, pcm...)
	fb := p.enc.FrameBytes()
	var out []Packet
	for len(p.pending) >= fb {
		data, err := p.enc.Encode(p.pending[:fb])
		p.pending = p.pending[fb:]
		if err != nil {
			return out, err
		}
		ts := p.ts
		p.ts += uint32(p.enc.FrameSamples())
		if len(data) == 0 {
			continue // encoder look-ahead
		}
		if len(data) > p.mtu {
			return out, fmt.Errorf("stream: AAC frame of %d bytes exceeds the MTU of %d", len(data), p.mtu)
		}
		out = append(out, Packet{Timestamp: ts, Duration: samplesDuration(p.enc.FrameSamples(), p.cfg.SampleRate),
			Frames: 1, Data: data})
	}
	p.pending = compact(p.pending)
	return out, nil
}

func (p *aacPacketizer) Reset()           { p.pending = p.pending[:0] }
func (p *aacPacketizer) Close()           { p.enc.Close() }
func (p *aacPacketizer) Bitrate() int     { return p.bitrate }
func (p *aacPacketizer) Describe() string { return p.cfg.String() }

// --- LDAC --------------------------------------------------------------------

type ldacPacketizer struct {
	enc     *ldac.Encoder
	cfg     node.CodecConfig
	pending []byte
	ts      uint32
}

func newLDACPacketizer(cfg node.CodecConfig, mtu int, opt Options) (*ldacPacketizer, error) {
	cm := map[string]int{"stereo": 0x01, "dual": 0x02, "mono": 0x04}[cfg.ChannelMode]
	// libldac sizes packets for the AVDTP MTU including RTP (12 bytes) and
	// the LDAC payload header (1 byte); the node reports the payload size.
	enc, err := ldac.NewEncoder(ldac.Config{MTU: mtu + 13, Quality: opt.LDACQuality, ChannelMode: cm,
		SampleRate: cfg.SampleRate})
	if err != nil {
		return nil, err
	}
	return &ldacPacketizer{enc: enc, cfg: cfg}, nil
}

func (p *ldacPacketizer) Write(pcm []byte) ([]Packet, error) {
	p.pending = append(p.pending, pcm...)
	fb := p.enc.FrameBytes()
	var out []Packet
	for len(p.pending) >= fb {
		payload, frames, err := p.enc.Encode(p.pending[:fb])
		p.pending = p.pending[fb:]
		if err != nil {
			return out, err
		}
		if payload == nil {
			continue
		}
		if frames > 15 {
			return out, fmt.Errorf("stream: %d LDAC frames do not fit the payload header", frames)
		}
		// Payload header as for SBC: fragmentation bits clear, frame count.
		data := append([]byte{byte(frames)}, payload...)
		out = append(out, Packet{Timestamp: p.ts, Duration: samplesDuration(frames*ldac.FrameSamples, p.cfg.SampleRate),
			Frames: uint16(frames), Data: data})
		p.ts += uint32(frames * ldac.FrameSamples)
	}
	p.pending = compact(p.pending)
	return out, nil
}

func (p *ldacPacketizer) Reset()       { p.pending = p.pending[:0] }
func (p *ldacPacketizer) Close()       { p.enc.Close() }
func (p *ldacPacketizer) Bitrate() int { return p.enc.Bitrate() }
func (p *ldacPacketizer) Describe() string {
	return fmt.Sprintf("%s %s", p.cfg.String(), p.enc.Quality())
}

// SetLDACQuality changes the quality if p encodes LDAC.
func SetLDACQuality(p Packetizer, q ldac.Quality) error {
	if lp, ok := p.(*ldacPacketizer); ok {
		return lp.enc.SetQuality(q)
	}
	return nil
}

// LDACQuality returns the current quality if p encodes LDAC.
func LDACQuality(p Packetizer) (ldac.Quality, bool) {
	if lp, ok := p.(*ldacPacketizer); ok {
		return lp.enc.Quality(), true
	}
	return 0, false
}

// --- aptX / aptX HD ----------------------------------------------------------

type aptxPacketizer struct {
	enc       *aptx.Encoder
	cfg       node.CodecConfig
	frames    int // PCM frames per packet
	pending   []byte
	ts        uint32
	codewords int
}

func newAptXPacketizer(cfg node.CodecConfig, mtu int) (*aptxPacketizer, error) {
	if cfg.Channels != 2 {
		return nil, fmt.Errorf("stream: aptX needs stereo")
	}
	enc, err := aptx.NewEncoder(cfg.Codec == node.CodecNameAptXHD)
	if err != nil {
		return nil, err
	}
	codewords := mtu / enc.CodewordBytes()
	if codewords < 1 {
		enc.Close()
		return nil, fmt.Errorf("stream: MTU %d too small for aptX", mtu)
	}
	return &aptxPacketizer{enc: enc, cfg: cfg, frames: codewords * 4, codewords: codewords}, nil
}

func (p *aptxPacketizer) Write(pcm []byte) ([]Packet, error) {
	p.pending = append(p.pending, pcm...)
	need := p.frames * 4 // stereo s16 bytes
	var out []Packet
	for len(p.pending) >= need {
		data, err := p.enc.Encode(p.pending[:need])
		p.pending = p.pending[need:]
		if err != nil {
			return out, err
		}
		out = append(out, Packet{Timestamp: p.ts, Duration: samplesDuration(p.frames, p.cfg.SampleRate),
			Frames: uint16(p.codewords), Data: data})
		p.ts += uint32(p.frames)
	}
	p.pending = compact(p.pending)
	return out, nil
}

func (p *aptxPacketizer) Reset() { p.pending = p.pending[:0] }
func (p *aptxPacketizer) Close() { p.enc.Close() }
func (p *aptxPacketizer) Bitrate() int {
	return p.enc.CodewordBytes() * 8 * p.cfg.SampleRate / 4
}
func (p *aptxPacketizer) Describe() string { return p.cfg.String() }

// compact keeps a pending buffer's backing array from growing without bound.
func compact(b []byte) []byte {
	if cap(b) > 64<<10 && len(b) < 8<<10 {
		return append([]byte(nil), b...)
	}
	return b
}
