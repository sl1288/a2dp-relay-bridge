package bridge

import (
	"context"
	"fmt"

	"github.com/sl1288/a2dp-relay-bridge/internal/wyoming"
)

// Each headphone is a Wyoming satellite: Home Assistant connects to its port
// (Wyoming integration, host of the bridge) and runs the Assist pipeline
// chosen there whenever the voice assistant is started on the headphone.

// voiceMicRate is the microphone format sent to Home Assistant; 8 kHz voice
// (CVSD) is resampled to it.
const voiceMicRate = 16000

func (h *headphone) startSatellite(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sat != nil || h.cfg.WyomingPort == 0 || ctx == nil {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	h.sat = wyoming.New(wyoming.Config{
		Name:    h.cfg.Name,
		Listen:  fmt.Sprintf(":%d", h.cfg.WyomingPort),
		Mic:     wyoming.AudioFormat{Rate: voiceMicRate, Width: 2, Channels: 1},
		Version: Version,
	}, satHandler{h}, h.log)
	h.stopSat = cancel
	sat := h.sat
	go func() {
		if err := sat.Run(sctx); err != nil {
			h.log.Warn("Wyoming satellite stopped", "err", err)
		}
	}()
}

func (h *headphone) stopSatellite() {
	h.mu.Lock()
	stop := h.stopSat
	h.sat, h.stopSat = nil, nil
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (h *headphone) satellite() *wyoming.Satellite {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sat
}

// voice returns the headphone's running voice session.
func (h *headphone) voice() *voiceSession {
	h.b.voiceMu.Lock()
	defer h.b.voiceMu.Unlock()
	for _, s := range h.b.voice {
		if s.h == h {
			return s
		}
	}
	return nil
}

// --- wyoming.Handler -----------------------------------------------------------

// satHandler passes Home Assistant's events to the headphone's voice session.
type satHandler struct{ h *headphone }

func (a satHandler) VoiceStopped() {
	h := a.h
	if s := h.voice(); s != nil {
		s.micEnded("Home Assistant detected the end of the command")
	}
}

func (a satHandler) Transcript(text string) {
	h := a.h
	h.log.Info("voice assistant heard", "text", text)
	if s := h.voice(); s != nil {
		s.mu.Lock()
		s.transcript = text
		s.mu.Unlock()
	}
}

func (a satHandler) AudioStart(f wyoming.AudioFormat) {
	h := a.h
	if s := h.voice(); s != nil {
		s.answerStart(f)
	}
}

func (a satHandler) Audio(pcm []byte) {
	h := a.h
	if s := h.voice(); s != nil {
		s.answerAudio(pcm)
	}
}

func (a satHandler) AudioStop() {
	h := a.h
	if s := h.voice(); s != nil {
		s.answerEnd()
	} else if sat := h.satellite(); sat != nil {
		// An announcement while no voice connection is open: not played yet.
		_ = sat.Played()
	}
}

func (a satHandler) PipelineError(text string) {
	h := a.h
	h.log.Warn("voice assistant failed", "err", text)
	if s := h.voice(); s != nil {
		h.b.stopVoice(s)
	}
}
