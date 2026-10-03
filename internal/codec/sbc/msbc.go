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

// mSBC is the wide band speech codec of the hands-free profile: 16 kHz mono,
// 120 samples (7.5 ms) per 57-byte frame.
const (
	MSBCFrameLen     = 57
	MSBCFrameSamples = 120
	MSBCSampleRate   = 16000
)

// MSBC encodes and decodes mSBC frames (signed 16-bit little-endian PCM).
type MSBC struct {
	enc, dec *C.sbc_t
}

func newMSBCState() (*C.sbc_t, error) {
	st := (*C.sbc_t)(C.calloc(1, C.size_t(unsafe.Sizeof(C.sbc_t{}))))
	if st == nil {
		return nil, errors.New("sbc: out of memory")
	}
	if rc := C.sbc_init_msbc(st, 0); rc != 0 {
		C.free(unsafe.Pointer(st))
		return nil, fmt.Errorf("sbc: mSBC init failed (%d)", rc)
	}
	st.endian = C.SBC_LE
	return st, nil
}

// NewMSBC creates an mSBC codec.
func NewMSBC() (*MSBC, error) {
	enc, err := newMSBCState()
	if err != nil {
		return nil, err
	}
	dec, err := newMSBCState()
	if err != nil {
		C.sbc_finish(enc)
		C.free(unsafe.Pointer(enc))
		return nil, err
	}
	return &MSBC{enc: enc, dec: dec}, nil
}

// Close releases the codec.
func (m *MSBC) Close() {
	for _, st := range []**C.sbc_t{&m.enc, &m.dec} {
		if *st != nil {
			C.sbc_finish(*st)
			C.free(unsafe.Pointer(*st))
			*st = nil
		}
	}
}

// Decode decodes one 57-byte frame into 120 samples.
func (m *MSBC) Decode(frame []byte) ([]byte, error) {
	if len(frame) < MSBCFrameLen {
		return nil, errors.New("sbc: short mSBC frame")
	}
	out := make([]byte, MSBCFrameSamples*2)
	var written C.size_t
	n := C.sbc_decode(m.dec, unsafe.Pointer(&frame[0]), C.size_t(MSBCFrameLen),
		unsafe.Pointer(&out[0]), C.size_t(len(out)), &written)
	if n <= 0 {
		return nil, fmt.Errorf("sbc: mSBC decode failed (%d)", n)
	}
	return out[:written], nil
}

// Encode encodes 120 samples (240 bytes) into one 57-byte frame.
func (m *MSBC) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) != MSBCFrameSamples*2 {
		return nil, fmt.Errorf("sbc: mSBC needs %d bytes of PCM, got %d", MSBCFrameSamples*2, len(pcm))
	}
	out := make([]byte, MSBCFrameLen)
	var written C.ssize_t
	n := C.sbc_encode(m.enc, unsafe.Pointer(&pcm[0]), C.size_t(len(pcm)),
		unsafe.Pointer(&out[0]), C.size_t(len(out)), &written)
	if n <= 0 || written != MSBCFrameLen {
		return nil, fmt.Errorf("sbc: mSBC encode failed (%d, %d bytes)", n, written)
	}
	return out, nil
}
