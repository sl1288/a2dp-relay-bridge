package sbc

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestRoundTrip encodes a stereo sine with a configuration headphones commonly
// negotiate (48 kHz joint stereo, 16 blocks, 8 subbands, loudness, bitpool
// 53), decodes it again and checks frame size, bit rate and signal quality.
func TestRoundTrip(t *testing.T) {
	enc, err := NewEncoder(Config{SampleRate: 48000, Mode: JointStereo, Blocks: 16, Subbands: 8, Loudness: true, Bitpool: 53})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if enc.FrameLength() != 119 || enc.CodeSize() != 512 || enc.SamplesPerFrame() != 128 {
		t.Fatalf("frame length %d, code size %d, samples %d", enc.FrameLength(), enc.CodeSize(), enc.SamplesPerFrame())
	}
	if br := enc.Bitrate(); br != 357000 {
		t.Fatalf("bitrate %d", br)
	}

	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	const frames = 400
	in := make([]int16, 0, frames*128*2)
	for i := range frames * 128 {
		l := 12000 * math.Sin(2*math.Pi*440*float64(i)/48000)
		r := 12000 * math.Sin(2*math.Pi*1000*float64(i)/48000)
		in = append(in, int16(l), int16(r))
	}
	pcm := make([]byte, len(in)*2)
	for i, s := range in {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}

	var stream []byte
	for f := range frames {
		stream, err = enc.EncodeFrame(stream, pcm[f*512:(f+1)*512])
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(stream) != frames*119 {
		t.Fatalf("stream length %d", len(stream))
	}

	var out []int16
	for off := 0; off < len(stream); {
		p, n, err := dec.DecodeFrame(stream[off:])
		if err != nil {
			t.Fatal(err)
		}
		off += n
		for i := 0; i+1 < len(p); i += 2 {
			out = append(out, int16(binary.LittleEndian.Uint16(p[i:])))
		}
	}
	if len(out) != len(in) {
		t.Fatalf("decoded %d samples, want %d", len(out), len(in))
	}

	// The SBC filter bank delays the signal; find the delay by correlation,
	// then measure the signal-to-noise ratio of the aligned signals.
	best, bestDelay := -1.0, 0
	for d := 0; d < 400; d += 2 {
		var c float64
		for i := 20000; i < 30000; i++ {
			c += float64(in[i]) * float64(out[i+d])
		}
		if c > best {
			best, bestDelay = c, d
		}
	}
	var sig, noise float64
	for i := 20000; i < len(in)-bestDelay-1000; i++ {
		s := float64(in[i])
		e := float64(out[i+bestDelay]) - s
		sig += s * s
		noise += e * e
	}
	snr := 10 * math.Log10(sig/noise)
	t.Logf("delay %d samples, SNR %.1f dB", bestDelay/2, snr)
	if snr < 30 {
		t.Fatalf("SNR %.1f dB too low", snr)
	}
}
