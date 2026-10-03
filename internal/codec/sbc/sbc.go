// Package sbc wraps the BlueZ SBC encoder (libsbc 2.2, LGPL-2.1, vendored in
// libsbc/) for producing A2DP SBC frames.
package sbc

/*
#cgo CFLAGS: -I${SRCDIR} -I${SRCDIR}/libsbc -O2 -Wno-unused-parameter
#include <stdlib.h>
#include "sbc.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"slices"
	"unsafe"
)

// Mode is the SBC channel mode.
type Mode int

const (
	Mono Mode = iota
	DualChannel
	Stereo
	JointStereo
)

// Config selects the encoder parameters. They must match the configuration
// negotiated with the headphone; Bitpool may be anything up to its maximum.
type Config struct {
	SampleRate int // 16000, 32000, 44100 or 48000
	Mode       Mode
	Blocks     int // 4, 8, 12 or 16
	Subbands   int // 4 or 8
	Loudness   bool
	Bitpool    int
}

// Encoder encodes interleaved signed 16-bit little-endian PCM into SBC frames.
type Encoder struct {
	sbc      *C.sbc_t
	cfg      Config
	codeSize int
	frameLen int
	channels int
	perFrame int // samples per channel per frame
}

// NewEncoder creates an encoder; call Close when done.
func NewEncoder(cfg Config) (*Encoder, error) {
	freq := map[int]C.uint8_t{16000: C.SBC_FREQ_16000, 32000: C.SBC_FREQ_32000, 44100: C.SBC_FREQ_44100, 48000: C.SBC_FREQ_48000}
	blocks := map[int]C.uint8_t{4: C.SBC_BLK_4, 8: C.SBC_BLK_8, 12: C.SBC_BLK_12, 16: C.SBC_BLK_16}
	subbands := map[int]C.uint8_t{4: C.SBC_SB_4, 8: C.SBC_SB_8}
	f, ok1 := freq[cfg.SampleRate]
	b, ok2 := blocks[cfg.Blocks]
	s, ok3 := subbands[cfg.Subbands]
	if !ok1 || !ok2 || !ok3 || cfg.Mode < Mono || cfg.Mode > JointStereo {
		return nil, fmt.Errorf("sbc: unsupported configuration %+v", cfg)
	}
	if cfg.Bitpool < 2 || cfg.Bitpool > 250 {
		return nil, fmt.Errorf("sbc: bitpool %d out of range", cfg.Bitpool)
	}

	// The state lives in C memory: libsbc keeps internal pointers into it.
	st := (*C.sbc_t)(C.calloc(1, C.size_t(unsafe.Sizeof(C.sbc_t{}))))
	if st == nil {
		return nil, errors.New("sbc: out of memory")
	}
	if rc := C.sbc_init(st, 0); rc != 0 {
		C.free(unsafe.Pointer(st))
		return nil, fmt.Errorf("sbc: init failed (%d)", rc)
	}
	st.frequency = f
	st.blocks = b
	st.subbands = s
	st.mode = C.uint8_t(cfg.Mode)
	st.allocation = C.SBC_AM_SNR
	if cfg.Loudness {
		st.allocation = C.SBC_AM_LOUDNESS
	}
	st.bitpool = C.uint8_t(cfg.Bitpool)
	st.endian = C.SBC_LE

	channels := 2
	if cfg.Mode == Mono {
		channels = 1
	}
	e := &Encoder{
		sbc:      st,
		cfg:      cfg,
		codeSize: int(C.sbc_get_codesize(st)),
		frameLen: int(C.sbc_get_frame_length(st)),
		channels: channels,
		perFrame: cfg.Blocks * cfg.Subbands,
	}
	return e, nil
}

// Close releases the encoder.
func (e *Encoder) Close() {
	if e.sbc != nil {
		C.sbc_finish(e.sbc)
		C.free(unsafe.Pointer(e.sbc))
		e.sbc = nil
	}
}

// Config returns the encoder configuration.
func (e *Encoder) Config() Config { return e.cfg }

// CodeSize is the number of PCM bytes consumed per frame.
func (e *Encoder) CodeSize() int { return e.codeSize }

// FrameLength is the size of one encoded frame in bytes.
func (e *Encoder) FrameLength() int { return e.frameLen }

// SamplesPerFrame is the number of samples per channel in one frame.
func (e *Encoder) SamplesPerFrame() int { return e.perFrame }

// Channels returns the number of PCM channels the encoder expects.
func (e *Encoder) Channels() int { return e.channels }

// Bitrate returns the encoded bit rate in bits per second.
func (e *Encoder) Bitrate() int { return e.frameLen * 8 * e.cfg.SampleRate / e.perFrame }

// EncodeFrame encodes exactly CodeSize bytes of PCM and appends one frame to dst.
func (e *Encoder) EncodeFrame(dst, pcm []byte) ([]byte, error) {
	if len(pcm) != e.codeSize {
		return dst, fmt.Errorf("sbc: need %d PCM bytes, got %d", e.codeSize, len(pcm))
	}
	start := len(dst)
	dst = slices.Grow(dst, e.frameLen)[:start+e.frameLen]
	var written C.ssize_t
	n := C.sbc_encode(e.sbc, unsafe.Pointer(&pcm[0]), C.size_t(len(pcm)),
		unsafe.Pointer(&dst[start]), C.size_t(e.frameLen), &written)
	if n < 0 {
		return dst[:start], fmt.Errorf("sbc: encode failed (%d)", n)
	}
	if int(n) != len(pcm) || int(written) != e.frameLen {
		return dst[:start], fmt.Errorf("sbc: encoded %d of %d bytes into %d", n, len(pcm), written)
	}
	return dst, nil
}
