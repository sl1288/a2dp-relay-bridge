// Package aac wraps the Fraunhofer FDK AAC encoder (libfdk-aac) for A2DP:
// AAC-LC in LATM with an in-band StreamMuxConfig, one AudioMuxElement per
// 1024-sample frame, as Android and PipeWire send it.
package aac

/*
#cgo LDFLAGS: -lfdk-aac
#include <fdk-aac/aacenc_lib.h>

static AACENC_ERROR encode_frame(HANDLE_AACENCODER h, void *pcm, int pcm_bytes,
                                 void *out, int out_size, int *out_bytes) {
	AACENC_BufDesc in_buf = {0}, out_buf = {0};
	AACENC_InArgs in_args = {0};
	AACENC_OutArgs out_args = {0};
	int in_id = IN_AUDIO_DATA, in_el = 2, in_size = pcm_bytes;
	int out_id = OUT_BITSTREAM_DATA, out_el = 1;
	void *in_ptr = pcm, *out_ptr = out;

	in_buf.numBufs = 1;
	in_buf.bufs = &in_ptr;
	in_buf.bufferIdentifiers = &in_id;
	in_buf.bufSizes = &in_size;
	in_buf.bufElSizes = &in_el;
	out_buf.numBufs = 1;
	out_buf.bufs = &out_ptr;
	out_buf.bufferIdentifiers = &out_id;
	out_buf.bufSizes = &out_size;
	out_buf.bufElSizes = &out_el;
	in_args.numInSamples = pcm_bytes / 2;

	AACENC_ERROR err = aacEncEncode(h, &in_buf, &out_buf, &in_args, &out_args);
	*out_bytes = out_args.numOutBytes;
	return err;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Config selects the encoder parameters.
type Config struct {
	SampleRate  int
	Channels    int
	Bitrate     int  // target bit rate (bit/s)
	PeakBitrate int  // hard cap per frame, derived from the MTU (bit/s)
	MPEG2       bool // MPEG-2 AAC LC object type (else MPEG-4 AAC LC)
}

// Encoder encodes interleaved s16le PCM to LATM AudioMuxElements.
type Encoder struct {
	h        C.HANDLE_AACENCODER
	cfg      Config
	frameLen int // samples per channel per frame
	maxOut   int
	out      []byte
}

// NewEncoder opens an encoder.
func NewEncoder(cfg Config) (*Encoder, error) {
	if cfg.Channels < 1 || cfg.Channels > 2 {
		return nil, fmt.Errorf("aac: %d channels unsupported", cfg.Channels)
	}
	e := &Encoder{cfg: cfg}
	if err := C.aacEncOpen(&e.h, 0, C.UINT(cfg.Channels)); err != C.AACENC_OK {
		return nil, fmt.Errorf("aac: open failed (0x%x)", int(err))
	}
	aot := C.UINT(C.AOT_AAC_LC)
	if cfg.MPEG2 {
		aot = C.UINT(C.AOT_MP2_AAC_LC)
	}
	mode := C.UINT(C.MODE_2)
	if cfg.Channels == 1 {
		mode = C.UINT(C.MODE_1)
	}
	params := []struct {
		id    C.AACENC_PARAM
		value C.UINT
		name  string
	}{
		{C.AACENC_AOT, aot, "object type"},
		{C.AACENC_SAMPLERATE, C.UINT(cfg.SampleRate), "sample rate"},
		{C.AACENC_CHANNELMODE, mode, "channel mode"},
		{C.AACENC_BITRATEMODE, 0, "bit rate mode"}, // constant bit rate
		{C.AACENC_BITRATE, C.UINT(cfg.Bitrate), "bit rate"},
		{C.AACENC_PEAK_BITRATE, C.UINT(cfg.PeakBitrate), "peak bit rate"},
		{C.AACENC_TRANSMUX, C.UINT(C.TT_MP4_LATM_MCP1), "transport"},
		{C.AACENC_HEADER_PERIOD, 1, "header period"},
		{C.AACENC_AFTERBURNER, 1, "afterburner"},
	}
	for _, p := range params {
		if err := C.aacEncoder_SetParam(e.h, p.id, p.value); err != C.AACENC_OK {
			C.aacEncClose(&e.h)
			return nil, fmt.Errorf("aac: setting %s to %d failed (0x%x)", p.name, int(p.value), int(err))
		}
	}
	// A call without buffers applies the parameters.
	if err := C.aacEncEncode(e.h, nil, nil, nil, nil); err != C.AACENC_OK {
		C.aacEncClose(&e.h)
		return nil, fmt.Errorf("aac: initialisation failed (0x%x)", int(err))
	}
	var info C.AACENC_InfoStruct
	if err := C.aacEncInfo(e.h, &info); err != C.AACENC_OK {
		C.aacEncClose(&e.h)
		return nil, fmt.Errorf("aac: info failed (0x%x)", int(err))
	}
	e.frameLen = int(info.frameLength)
	e.maxOut = int(info.maxOutBufBytes)
	e.out = make([]byte, e.maxOut)
	return e, nil
}

// Close releases the encoder.
func (e *Encoder) Close() {
	if e.h != nil {
		C.aacEncClose(&e.h)
		e.h = nil
	}
}

// FrameSamples returns the samples per channel in one frame (1024).
func (e *Encoder) FrameSamples() int { return e.frameLen }

// FrameBytes returns the PCM bytes consumed per frame.
func (e *Encoder) FrameBytes() int { return e.frameLen * e.cfg.Channels * 2 }

// Encode encodes exactly one frame of PCM. It returns the AudioMuxElement,
// which is empty while the encoder fills its look-ahead.
func (e *Encoder) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) != e.FrameBytes() {
		return nil, fmt.Errorf("aac: need %d PCM bytes, got %d", e.FrameBytes(), len(pcm))
	}
	var n C.int
	err := C.encode_frame(e.h, unsafe.Pointer(&pcm[0]), C.int(len(pcm)), unsafe.Pointer(&e.out[0]), C.int(len(e.out)), &n)
	if err != C.AACENC_OK {
		return nil, fmt.Errorf("aac: encode failed (0x%x)", int(err))
	}
	return append([]byte(nil), e.out[:n]...), nil
}
