package sbc

/*
#include <stdlib.h>
#include "sbc.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Decoder decodes SBC frames to interleaved signed 16-bit little-endian PCM.
// The bridge only encodes; the decoder exists for tests and diagnostics.
type Decoder struct {
	sbc *C.sbc_t
}

// NewDecoder creates a decoder that takes its parameters from the frames.
func NewDecoder() (*Decoder, error) {
	st := (*C.sbc_t)(C.calloc(1, C.size_t(unsafe.Sizeof(C.sbc_t{}))))
	if st == nil {
		return nil, errors.New("sbc: out of memory")
	}
	if rc := C.sbc_init(st, 0); rc != 0 {
		C.free(unsafe.Pointer(st))
		return nil, fmt.Errorf("sbc: init failed (%d)", rc)
	}
	st.endian = C.SBC_LE
	return &Decoder{sbc: st}, nil
}

// Close releases the decoder.
func (d *Decoder) Close() {
	if d.sbc != nil {
		C.sbc_finish(d.sbc)
		C.free(unsafe.Pointer(d.sbc))
		d.sbc = nil
	}
}

// DecodeFrame decodes the first frame in src. It returns the PCM and the
// number of bytes of src consumed.
func (d *Decoder) DecodeFrame(src []byte) ([]byte, int, error) {
	if len(src) == 0 {
		return nil, 0, errors.New("sbc: empty input")
	}
	out := make([]byte, 16*8*2*2) // largest frame: 16 blocks, 8 subbands, 2 channels
	var written C.size_t
	n := C.sbc_decode(d.sbc, unsafe.Pointer(&src[0]), C.size_t(len(src)),
		unsafe.Pointer(&out[0]), C.size_t(len(out)), &written)
	if n <= 0 {
		return nil, 0, fmt.Errorf("sbc: decode failed (%d)", n)
	}
	return out[:written], int(n), nil
}
