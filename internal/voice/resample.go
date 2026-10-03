package voice

import "encoding/binary"

// Resampler converts mono s16le PCM between sample rates by linear
// interpolation, keeping its position across calls. Good enough for speech.
type Resampler struct {
	from, to int
	pos      float64 // position of the next output sample in input samples
	last     int16   // last input sample of the previous call
	started  bool
}

// NewResampler converts from one rate to another.
func NewResampler(from, to int) *Resampler { return &Resampler{from: from, to: to} }

// Convert resamples a block (an even number of bytes).
func (r *Resampler) Convert(pcm []byte) []byte {
	n := len(pcm) / 2
	if n == 0 {
		return nil
	}
	if r.from == r.to {
		return append([]byte(nil), pcm[:n*2]...)
	}
	in := func(i int) float64 {
		if i < 0 {
			return float64(r.last)
		}
		return float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	if !r.started {
		r.started = true
		r.last = int16(binary.LittleEndian.Uint16(pcm))
	}
	step := float64(r.from) / float64(r.to)
	var out []byte
	// Input index -1 is the last sample of the previous block.
	for r.pos < float64(n-1) {
		i := int(r.pos)
		if r.pos < 0 {
			i = -1
		}
		frac := r.pos - float64(i)
		v := in(i) + (in(i+1)-in(i))*frac
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(v)))
		r.pos += step
	}
	r.pos -= float64(n)
	r.last = int16(binary.LittleEndian.Uint16(pcm[(n-1)*2:]))
	return out
}
