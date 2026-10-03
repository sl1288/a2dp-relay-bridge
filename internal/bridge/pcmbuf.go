package bridge

import (
	"context"
	"sync"
	"time"
)

type pcmChunk struct {
	playAt time.Time
	data   []byte
}

// pcmBuffer queues timestamped PCM from Sendspin for the streamer.
type pcmBuffer struct {
	mu          sync.Mutex
	chunks      []pcmChunk
	off         int // bytes of chunks[0] already consumed
	bytes       int
	frameBytes  int // bytes per PCM frame (all channels)
	sampleRate  int
	notify      chan struct{}
	generation  uint64 // increments on reset
	droppedLate int
}

func newPCMBuffer() *pcmBuffer {
	return &pcmBuffer{notify: make(chan struct{}, 1)}
}

// reset drops all audio and sets the format of what follows.
func (b *pcmBuffer) reset(frameBytes, sampleRate int) {
	b.mu.Lock()
	b.chunks, b.off, b.bytes = nil, 0, 0
	b.frameBytes, b.sampleRate = frameBytes, sampleRate
	b.generation++
	b.mu.Unlock()
	b.wake()
}

func (b *pcmBuffer) wake() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// keepLate bounds the buffer while nothing reads it (no headphone connected):
// audio this late is never played, a fresh start skips everything older than
// 5 s.
const keepLate = 6 * time.Second

func (b *pcmBuffer) push(playAt time.Time, data []byte) {
	b.mu.Lock()
	b.chunks = append(b.chunks, pcmChunk{playAt: playAt, data: data})
	b.bytes += len(data)
	b.dropBeforeLocked(time.Now().Add(-keepLate))
	b.mu.Unlock()
	b.wake()
}

// buffered returns how much audio is queued.
func (b *pcmBuffer) buffered() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frameBytes == 0 || b.sampleRate == 0 {
		return 0
	}
	return time.Duration(b.bytes-b.off) / time.Duration(b.frameBytes) * time.Second / time.Duration(b.sampleRate)
}

// headPlayAt returns the playback time of the next unread byte.
func (b *pcmBuffer) headPlayAt() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.headLocked()
}

func (b *pcmBuffer) headLocked() (time.Time, bool) {
	if len(b.chunks) == 0 || b.frameBytes == 0 {
		return time.Time{}, false
	}
	c := b.chunks[0]
	frames := b.off / b.frameBytes
	return c.playAt.Add(time.Duration(frames) * time.Second / time.Duration(b.sampleRate)), true
}

// dropBefore discards whole chunks that end before t (they are too late to
// play) and returns how much audio was dropped.
func (b *pcmBuffer) dropBefore(t time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropBeforeLocked(t)
}

func (b *pcmBuffer) dropBeforeLocked(t time.Time) time.Duration {
	var dropped int
	for len(b.chunks) > 0 && b.frameBytes > 0 {
		c := b.chunks[0]
		dur := time.Duration(len(c.data)/b.frameBytes) * time.Second / time.Duration(b.sampleRate)
		if !c.playAt.Add(dur).Before(t) {
			break
		}
		dropped += len(c.data) - b.off
		b.bytes -= len(c.data)
		b.chunks = b.chunks[1:]
		b.off = 0
		b.droppedLate++
	}
	if b.frameBytes == 0 || b.sampleRate == 0 {
		return 0
	}
	return time.Duration(dropped/b.frameBytes) * time.Second / time.Duration(b.sampleRate)
}

// read fills dst completely, waiting for audio as needed. It returns the
// playback time of dst[0]. A reset while waiting returns errReset.
func (b *pcmBuffer) read(ctx context.Context, dst []byte) (time.Time, error) {
	b.mu.Lock()
	gen := b.generation
	b.mu.Unlock()
	var first time.Time
	n := 0
	for n < len(dst) {
		b.mu.Lock()
		if b.generation != gen {
			b.mu.Unlock()
			return first, errReset
		}
		if n == 0 {
			first, _ = b.headLocked()
		}
		for n < len(dst) && len(b.chunks) > 0 {
			c := b.chunks[0]
			k := copy(dst[n:], c.data[b.off:])
			n += k
			b.off += k
			if b.off == len(c.data) {
				b.bytes -= len(c.data)
				b.chunks = b.chunks[1:]
				b.off = 0
			}
		}
		b.mu.Unlock()
		if n == len(dst) {
			break
		}
		select {
		case <-ctx.Done():
			return first, ctx.Err()
		case <-b.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
	return first, nil
}

// skip discards n bytes (used for drift correction).
func (b *pcmBuffer) skip(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for n > 0 && len(b.chunks) > 0 {
		c := b.chunks[0]
		k := min(n, len(c.data)-b.off)
		b.off += k
		n -= k
		if b.off == len(c.data) {
			b.bytes -= len(c.data)
			b.chunks = b.chunks[1:]
			b.off = 0
		}
	}
}
