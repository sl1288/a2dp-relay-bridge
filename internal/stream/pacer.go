// Package stream turns PCM into paced A2DP media packets for a relay node.
package stream

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Pacer keeps a node's media queue filled to a target duration.
//
// The node reports how much audio it has queued every 20 ms (a credit). The
// pacer adds what was sent since the last credit and lets the sender proceed
// while the estimate is below the target. The node plays out at its own clock,
// so the pacer automatically follows it.
//
// Between two credits at most maxBurst of audio is sent. Without this limit
// the queue would be filled in one burst when a stream starts, and the node
// would briefly hold that burst twice: in its TCP buffers and as Bluetooth
// packets. With high bit rate codecs that exhausts its memory.
type Pacer struct {
	target   time.Duration
	maxBurst time.Duration

	mu        sync.Mutex
	queued    time.Duration // node-side queue at the last credit
	inFlight  time.Duration // sent since the last credit
	lastCred  time.Time
	primed    bool          // the queue has been filled once
	minQueued time.Duration // smallest queue reported since then
	wake      chan struct{}
}

// NewPacer creates a pacer that aims for target of queued audio on the node.
func NewPacer(target time.Duration) *Pacer {
	return &Pacer{target: target, maxBurst: 40 * time.Millisecond, wake: make(chan struct{}, 1)}
}

// TargetForBitrate limits a queue target so that the node never holds more
// than maxBytes of encoded audio; packetRate adds the per-packet overhead.
func TargetForBitrate(target time.Duration, bitrate int, packetRate float64, maxBytes int) time.Duration {
	bytesPerSecond := float64(bitrate)/8 + packetRate*48 // buffer header and bookkeeping per packet
	if bytesPerSecond <= 0 {
		return target
	}
	return min(target, time.Duration(float64(maxBytes)/bytesPerSecond*float64(time.Second)))
}

// Credit records a node credit report.
func (p *Pacer) Credit(queuedUs uint32, underruns uint32) {
	p.mu.Lock()
	p.queued = time.Duration(queuedUs) * time.Microsecond
	p.inFlight = 0
	p.lastCred = time.Now()
	// The queue swings below the target by up to a credit interval and a
	// packet; count it as filled at three quarters.
	if !p.primed && p.queued >= p.target*3/4 {
		p.primed, p.minQueued = true, p.queued
	}
	if p.primed {
		p.minQueued = min(p.minQueued, p.queued)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Reset forgets the queue state, e.g. after a flush or a new stream.
func (p *Pacer) Reset() {
	p.mu.Lock()
	p.queued, p.inFlight, p.lastCred = 0, 0, time.Time{}
	p.primed, p.minQueued = false, 0
	p.mu.Unlock()
}

// MinQueued returns the smallest queue the node reported since the queue was
// first filled: how much of the buffer was actually needed. ok is false while
// the queue is still filling.
func (p *Pacer) MinQueued() (d time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.minQueued, p.primed
}

// Queued returns the estimated audio queued on the node.
func (p *Pacer) Queued() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.estimate()
}

func (p *Pacer) estimate() time.Duration {
	q := p.queued + p.inFlight
	// Between credits the node keeps playing; without this the estimate would
	// lag by up to one credit interval.
	if !p.lastCred.IsZero() {
		q -= min(time.Since(p.lastCred), q)
	}
	return q
}

// ErrNoCredits means the node stopped reporting its queue: it went offline or
// rebooted, and the burst limit would otherwise block the sender forever.
var ErrNoCredits = errors.New("node stopped sending credits")

// creditTimeout is how long Wait tolerates missing credits (nominal: 20 ms).
const creditTimeout = time.Second

// Wait blocks until a packet of duration d can be sent without exceeding the
// target or the burst limit, then accounts for it.
func (p *Pacer) Wait(ctx context.Context, d time.Duration) error {
	// At least two packets per credit interval: with long packets (AAC carries
	// 21 ms) a single one per interval is slower than real time.
	burst := max(p.maxBurst, 2*d)
	start := time.Now()
	for {
		p.mu.Lock()
		if p.estimate()+d <= p.target && (p.inFlight == 0 || p.inFlight+d <= burst) {
			p.inFlight += d
			p.mu.Unlock()
			return nil
		}
		last := p.lastCred
		p.mu.Unlock()
		if last.IsZero() {
			last = start // no credit yet since the stream started
		}
		if time.Since(last) > creditTimeout && time.Since(start) > creditTimeout {
			return ErrNoCredits
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.wake:
		case <-time.After(5 * time.Millisecond):
		}
	}
}
