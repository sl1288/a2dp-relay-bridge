package wyoming

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Handler receives what Home Assistant sends during a pipeline run. The
// methods are called from the connection's read loop, one at a time.
type Handler interface {
	// VoiceStopped: Home Assistant detected the end of the spoken command.
	VoiceStopped()
	// Transcript is the recognised text.
	Transcript(text string)
	// AudioStart, Audio and AudioStop carry the spoken answer (or an
	// announcement) as PCM.
	AudioStart(f AudioFormat)
	Audio(pcm []byte)
	AudioStop()
	// PipelineError reports a failed pipeline run.
	PipelineError(text string)
}

// Config describes the satellite to Home Assistant.
type Config struct {
	Name    string      // shown in Home Assistant
	Listen  string      // TCP address, e.g. ":10700"
	Mic     AudioFormat // format of the microphone audio sent
	Version string
}

// Satellite serves Home Assistant. Besides the long-lived connection that
// runs the satellite, Home Assistant opens short ones (every 30 s) to query
// its info, so connections are independent; the active one is the last
// that sent run-satellite.
type Satellite struct {
	cfg Config
	h   Handler
	log *slog.Logger

	mu     sync.Mutex
	conns  map[net.Conn]*bufio.Writer
	active net.Conn // connection that runs the satellite, nil if none
	micTS  int      // microphone timestamp in ms
}

// New creates a satellite; call Run to serve it.
func New(cfg Config, h Handler, log *slog.Logger) *Satellite {
	return &Satellite{cfg: cfg, h: h, log: log.With("wyoming", cfg.Listen), conns: map[net.Conn]*bufio.Writer{}}
}

// Ready reports whether Home Assistant is connected and runs the satellite.
func (s *Satellite) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil
}

// Run listens until ctx ends.
func (s *Satellite) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	s.log.Info("Wyoming satellite listening", "name", s.cfg.Name)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[c] = bufio.NewWriter(c)
		s.mu.Unlock()
		s.log.Debug("Home Assistant connected", "from", c.RemoteAddr().String())
		go s.serve(c)
	}
}

func (s *Satellite) serve(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		wasActive := s.active == c
		if wasActive {
			s.active = nil
		}
		s.mu.Unlock()
		c.Close()
		if wasActive {
			s.log.Info("Home Assistant disconnected")
		}
	}()
	r := bufio.NewReader(c)
	for {
		ev, err := readEvent(r)
		if err != nil {
			return
		}
		if err := s.handle(c, ev); err != nil {
			s.log.Warn("Wyoming event failed", "type", ev.Type, "err", err)
			return
		}
	}
}

func (s *Satellite) handle(c net.Conn, ev Event) error {
	switch ev.Type {
	case "describe":
		return s.write(c, s.info())
	case "ping":
		d := map[string]any{}
		if t := str(ev.Data, "text"); t != "" {
			d["text"] = t
		}
		return s.write(c, Event{Type: "pong", Data: d})
	case "run-satellite":
		s.mu.Lock()
		changed := s.active != c
		s.active = c
		s.mu.Unlock()
		if changed {
			s.log.Info("satellite running", "from", c.RemoteAddr().String())
		}
	case "pause-satellite":
		s.mu.Lock()
		if s.active == c {
			s.active = nil
		}
		s.mu.Unlock()
		s.log.Info("satellite paused")
	case "voice-stopped":
		s.h.VoiceStopped()
	case "transcript":
		s.h.Transcript(str(ev.Data, "text"))
	case "audio-start":
		s.h.AudioStart(formatOf(ev.Data))
	case "audio-chunk":
		s.h.Audio(ev.Payload)
	case "audio-stop":
		s.h.AudioStop()
	case "error":
		s.h.PipelineError(str(ev.Data, "text"))
	case "transcribe", "voice-started", "synthesize", "detect", "detection", "pong",
		"timer-started", "timer-updated", "timer-cancelled", "timer-finished":
		s.log.Debug("Wyoming event", "type", ev.Type, "data", ev.Data)
	default:
		s.log.Debug("unhandled Wyoming event", "type", ev.Type)
	}
	return nil
}

func (s *Satellite) info() Event {
	attr := map[string]any{"name": "a2dp-relay", "url": "https://github.com/sl1288/a2dp-relay-bridge"}
	artifact := func(name, desc string) map[string]any {
		return map[string]any{"name": name, "attribution": attr, "installed": true,
			"description": desc, "version": s.cfg.Version}
	}
	mic := artifact("headphone-mic", "Headphone microphone over Bluetooth hands-free")
	mic["mic_format"] = s.cfg.Mic.data(nil)
	snd := artifact("headphone-speaker", "Headphone speaker over Bluetooth hands-free")
	snd["snd_format"] = AudioFormat{Rate: 22050, Width: 2, Channels: 1}.data(nil)
	sat := artifact(s.cfg.Name, "Bluetooth headphone voice assistant (a2dp-relay)")
	sat["area"] = nil
	sat["has_vad"] = false
	sat["active_wake_words"] = []string{}
	sat["max_active_wake_words"] = 0
	sat["supports_trigger"] = false
	return Event{Type: "info", Data: map[string]any{
		"asr": []any{}, "tts": []any{}, "handle": []any{}, "intent": []any{}, "wake": []any{},
		"mic": []any{mic}, "snd": []any{snd}, "satellite": sat,
	}}
}

func (s *Satellite) write(c net.Conn, ev Event) error {
	b, err := ev.encode()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.conns[c]
	if w == nil {
		return errors.New("wyoming: connection closed")
	}
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Satellite) current() (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil, errors.New("wyoming: Home Assistant is not connected")
	}
	return s.active, nil
}

// StartPipeline asks Home Assistant to run speech-to-text through
// text-to-speech and announces the microphone stream.
func (s *Satellite) StartPipeline() error {
	c, err := s.current()
	if err != nil {
		return err
	}
	if err := s.write(c, Event{Type: "run-pipeline", Data: map[string]any{
		"start_stage": "asr", "end_stage": "tts", "restart_on_end": false}}); err != nil {
		return err
	}
	s.mu.Lock()
	s.micTS = 0
	s.mu.Unlock()
	return s.write(c, Event{Type: "audio-start", Data: s.cfg.Mic.data(map[string]any{"timestamp": 0})})
}

// SendAudio streams microphone PCM in the configured format.
func (s *Satellite) SendAudio(pcm []byte) error {
	c, err := s.current()
	if err != nil {
		return err
	}
	s.mu.Lock()
	ts := s.micTS
	f := s.cfg.Mic
	s.micTS += len(pcm) * 1000 / (f.Rate * f.Width * f.Channels)
	s.mu.Unlock()
	return s.write(c, Event{Type: "audio-chunk", Data: f.data(map[string]any{"timestamp": ts}), Payload: pcm})
}

// StopAudio ends the microphone stream.
func (s *Satellite) StopAudio() error {
	c, err := s.current()
	if err != nil {
		return err
	}
	s.mu.Lock()
	ts := s.micTS
	s.mu.Unlock()
	return s.write(c, Event{Type: "audio-stop", Data: map[string]any{"timestamp": ts}})
}

// Played tells Home Assistant that the answer has finished playing.
func (s *Satellite) Played() error {
	c, err := s.current()
	if err != nil {
		return err
	}
	return s.write(c, Event{Type: "played"})
}
