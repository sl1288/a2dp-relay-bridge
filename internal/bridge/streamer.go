package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/sendspin"
	"github.com/sl1288/a2dp-relay-bridge/internal/stream"
)

var (
	errReset           = errors.New("stream reset")
	errLinkLost        = errors.New("Bluetooth link lost")
	errFormatRequested = errors.New("waiting for the requested stream format")
)

// StreamStats describes a running stream for the web interface.
type StreamStats struct {
	Node        string    `json:"node"`
	Codec       string    `json:"codec"`
	Bitrate     int       `json:"bitrate"`
	SampleRate  int       `json:"sample_rate"`
	MTU         int       `json:"mtu"`
	Packets     uint64    `json:"packets"`
	StartedAt   time.Time `json:"started_at"`
	NodeQueueMs int       `json:"node_queue_ms"`
	// NodeQueueMinMs is the lowest node queue since it was first filled (nil before).
	NodeQueueMinMs *int    `json:"node_queue_min_ms,omitempty"`
	BufferedMs     int     `json:"buffered_ms"`
	DriftMs        float64 `json:"drift_ms"`
	Corrections    int     `json:"corrections"`
	Congestion     uint32  `json:"congestion"` // times the node's Bluetooth queue was full
}

// streamer moves PCM from a headphone's buffer to one node.
type streamer struct {
	h      *headphone
	n      *nodeSession
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	stats StreamStats
}

func (s *streamer) snapshot() StreamStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *streamer) stop() {
	s.cancel()
	<-s.done
}

// ldacABR adapts the LDAC quality to the link: step down when the node's
// Bluetooth queue overflows, step up again after a quiet period.
type ldacABR struct {
	enabled   bool
	settle    time.Time // congestion before this is the link waking up, not a weak link
	lastBusy  uint32
	lastCheck time.Time
	lastDown  time.Time
	quietFrom time.Time
}

// ldacSettle is how long congestion is ignored after a stream starts: a link
// that was idle first leaves its power-saving mode, and the queue backs up.
const ldacSettle = 2 * time.Second

func (a *ldacABR) update(p stream.Packetizer, busy uint32, now time.Time) (ldac.Quality, bool) {
	q, isLDAC := stream.LDACQuality(p)
	if !a.enabled || !isLDAC || now.Sub(a.lastCheck) < 500*time.Millisecond {
		return q, false
	}
	a.lastCheck = now
	delta := busy - a.lastBusy
	a.lastBusy = busy
	if now.Before(a.settle) {
		a.quietFrom = now
		return q, false
	}
	switch {
	case delta >= 3 && q < ldac.MQ && now.Sub(a.lastDown) > 2*time.Second:
		a.lastDown, a.quietFrom = now, now
		return q + 1, true // lower quality, more robust
	case delta > 0:
		a.quietFrom = now
	case q > ldac.HQ && now.Sub(a.quietFrom) > 10*time.Second:
		a.quietFrom = now
		return q - 1, true
	}
	return q, false
}

// startTiming decides when a fresh stream starts. The node's queue is empty
// then, so the first packet reaches the Bluetooth stack at once: audio meant
// to be heard at its timestamp (later by the player's static delay, as in the
// Sendspin reference player) has to be sent outputLatency before. The node
// queue fills up afterwards and adds no delay, because the node plays at the
// pace of its first packet.
type startTiming struct {
	outputLatency time.Duration
	staticDelay   time.Duration
}

// due reports whether audio with timestamp playAt has to be sent now.
func (t startTiming) due(playAt, now time.Time) bool {
	return playAt.Add(t.staticDelay).Sub(now) <= t.outputLatency
}

// lateBefore returns the timestamp before which audio would be heard more
// than tolerance too late.
func (t startTiming) lateBefore(now time.Time, tolerance time.Duration) time.Time {
	return now.Add(t.outputLatency - t.staticDelay - tolerance)
}

// run streams until ctx ends, the stream resets or the link fails. fresh
// aligns the first sample with Music Assistant's timeline; otherwise playback
// continues where the previous streamer stopped (after a handover).
func (s *streamer) run(ctx context.Context, fresh bool) error {
	b, h, n := s.h.b, s.h, s.n
	c, err := n.getConn()
	if err != nil {
		return err
	}
	h.mu.Lock()
	addr, client, cfg := h.addr, h.client, h.cfg
	h.mu.Unlock()
	if client == nil {
		return errors.New("no Music Assistant session")
	}

	// 1. Bluetooth link with a usable codec configuration.
	ready := func(l linkInfo) bool {
		return l.Peer == addr && l.State >= node.LinkConnected && l.Codec != nil && l.MTU > 0
	}
	if !ready(n.currentLink()) {
		if l := n.currentLink(); l.State == node.LinkIdle || l.Peer != addr {
			if err := c.SetCodecs(b.codecPrefs(cfg)); err != nil {
				return err
			}
			h.log.Info("connecting headphone", "node", n.name())
			if err := c.Connect(addr); err != nil {
				return err
			}
		}
		if _, err := n.waitLink(ctx, 20*time.Second, ready); err != nil {
			return fmt.Errorf("connecting via %s: %w", n.name(), err)
		}
	}
	link := n.currentLink()
	// A headphone that connected on its own chose the codec itself. If that
	// is not the one set for it, reconnect once from the node, which then
	// negotiates the preferred codec.
	if want := cfg.Codec; want != "" && want != "auto" && link.Codec.Codec != want && h.mayRetryCodec(n.offeredCodecs(addr), want) {
		h.log.Info("headphone chose another codec, reconnecting", "got", link.Codec.Codec, "want", want)
		if err := c.Disconnect(addr); err != nil {
			return err
		}
		if _, err := n.waitLink(ctx, 6*time.Second, func(l linkInfo) bool { return l.State == node.LinkIdle }); err != nil {
			return fmt.Errorf("disconnecting for the codec change: %w", err)
		}
		if err := c.SetCodecs(b.codecPrefs(cfg)); err != nil {
			return err
		}
		if err := c.Connect(addr); err != nil {
			return err
		}
		if _, err := n.waitLink(ctx, 20*time.Second, ready); err != nil {
			return fmt.Errorf("reconnecting via %s: %w", n.name(), err)
		}
		link = n.currentLink()
	}
	codec := *link.Codec

	// 2. Ask Music Assistant for the negotiated sample rate instead of resampling.
	want := sendspin.Format{Codec: "pcm", Channels: codec.Channels, SampleRate: codec.SampleRate, BitDepth: 16}
	got, ok := client.Format()
	if !ok {
		return errReset
	}
	if got != want {
		h.log.Info("requesting stream format", "from", got, "to", want)
		if err := client.RequestFormat(ctx, want); err != nil {
			return err
		}
		return errFormatRequested
	}

	// 3. Encoder and stream start.
	quality, adaptive := ldacQuality(cfg.LDACQuality)
	if adaptive {
		// Continue at the quality the link sustained during the last track.
		h.mu.Lock()
		if h.ldacLink == n {
			quality = h.ldacQ
		}
		h.mu.Unlock()
	}
	p, err := stream.New(codec, link.MTU, stream.Options{SBCMaxBitpool: b.cfg.Streaming.SBCMaxBitpool,
		AACMaxBitrate: b.cfg.Streaming.AACMaxBitrate, LDACQuality: quality})
	if err != nil {
		return err
	}
	defer p.Close()
	if link.State != node.LinkStreaming {
		if err := c.MediaStart(); err != nil {
			return err
		}
		if _, err := n.waitLink(ctx, 8*time.Second, func(l linkInfo) bool { return l.State == node.LinkStreaming }); err != nil {
			return fmt.Errorf("starting audio: %w", err)
		}
	}
	_ = c.Flush()
	// Bound the node's memory: at most 12 KB of queued packets. That is
	// about 90 ms with LDAC at 990 kbit/s (the queue rarely drops by more
	// than 40 ms); low bit rates keep the full target.
	packetRate := 1000.0 / 5.3 // the densest case: LDAC HQ sends a packet every 5.3 ms
	target := stream.TargetForBitrate(b.cfg.Streaming.NodeBuffer, maxBitrate(codec, p), packetRate, 12<<10)
	pacer := stream.NewPacer(target)
	n.pacer.Store(pacer)
	defer n.pacer.CompareAndSwap(pacer, nil)

	frameBytes := codec.Channels * 2
	s.mu.Lock()
	s.stats = StreamStats{Node: n.name(), Codec: p.Describe(), Bitrate: p.Bitrate(), SampleRate: codec.SampleRate,
		MTU: link.MTU, StartedAt: time.Now()}
	s.mu.Unlock()
	h.log.Info("streaming", "node", n.name(), "codec", p.Describe(), "kbit/s", p.Bitrate()/1000)
	h.mu.Lock()
	h.failures = 0
	played := h.played
	h.played = true
	h.mu.Unlock()

	// 4. Align the start with the timeline, see startTiming. If connecting the
	// headphone took longer than Music Assistant's lead, start late rather
	// than cutting off the beginning of the track; only audio that is very
	// late is skipped. A stream that already played (the headphone
	// reconnected) continues live.
	if fresh {
		timing := startTiming{outputLatency: b.cfg.Streaming.OutputLatency,
			staticDelay: time.Duration(cfg.StaticDelayMs) * time.Millisecond}
		tolerance := 5 * time.Second
		if played {
			tolerance = 0
		}
		for {
			head, ok := h.buf.headPlayAt()
			if now := time.Now(); ok && timing.due(head, now) {
				if dropped := h.buf.dropBefore(timing.lateBefore(now, tolerance)); dropped > 0 {
					h.log.Info("skipped late audio", "ms", dropped.Milliseconds())
				}
				h.log.Debug("stream aligned", "until_due_ms", time.Until(head).Milliseconds(),
					"output_latency", timing.outputLatency, "static_delay", timing.staticDelay)
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	// 5. Stream in 10 ms chunks. The node plays at its own clock; keep the
	// distance between the timeline and our output constant by dropping or
	// repeating a single PCM frame now and then (clock drift between Music
	// Assistant and the node).
	chunk := make([]byte, codec.SampleRate/100*frameBytes)
	last := make([]byte, frameBytes)
	abr := ldacABR{enabled: adaptive, settle: time.Now().Add(ldacSettle), lastBusy: n.stackBusy.Load(), quietFrom: time.Now()}
	var (
		refSet     bool
		ref, ewma  time.Duration
		samples    []time.Duration
		started    = time.Now()
		packets    uint64
		correction int // -1: drop a frame per chunk, +1: repeat one, 0: none
	)
	for {
		if l := n.currentLink(); l.State != node.LinkStreaming || l.Peer != addr {
			return errLinkLost
		}
		if refSet {
			dev := ewma - ref
			if dev < -250*time.Millisecond || dev > 250*time.Millisecond {
				// Not clock drift but a stall (link trouble, a slow node):
				// accept the new timeline instead of dropping audio.
				h.log.Warn("timeline shifted, rebasing drift correction", "ms", dev.Milliseconds())
				ref, dev, correction = ewma, 0, 0
			}
			// Start correcting at 20 ms, stop once back within 5 ms.
			switch {
			case dev < -20*time.Millisecond:
				correction = -1 // late
			case dev > 20*time.Millisecond:
				correction = 1 // early
			case dev > -5*time.Millisecond && dev < 5*time.Millisecond:
				correction = 0
			}
		}
		var playAt time.Time
		if correction == 1 {
			copy(chunk, last)
			playAt, err = h.buf.read(ctx, chunk[frameBytes:])
		} else {
			playAt, err = h.buf.read(ctx, chunk)
			if correction == -1 {
				h.buf.skip(frameBytes)
			}
		}
		if err != nil {
			return err
		}
		copy(last, chunk[len(chunk)-frameBytes:])

		pkts, err := p.Write(chunk)
		if err != nil {
			return err
		}
		for _, pkt := range pkts {
			if err := pacer.Wait(ctx, pkt.Duration); err != nil {
				if errors.Is(err, stream.ErrNoCredits) {
					return fmt.Errorf("%w: %v", errLinkLost, err)
				}
				return err
			}
			if err := c.Media(pkt.Timestamp, uint32(pkt.Duration.Microseconds()), pkt.Frames, pkt.Data); err != nil {
				return err
			}
			packets++
		}

		now := time.Now()
		busy := n.stackBusy.Load()
		if q, changed := abr.update(p, busy, now); changed {
			if err := stream.SetLDACQuality(p, q); err == nil {
				h.mu.Lock()
				h.ldacQ, h.ldacLink = q, n
				h.mu.Unlock()
				// The encoder reports the new bit rate only after its next frame.
				h.log.Info("LDAC quality changed", "quality", q.String(), "congestion", busy)
			}
		}

		offset := time.Until(playAt) - pacer.Queued()
		switch {
		case !refSet && now.Sub(started) > 3*time.Second:
			samples = append(samples, offset)
			if len(samples) == 100 {
				ref = median(samples)
				ewma = ref
				refSet = true
			}
		case refSet:
			ewma += (offset - ewma) / 64
		}
		s.mu.Lock()
		s.stats.Packets = packets
		s.stats.Codec = p.Describe()
		s.stats.Bitrate = p.Bitrate()
		s.stats.NodeQueueMs = int(pacer.Queued().Milliseconds())
		if m, ok := pacer.MinQueued(); ok {
			ms := int(m.Milliseconds())
			s.stats.NodeQueueMinMs = &ms
		}
		s.stats.BufferedMs = int(h.buf.buffered().Milliseconds())
		s.stats.Congestion = busy
		if refSet {
			s.stats.DriftMs = float64(ewma-ref) / float64(time.Millisecond)
		}
		if correction != 0 {
			s.stats.Corrections++
		}
		s.mu.Unlock()
	}
}

// maxBitrate is the highest bit rate the stream can reach: LDAC may step up
// to 990 kbit/s while it runs, so size the node queue for that.
func maxBitrate(codec node.CodecConfig, p stream.Packetizer) int {
	if codec.Codec == node.CodecNameLDAC {
		return 990000
	}
	return p.Bitrate()
}

func median(v []time.Duration) time.Duration {
	c := append([]time.Duration(nil), v...)
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j] < c[j-1]; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
	return c[len(c)/2]
}
