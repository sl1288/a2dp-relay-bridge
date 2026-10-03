package sbc

import (
	"encoding/binary"
	"math"
	"testing"
)

// A 1 kHz tone survives encoding and decoding with mSBC.
func TestMSBCRoundTrip(t *testing.T) {
	m, err := NewMSBC()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var energy float64
	for f := range 20 {
		pcm := make([]byte, MSBCFrameSamples*2)
		for i := range MSBCFrameSamples {
			n := f*MSBCFrameSamples + i
			v := 0.5 * math.Sin(2*math.Pi*1000*float64(n)/MSBCSampleRate)
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v*32767)))
		}
		frame, err := m.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) != MSBCFrameLen || frame[0] != 0xAD {
			t.Fatalf("frame of %d bytes starting with %#x", len(frame), frame[0])
		}
		out, err := m.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != MSBCFrameSamples*2 {
			t.Fatalf("decoded %d bytes", len(out))
		}
		if f >= 10 { // after the filter delay
			for i := 0; i < len(out); i += 2 {
				s := float64(int16(binary.LittleEndian.Uint16(out[i:]))) / 32767
				energy += s * s
			}
		}
	}
	if rms := math.Sqrt(energy / (10 * MSBCFrameSamples)); rms < 0.25 || rms > 0.45 {
		t.Errorf("decoded RMS %.3f, want about 0.35", rms)
	}
}
