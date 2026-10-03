package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// simNode consumes audio in real time and reports credits like a node.
type simNode struct {
	mu       sync.Mutex
	queued   time.Duration
	lastTick time.Time
}

func (n *simNode) receive(d time.Duration) {
	n.mu.Lock()
	n.queued += d
	n.mu.Unlock()
}

func (n *simNode) credit(now time.Time) uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.queued -= min(now.Sub(n.lastTick), n.queued)
	n.lastTick = now
	return uint32(n.queued.Microseconds())
}

// The simulated node sends a credit every 25 ms (slower than nominal, as on a
// busy node). The pacer must keep up with every packet duration the codecs
// produce.
func TestPacerKeepsUpWithLongPackets(t *testing.T) {
	for _, d := range []time.Duration{
		5333 * time.Microsecond,  // LDAC HQ
		18667 * time.Microsecond, // SBC, 7 frames
		21333 * time.Microsecond, // AAC
	} {
		p := NewPacer(200 * time.Millisecond)
		start := time.Now()
		node := &simNode{lastTick: start}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(25 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-tick.C:
					p.Credit(node.credit(now), 0)
				}
			}
		}()
		var sent time.Duration
		for p.Wait(ctx, d) == nil {
			node.receive(d)
			sent += d
		}
		cancel()
		<-done
		// After the initial fill the sender must have kept pace with real time.
		if elapsed := time.Since(start); sent < elapsed {
			t.Errorf("packets of %v: sent %v of audio in %v, falling behind real time",
				d, sent.Round(time.Millisecond), elapsed.Round(time.Millisecond))
		}
	}
}

// A node that vanishes stops sending credits: Wait must give up instead of
// blocking on the burst limit forever.
func TestPacerGivesUpWithoutCredits(t *testing.T) {
	p := NewPacer(200 * time.Millisecond)
	p.Credit(0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var err error
	for err == nil {
		err = p.Wait(ctx, 10*time.Millisecond)
	}
	if !errors.Is(err, ErrNoCredits) {
		t.Fatalf("Wait returned %v, want ErrNoCredits", err)
	}
	if d := time.Since(start); d < creditTimeout || d > creditTimeout+500*time.Millisecond {
		t.Errorf("gave up after %v, want about %v", d, creditTimeout)
	}
}

// The minimum only counts once the queue was filled: the start is always empty.
func TestPacerMinQueued(t *testing.T) {
	p := NewPacer(200 * time.Millisecond)
	ms := func(v int) uint32 { return uint32(v * 1000) }
	for _, q := range []int{0, 40, 80, 120} {
		p.Credit(ms(q), 0)
	}
	if _, ok := p.MinQueued(); ok {
		t.Fatal("minimum reported while the queue was still filling")
	}
	for _, q := range []int{160, 190, 175, 120, 185} {
		p.Credit(ms(q), 0)
	}
	if m, ok := p.MinQueued(); !ok || m != 120*time.Millisecond {
		t.Fatalf("minimum %v (ok %v), want 120ms", m, ok)
	}
	p.Reset()
	if _, ok := p.MinQueued(); ok {
		t.Fatal("minimum survived a reset")
	}
}
