// Command sendspin-probe registers a test player with a Sendspin server and
// logs what the server sends. It verifies the Sendspin client against Music
// Assistant without any Bluetooth hardware.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/sendspin"
)

type probe struct {
	log *slog.Logger

	mu        sync.Mutex
	chunks    int
	bytes     int
	minLead   time.Duration
	maxLead   time.Duration
	format    sendspin.Format
	playing   bool
	firstSeen chan struct{}
	once      sync.Once
}

func (p *probe) ConnectionChanged(connected bool, server string) {
	p.log.Info("connection", "connected", connected, "server", server)
}

func (p *probe) StreamStarted(f sendspin.Format) {
	p.mu.Lock()
	p.format = f
	p.mu.Unlock()
	p.log.Info("stream/start", "format", f)
}

func (p *probe) StreamCleared() { p.log.Info("stream/clear") }
func (p *probe) StreamEnded()   { p.log.Info("stream/end") }

func (p *probe) Audio(playAt time.Time, data []byte) {
	lead := time.Until(playAt)
	p.mu.Lock()
	if p.chunks == 0 || lead < p.minLead {
		p.minLead = lead
	}
	if p.chunks == 0 || lead > p.maxLead {
		p.maxLead = lead
	}
	p.chunks++
	p.bytes += len(data)
	p.mu.Unlock()
	p.once.Do(func() { close(p.firstSeen) })
}

func (p *probe) VolumeChanged(v int, m bool) { p.log.Info("volume", "volume", v, "muted", m) }
func (p *probe) StaticDelayChanged(ms int)   { p.log.Info("static delay", "ms", ms) }
func (p *probe) MetadataChanged(m sendspin.Metadata) {
	p.log.Info("metadata", "title", m.Title, "artist", m.Artist, "album", m.Album)
}

func (p *probe) PlaybackStateChanged(playing bool) {
	p.mu.Lock()
	p.playing = playing
	p.mu.Unlock()
	p.log.Info("playback state", "playing", playing)
}

func (p *probe) ControllerStateChanged(s sendspin.ControllerState) {
	p.log.Info("controller state", "commands", s.SupportedCommands, "volume", s.Volume)
}

func (p *probe) report() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chunks == 0 {
		return
	}
	secs := 0.0
	if bpf := p.format.BytesPerFrame(); bpf > 0 && p.format.SampleRate > 0 {
		secs = float64(p.bytes) / float64(bpf*p.format.SampleRate)
	}
	p.log.Info("audio", "chunks", p.chunks, "bytes", p.bytes, "audio_s", secs,
		"lead_min", p.minLead.Round(time.Millisecond), "lead_max", p.maxLead.Round(time.Millisecond))
	p.chunks, p.bytes = 0, 0
}

func main() {
	url := flag.String("url", "ws://localhost:8927/sendspin", "Sendspin server endpoint")
	id := flag.String("id", "a2dp-relay-probe", "client ID")
	name := flag.String("name", "A2DP Relay Test", "player name")
	duration := flag.Duration("duration", 0, "exit after this long (0 = run until interrupted)")
	command := flag.String("command", "", "controller command to send 5 s after audio starts (e.g. next)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	p := &probe{log: log, firstSeen: make(chan struct{})}
	c := sendspin.New(sendspin.Config{
		URL:                *url,
		ClientID:           *id,
		Name:               *name,
		ProductName:        "A2DP relay bridge",
		Manufacturer:       "a2dp-relay",
		SoftwareVersion:    "0.1.0-dev",
		Formats:            []sendspin.Format{{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16}, {Codec: "pcm", Channels: 2, SampleRate: 44100, BitDepth: 16}},
		BufferCapacity:     2 << 20,
		RequiredLeadTimeMs: 300,
		MinBufferMs:        500,
		Volume:             50,
	}, p, log)

	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.report()
			}
		}
	}()

	if *command != "" {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-p.firstSeen:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			err := c.Command(ctx, *command)
			log.Info("sent controller command", "command", *command, "err", err)
		}()
	}

	err := c.Run(ctx)
	p.report()
	log.Info("probe finished", "err", err)
}
