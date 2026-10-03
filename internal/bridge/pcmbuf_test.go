package bridge

import (
	"testing"
	"time"
)

// Without a headphone nothing reads the buffer while Music Assistant keeps
// sending: stale audio must not pile up.
func TestPCMBufferDropsStaleAudio(t *testing.T) {
	b := newPCMBuffer()
	b.reset(4, 48000)
	chunk := make([]byte, 480*4) // 10 ms
	start := time.Now().Add(-30 * time.Second)
	for i := range 3000 { // 30 s, the last chunk is due now
		b.push(start.Add(time.Duration(i)*10*time.Millisecond), chunk)
	}
	if got := b.buffered(); got > keepLate+50*time.Millisecond {
		t.Fatalf("%v buffered, want at most %v", got, keepLate)
	}
	head, ok := b.headPlayAt()
	if !ok {
		t.Fatal("buffer empty")
	}
	if late := time.Since(head); late > keepLate+50*time.Millisecond {
		t.Fatalf("oldest audio is %v late", late)
	}
}

// Audio that is still due stays untouched.
func TestPCMBufferKeepsFutureAudio(t *testing.T) {
	b := newPCMBuffer()
	b.reset(4, 48000)
	chunk := make([]byte, 480*4)
	start := time.Now().Add(time.Second)
	for i := range 1000 {
		b.push(start.Add(time.Duration(i)*10*time.Millisecond), chunk)
	}
	if got := b.buffered(); got != 10*time.Second {
		t.Fatalf("%v buffered, want 10s", got)
	}
}
