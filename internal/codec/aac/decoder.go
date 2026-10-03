package aac

/*
#cgo LDFLAGS: -lfdk-aac
#include <fdk-aac/aacdecoder_lib.h>

// aacDecoder_Fill takes pointers to pointers, which cgo forbids for Go memory.
static AAC_DECODER_ERROR fill(HANDLE_AACDECODER h, UCHAR *data, UINT size) {
	UCHAR *buf = data;
	UINT valid = size;
	return aacDecoder_Fill(h, &buf, &size, &valid);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Decoder decodes LATM AudioMuxElements as sent over A2DP. The bridge only
// encodes; the decoder exists for tests.
type Decoder struct {
	h   C.HANDLE_AACDECODER
	pcm []C.INT_PCM
}

// NewDecoder opens a LATM (with StreamMuxConfig) decoder.
func NewDecoder() (*Decoder, error) {
	h := C.aacDecoder_Open(C.TT_MP4_LATM_MCP1, 1)
	if h == nil {
		return nil, fmt.Errorf("aac: decoder open failed")
	}
	return &Decoder{h: h, pcm: make([]C.INT_PCM, 2048*8)}, nil
}

// Close releases the decoder.
func (d *Decoder) Close() {
	if d.h != nil {
		C.aacDecoder_Close(d.h)
		d.h = nil
	}
}

// Decode decodes one AudioMuxElement and returns interleaved s16 samples.
func (d *Decoder) Decode(elem []byte) ([]int16, error) {
	if len(elem) == 0 {
		return nil, nil
	}
	if err := C.fill(d.h, (*C.UCHAR)(unsafe.Pointer(&elem[0])), C.UINT(len(elem))); err != C.AAC_DEC_OK {
		return nil, fmt.Errorf("aac: fill failed (0x%x)", int(err))
	}
	err := C.aacDecoder_DecodeFrame(d.h, &d.pcm[0], C.INT(len(d.pcm)), 0)
	if err == C.AAC_DEC_NOT_ENOUGH_BITS {
		return nil, nil
	}
	if err != C.AAC_DEC_OK {
		return nil, fmt.Errorf("aac: decode failed (0x%x)", int(err))
	}
	info := C.aacDecoder_GetStreamInfo(d.h)
	n := int(info.frameSize) * int(info.numChannels)
	out := make([]int16, n)
	for i := range n {
		out[i] = int16(d.pcm[i])
	}
	return out, nil
}

// StreamInfo returns sample rate and channel count of the decoded stream.
func (d *Decoder) StreamInfo() (rate, channels int) {
	info := C.aacDecoder_GetStreamInfo(d.h)
	return int(info.sampleRate), int(info.numChannels)
}
