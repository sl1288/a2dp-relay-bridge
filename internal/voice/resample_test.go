package voice

import (
	"encoding/binary"
	"math"
	"testing"
)

// 1 s at 22050 Hz becomes about 1 s at 16000 Hz, fed in uneven blocks, and a
// tone keeps its level.
func TestResampler(t *testing.T) {
	r := NewResampler(22050, 16000)
	var pcm []byte
	for i := range 22050 {
		v := 0.5 * math.Sin(2*math.Pi*440*float64(i)/22050)
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(int16(v*32767)))
	}
	var out []byte
	for off := 0; off < len(pcm); {
		n := min(2048+off%7*2, len(pcm)-off)
		out = append(out, r.Convert(pcm[off:off+n])...)
		off += n
	}
	if got := len(out) / 2; got < 15990 || got > 16000 {
		t.Fatalf("%d samples, want about 16000", got)
	}
	var e float64
	for i := 0; i < len(out); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(out[i:]))) / 32767
		e += s * s
	}
	if rms := math.Sqrt(e / float64(len(out)/2)); math.Abs(rms-0.3536) > 0.01 {
		t.Errorf("RMS %.4f, want 0.354", rms)
	}
}
