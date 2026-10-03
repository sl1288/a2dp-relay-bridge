// Package ldac wraps Sony's LDAC encoder from AOSP (libldacBT_enc).
package ldac

/*
#cgo LDFLAGS: -lldacBT_enc
#include <ldac/ldacBT.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Quality is the LDAC encode quality mode (EQMID).
type Quality int

// Values of LDACBT_EQMID_HQ/SQ/MQ in ldacBT.h.
const (
	HQ Quality = 0 // 990 kbit/s at 48 kHz
	SQ Quality = 1 // 660 kbit/s
	MQ Quality = 2 // 330 kbit/s
)

func (q Quality) String() string {
	switch q {
	case HQ:
		return "HQ"
	case SQ:
		return "SQ"
	case MQ:
		return "MQ"
	}
	return fmt.Sprintf("EQMID %d", int(q))
}

// FrameSamples is the number of samples per channel ldacBT_encode consumes per call.
const FrameSamples = C.LDACBT_ENC_LSU

// Config selects the encoder parameters.
type Config struct {
	// MTU of the AVDTP media channel (RTP and payload header included);
	// LDAC needs at least 679 bytes.
	MTU     int
	Quality Quality
	// ChannelMode as in the A2DP configuration: 0x01 stereo, 0x02 dual, 0x04 mono.
	ChannelMode int
	SampleRate  int
}

// Encoder encodes interleaved s16le PCM into LDAC transport frames.
type Encoder struct {
	h        C.HANDLE_LDAC_BT
	cfg      Config
	channels int
	out      []byte
}

// NewEncoder initialises an encoder.
func NewEncoder(cfg Config) (*Encoder, error) {
	h := C.ldacBT_get_handle()
	if h == nil {
		return nil, fmt.Errorf("ldac: out of memory")
	}
	if rc := C.ldacBT_init_handle_encode(h, C.int(cfg.MTU), C.int(cfg.Quality), C.int(cfg.ChannelMode),
		C.LDACBT_SMPL_FMT_S16, C.int(cfg.SampleRate)); rc != 0 {
		code := int(C.ldacBT_get_error_code(h))
		C.ldacBT_free_handle(h)
		return nil, fmt.Errorf("ldac: init failed (error %d; MTU %d, sample rate %d)", code, cfg.MTU, cfg.SampleRate)
	}
	channels := 2
	if cfg.ChannelMode == 0x04 {
		channels = 1
	}
	return &Encoder{h: h, cfg: cfg, channels: channels, out: make([]byte, C.LDACBT_MAX_NBYTES)}, nil
}

// Close releases the encoder.
func (e *Encoder) Close() {
	if e.h != nil {
		C.ldacBT_free_handle(e.h)
		e.h = nil
	}
}

// FrameBytes is the PCM input per call.
func (e *Encoder) FrameBytes() int { return FrameSamples * e.channels * 2 }

// SetQuality changes the encode quality on the fly.
func (e *Encoder) SetQuality(q Quality) error {
	if rc := C.ldacBT_set_eqmid(e.h, C.int(q)); rc != 0 {
		return fmt.Errorf("ldac: setting %v failed (error %d)", q, int(C.ldacBT_get_error_code(e.h)))
	}
	e.cfg.Quality = q
	return nil
}

// Quality returns the current encode quality.
func (e *Encoder) Quality() Quality { return Quality(C.ldacBT_get_eqmid(e.h)) }

// Bitrate returns the current bit rate in bit/s.
func (e *Encoder) Bitrate() int { return int(C.ldacBT_get_bitrate(e.h)) * 1000 }

// Encode consumes exactly 128 samples per channel. Once enough frames are
// collected it returns a packet payload (LDAC frames without header) and the
// number of frames in it; otherwise it returns nil.
func (e *Encoder) Encode(pcm []byte) ([]byte, int, error) {
	if len(pcm) != e.FrameBytes() {
		return nil, 0, fmt.Errorf("ldac: need %d PCM bytes, got %d", e.FrameBytes(), len(pcm))
	}
	var used, written, frames C.int
	rc := C.ldacBT_encode(e.h, unsafe.Pointer(&pcm[0]), &used, (*C.uchar)(unsafe.Pointer(&e.out[0])), &written, &frames)
	if rc != 0 {
		return nil, 0, fmt.Errorf("ldac: encode failed (error %d)", int(C.ldacBT_get_error_code(e.h)))
	}
	if written <= 0 {
		return nil, 0, nil
	}
	return append([]byte(nil), e.out[:written]...), int(frames), nil
}
