package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/sbc"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/voice"
	"github.com/sl1288/a2dp-relay-bridge/internal/wyoming"
)

const (
	// voiceMaxDuration ends a voice session that nobody ends.
	voiceMaxDuration = 45 * time.Second
	// voiceListenMax ends listening if Home Assistant does not detect the end
	// of the command; voiceRecordDuration is the recording without it.
	voiceListenMax      = 10 * time.Second
	voiceRecordDuration = 10 * time.Second
	// voiceChunkBytes is the microphone block sent to Home Assistant (64 ms).
	voiceChunkBytes = 2048
)

// voiceSession is one use of the voice assistant, from the double press on
// the headphone until the voice connection closes:
//
//  1. the node opens the voice connection; the music is paused,
//  2. once the headphone's audio path is up, a start tone plays,
//  3. after the tone, the microphone streams to Home Assistant until it
//     detects the end of the command,
//  4. the spoken answer plays, then the connection closes and the music
//     resumes.
//
// Without Home Assistant the microphone is recorded to a WAV file instead.
type voiceSession struct {
	h     *headphone
	n     *nodeSession
	sat   *wyoming.Satellite // nil: record only
	start time.Time

	mu          sync.Mutex
	codec       node.VoiceCodec
	msbc        *sbc.MSBC
	unpack      voice.Unpacker
	silence     []byte // one encoded mSBC frame of silence
	out         []byte // PCM queued for the headphone speaker
	tonePending bool   // play the start tone once the headphone audio is up
	toneEnd     bool   // the tone was queued; listening starts when it has played
	listening   bool   // microphone audio goes to Home Assistant
	listenAt    time.Time
	micDone     bool
	mic         []byte // microphone PCM not yet sent
	upsample    *voice.Resampler
	answer      *voice.Resampler // answer audio to the voice rate
	answerDone  bool
	played      bool
	transcript  string
	pcm         []byte // recording (without Home Assistant)
	packets     int
	bad         int
	empty       int // good packets without any data
	shown       int
	paused      bool // the music was paused for the session
	stopping    bool
	finished    bool
	stopTimer   *time.Timer
}

func (s *voiceSession) sampleRate() int {
	if s.codec == node.VoiceMSBC {
		return sbc.MSBCSampleRate
	}
	return 8000
}

func (b *Bridge) voiceFor(n *nodeSession) *voiceSession {
	b.voiceMu.Lock()
	defer b.voiceMu.Unlock()
	return b.voice[n]
}

func (b *Bridge) onVoice(n *nodeSession, v node.Voice) {
	switch v.Event {
	case node.VoiceRequested:
		b.startVoice(n, v.Addr)
	case node.VoiceCancelled:
		if s := b.voiceFor(n); s != nil {
			s.h.log.Info("voice assistant ended on the headphone")
			b.stopVoice(s)
		}
	case node.VoiceAudioOn:
		if s := b.voiceFor(n); s != nil {
			s.audioOn(v)
		}
	case node.VoiceAudioOff:
		if s := b.voiceFor(n); s != nil {
			b.finishVoice(s)
		}
	}
}

func (b *Bridge) startVoice(n *nodeSession, addr node.Addr) {
	h := b.headphoneByAddr(addr)
	if h == nil {
		return
	}
	b.voiceMu.Lock()
	if b.voice[n] != nil {
		b.voiceMu.Unlock()
		return
	}
	s := &voiceSession{h: h, n: n, start: time.Now()}
	if sat := h.satellite(); sat != nil && sat.Ready() {
		s.sat = sat
	}
	b.voice[n] = s
	b.voiceMu.Unlock()

	h.log.Info("voice assistant requested", "node", n.name(), "home_assistant", s.sat != nil)
	h.mu.Lock()
	playing, client := h.playing, h.client
	h.mu.Unlock()
	if playing && client != nil {
		ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
		if err := client.Command(ctx, "pause"); err == nil {
			s.paused = true
		} else {
			h.log.Warn("pausing the music failed", "err", err)
		}
		cancel()
	}
	c, err := n.getConn()
	if err == nil {
		err = c.VoiceStart()
	}
	if err != nil {
		h.log.Warn("opening the voice connection failed", "err", err)
		b.finishVoice(s)
		return
	}
	limit := voiceMaxDuration
	if s.sat == nil {
		limit = voiceRecordDuration + 2*time.Second
	}
	s.mu.Lock()
	s.stopTimer = time.AfterFunc(limit, func() { b.stopVoice(s) })
	s.mu.Unlock()
	b.notify()
}

func (s *voiceSession) audioOn(v node.Voice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codec = v.Codec
	if v.Codec == node.VoiceMSBC && s.msbc == nil {
		m, err := sbc.NewMSBC()
		if err != nil {
			s.h.log.Warn("mSBC codec unavailable", "err", err)
			return
		}
		s.msbc = m
		s.silence, _ = m.Encode(make([]byte, sbc.MSBCFrameSamples*2))
	}
	if s.sampleRate() != voiceMicRate {
		s.upsample = voice.NewResampler(s.sampleRate(), voiceMicRate)
	}
	// A start tone tells the user when to speak, as phones do. It waits for
	// the first real microphone packet: the headphone needs over a second
	// before its audio path is up, and would swallow an earlier tone.
	s.tonePending = true
	s.h.log.Info("voice connection open", "codec", map[node.VoiceCodec]string{node.VoiceCVSD: "CVSD 8 kHz",
		node.VoiceMSBC: "mSBC 16 kHz"}[v.Codec], "packet", v.FrameSize)
}

// startTone is two short rising beeps (s16le mono).
func startTone(rate int) []byte {
	var pcm []byte
	add := func(freq float64, d time.Duration) {
		n := int(d.Seconds() * float64(rate))
		for i := range n {
			v := 0.0
			if freq > 0 {
				// Short fade in and out against clicks.
				env := math.Min(1, math.Min(float64(i), float64(n-i))/float64(rate/200))
				v = 0.4 * env * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
			}
			pcm = binary.LittleEndian.AppendUint16(pcm, uint16(int16(v*32767)))
		}
	}
	add(0, 150*time.Millisecond)
	add(660, 90*time.Millisecond)
	add(0, 40*time.Millisecond)
	add(880, 120*time.Millisecond)
	return pcm
}

// nextOut takes n bytes of queued speaker PCM, padded with silence.
func (s *voiceSession) nextOut(n int) []byte {
	b := make([]byte, n)
	k := copy(b, s.out)
	s.out = s.out[k:]
	return b
}

// decodeMic returns the microphone PCM of one packet. Caller holds s.mu.
func (s *voiceSession) decodeMic(a node.VoiceAudio) []byte {
	if s.codec == node.VoiceCVSD {
		if a.Bad {
			return make([]byte, len(a.Data))
		}
		return a.Data
	}
	if s.msbc == nil {
		return nil
	}
	// ESP-IDF strips the H2 header: a packet is the bare 57-byte frame plus
	// the pad byte (58 bytes). Anything else is searched for H2-framed frames.
	var frames [][]byte
	switch {
	case a.Bad || !hasData(a.Data):
		frames = [][]byte{nil}
	case len(a.Data) >= sbc.MSBCFrameLen && a.Data[0] == 0xAD:
		frames = [][]byte{a.Data[:sbc.MSBCFrameLen]}
	default:
		frames = s.unpack.Push(a.Data)
	}
	var pcm []byte
	for _, f := range frames {
		if f != nil {
			if p, err := s.msbc.Decode(f); err == nil {
				pcm = append(pcm, p...)
				continue
			}
		}
		pcm = append(pcm, make([]byte, sbc.MSBCFrameSamples*2)...) // lost frame
	}
	return pcm
}

func (b *Bridge) onVoiceAudio(n *nodeSession, a node.VoiceAudio) {
	s := b.voiceFor(n)
	if s == nil {
		return
	}
	var (
		reply      []byte
		send       [][]byte // microphone blocks for Home Assistant
		startPipe  bool
		listenOver bool
		done       bool
	)
	s.mu.Lock()
	s.packets++
	if a.Bad {
		s.bad++
	} else if !hasData(a.Data) {
		s.empty++
	} else if s.shown < 3 {
		s.shown++
		s.h.log.Debug("voice packet", "n", s.packets, "len", len(a.Data), "data", fmt.Sprintf("% x", a.Data))
	}
	if s.tonePending && !a.Bad && hasData(a.Data) {
		s.tonePending = false
		s.toneEnd = true
		s.out = startTone(s.sampleRate())
	}
	pcm := s.decodeMic(a)
	switch {
	case s.sat == nil:
		if !s.tonePending {
			s.pcm = append(s.pcm, pcm...)
		}
	case s.listening:
		if s.upsample != nil {
			pcm = s.upsample.Convert(pcm)
		}
		if b.cfg.Voice.Record {
			s.pcm = append(s.pcm, pcm...) // at voiceMicRate, see finishVoice
		}
		s.mic = append(s.mic, pcm...)
		for len(s.mic) >= voiceChunkBytes {
			send = append(send, append([]byte(nil), s.mic[:voiceChunkBytes]...))
			s.mic = s.mic[voiceChunkBytes:]
		}
		listenOver = time.Since(s.listenAt) > voiceListenMax
	}

	// Speaker: tone, then the answer.
	n2 := len(a.Data)
	if s.codec == node.VoiceMSBC {
		n2 = sbc.MSBCFrameSamples * 2
	}
	hadOut := len(s.out) > 0
	out := s.nextOut(n2)
	if s.codec == node.VoiceMSBC && s.msbc != nil {
		reply = s.silence // the stack adds the H2 header
		if hadOut {
			if f, err := s.msbc.Encode(out); err == nil {
				reply = f
			}
		}
	} else if s.codec == node.VoiceCVSD {
		reply = out
	}
	if s.toneEnd && len(s.out) == 0 {
		// The tone has played: listen now.
		s.toneEnd = false
		if s.sat != nil {
			s.listening, s.listenAt, startPipe = true, time.Now(), true
		}
	}
	if s.answerDone && len(s.out) == 0 && !s.played {
		s.played, done = true, true
	}
	sat := s.sat
	s.mu.Unlock()

	if reply != nil {
		if c, err := n.getConn(); err == nil {
			_ = c.VoiceAudio(reply)
		}
	}
	if sat == nil {
		return
	}
	if startPipe {
		if err := sat.StartPipeline(); err != nil {
			s.h.log.Warn("starting the Home Assistant pipeline failed", "err", err)
			b.stopVoice(s)
			return
		}
		s.h.log.Info("listening")
	}
	for _, blk := range send {
		if err := sat.SendAudio(blk); err != nil {
			s.h.log.Warn("sending the microphone to Home Assistant failed", "err", err)
			b.stopVoice(s)
			return
		}
	}
	if listenOver {
		s.micEnded("listened for the maximum time")
	}
	if done {
		_ = sat.Played()
		b.stopVoice(s)
	}
}

// micEnded stops sending the microphone; the answer follows.
func (s *voiceSession) micEnded(why string) {
	s.mu.Lock()
	if !s.listening || s.micDone {
		s.mu.Unlock()
		return
	}
	s.listening, s.micDone = false, true
	rest := s.mic
	s.mic = nil
	sat := s.sat
	s.mu.Unlock()
	s.h.log.Info("stopped listening", "reason", why)
	if sat != nil {
		if len(rest) > 0 {
			_ = sat.SendAudio(rest)
		}
		_ = sat.StopAudio()
	}
}

func (s *voiceSession) answerStart(f wyoming.AudioFormat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Width != 2 || f.Channels != 1 || f.Rate == 0 {
		s.h.log.Warn("unsupported answer audio", "format", f)
		return
	}
	s.answer = voice.NewResampler(f.Rate, s.sampleRate())
	s.answerDone = false
}

func (s *voiceSession) answerAudio(pcm []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answer != nil {
		s.out = append(s.out, s.answer.Convert(pcm)...)
	}
}

func (s *voiceSession) answerEnd() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answerDone = true
	// Let the last word fade out before the connection closes.
	s.out = append(s.out, make([]byte, s.sampleRate()/4*2)...)
}

func hasData(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

// stopVoice asks the node to close the voice connection; finishVoice follows
// when it is closed (or after a grace period).
func (b *Bridge) stopVoice(s *voiceSession) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.mu.Unlock()
	s.micEnded("session ends")
	if c, err := s.n.getConn(); err == nil {
		_ = c.VoiceStop()
	}
	time.AfterFunc(3*time.Second, func() { b.finishVoice(s) })
}

func (b *Bridge) finishVoice(s *voiceSession) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	if s.stopTimer != nil {
		s.stopTimer.Stop()
	}
	pcm, rate, packets, bad, empty, transcript := s.pcm, s.sampleRate(), s.packets, s.bad, s.empty, s.transcript
	if s.sat != nil {
		rate = voiceMicRate // recorded as sent to Home Assistant
	}
	if s.msbc != nil {
		s.msbc.Close()
		s.msbc = nil
	}
	s.mu.Unlock()

	b.voiceMu.Lock()
	if b.voice[s.n] == s {
		delete(b.voice, s.n)
	}
	b.voiceMu.Unlock()

	if !s.stopping {
		if c, err := s.n.getConn(); err == nil {
			_ = c.VoiceStop()
		}
	}
	path := ""
	if len(pcm) > 0 {
		dir := filepath.Join(filepath.Dir(b.cfg.StateFile), "voice")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			path = filepath.Join(dir, fmt.Sprintf("%s-%s.wav", s.h.id(), s.start.Format("20060102-150405")))
			if err := voice.WriteWAV(path, rate, pcm); err != nil {
				s.h.log.Warn("saving the recording failed", "err", err)
				path = ""
			}
		}
	}
	s.h.log.Info("voice session ended", "duration", time.Since(s.start).Round(100*time.Millisecond),
		"packets", packets, "bad_packets", bad, "empty_packets", empty, "transcript", transcript, "recording", path)

	if s.paused {
		s.h.mu.Lock()
		client := s.h.client
		s.h.mu.Unlock()
		if client != nil {
			ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
			if err := client.Command(ctx, "play"); err != nil {
				s.h.log.Warn("resuming the music failed", "err", err)
			}
			cancel()
		}
	}
	b.notify()
}
