package aptx

/*
#include <stdlib.h>
#include <freeaptx.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Decoder decodes aptX or aptX HD back to packed s24le stereo. The bridge only
// encodes; the decoder exists for tests.
type Decoder struct {
	ctx *C.struct_aptx_context
}

// NewDecoder creates a decoder.
func NewDecoder(hd bool) (*Decoder, error) {
	flag := C.int(0)
	if hd {
		flag = 1
	}
	ctx := C.aptx_init(flag)
	if ctx == nil {
		return nil, fmt.Errorf("aptx: init failed")
	}
	return &Decoder{ctx: ctx}, nil
}

// Close releases the decoder.
func (d *Decoder) Close() {
	if d.ctx != nil {
		C.aptx_finish(d.ctx)
		d.ctx = nil
	}
}

// Decode returns s24le stereo for the given codewords.
func (d *Decoder) Decode(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]byte, len(in)*6) // generous: 4 or 6 bytes become 24
	var written C.size_t
	used := C.aptx_decode(d.ctx, (*C.uchar)(unsafe.Pointer(&in[0])), C.size_t(len(in)),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &written)
	if int(used) != len(in) {
		return nil, fmt.Errorf("aptx: decoded %d of %d bytes", int(used), len(in))
	}
	return out[:written], nil
}
