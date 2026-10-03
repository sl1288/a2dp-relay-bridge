// Package aptx wraps libfreeaptx (LGPL-2.1) for aptX and aptX HD encoding.
package aptx

/*
#cgo LDFLAGS: -lfreeaptx
#include <stdlib.h>
#include <freeaptx.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Encoder encodes stereo PCM: every 4 frames become 4 bytes (aptX) or 6 bytes (aptX HD).
type Encoder struct {
	ctx *C.struct_aptx_context
	hd  bool
	s24 []byte
	out []byte
}

// NewEncoder creates an aptX (hd = false) or aptX HD encoder.
func NewEncoder(hd bool) (*Encoder, error) {
	flag := C.int(0)
	if hd {
		flag = 1
	}
	ctx := C.aptx_init(flag)
	if ctx == nil {
		return nil, fmt.Errorf("aptx: init failed")
	}
	return &Encoder{ctx: ctx, hd: hd}, nil
}

// Close releases the encoder.
func (e *Encoder) Close() {
	if e.ctx != nil {
		C.aptx_finish(e.ctx)
		e.ctx = nil
	}
}

// CodewordBytes is the output size per 4 input frames.
func (e *Encoder) CodewordBytes() int {
	if e.hd {
		return 6
	}
	return 4
}

// Encode encodes interleaved stereo s16le PCM; the frame count must be a
// multiple of 4.
func (e *Encoder) Encode(pcm []byte) ([]byte, error) {
	frames := len(pcm) / 4
	if frames%4 != 0 || len(pcm)%4 != 0 {
		return nil, fmt.Errorf("aptx: %d bytes is not a multiple of 4 stereo frames", len(pcm))
	}
	if frames == 0 {
		return nil, nil
	}
	// libfreeaptx takes packed 24-bit samples: widen s16 by a zero low byte.
	need := frames * 6
	if cap(e.s24) < need {
		e.s24 = make([]byte, need)
	}
	s24 := e.s24[:need]
	for i := range frames * 2 {
		s24[i*3] = 0
		s24[i*3+1] = pcm[i*2]
		s24[i*3+2] = pcm[i*2+1]
	}
	outLen := frames / 4 * e.CodewordBytes()
	if cap(e.out) < outLen {
		e.out = make([]byte, outLen)
	}
	var written C.size_t
	used := C.aptx_encode(e.ctx, (*C.uchar)(unsafe.Pointer(&s24[0])), C.size_t(need),
		(*C.uchar)(unsafe.Pointer(&e.out[0])), C.size_t(outLen), &written)
	if int(used) != need || int(written) != outLen {
		return nil, fmt.Errorf("aptx: encoded %d of %d bytes into %d", int(used), need, int(written))
	}
	return append([]byte(nil), e.out[:written]...), nil
}
