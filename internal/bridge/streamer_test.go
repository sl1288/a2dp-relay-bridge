package bridge

import (
	"testing"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/stream"
)

func ldacPacketizer(t *testing.T, q ldac.Quality) stream.Packetizer {
	t.Helper()
	cfg, err := node.ParseCodecConfig([]byte{10, 0x00, 0xFF, 0x2D, 0x01, 0x00, 0x00, 0xAA, 0x00, 0x10, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	p, err := stream.New(cfg, 882, stream.Options{LDACQuality: q})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// abrRun calls update every 500 ms from..to (relative to start) while the
// congestion counter grows by perCheck, and returns the first change.
func abrRun(a *ldacABR, p stream.Packetizer, start time.Time, from, to time.Duration, busy *uint32, perCheck uint32) (ldac.Quality, time.Duration, bool) {
	for d := from; d <= to; d += 500 * time.Millisecond {
		*busy += perCheck
		if q, changed := a.update(p, *busy, start.Add(d)); changed {
			return q, d, true
		}
	}
	return 0, 0, false
}

func TestLDACABRIgnoresStartupCongestion(t *testing.T) {
	p := ldacPacketizer(t, ldac.SQ)
	start := time.Now()
	a := ldacABR{enabled: true, settle: start.Add(ldacSettle), quietFrom: start}
	var busy uint32
	// The link leaves its power-saving mode: the queue backs up at first.
	if q, d, changed := abrRun(&a, p, start, 500*time.Millisecond, 1500*time.Millisecond, &busy, 10); changed {
		t.Fatalf("quality changed to %v at %v, during the settle time", q, d)
	}
	// Congestion after that means a weak link.
	q, _, changed := abrRun(&a, p, start, 2500*time.Millisecond, 2500*time.Millisecond, &busy, 10)
	if !changed || q != ldac.MQ {
		t.Fatalf("got %v (changed %v), want a step down to MQ", q, changed)
	}
}

func TestLDACABRStepsUpWhenQuiet(t *testing.T) {
	p := ldacPacketizer(t, ldac.SQ)
	start := time.Now()
	a := ldacABR{enabled: true, settle: start.Add(ldacSettle), quietFrom: start}
	var busy uint32
	q, d, changed := abrRun(&a, p, start, 500*time.Millisecond, 20*time.Second, &busy, 0)
	if !changed || q != ldac.HQ {
		t.Fatalf("got %v (changed %v), want a step up to HQ", q, changed)
	}
	// Ten quiet seconds after the settle time.
	if d < ldacSettle+10*time.Second-500*time.Millisecond {
		t.Errorf("stepped up after %v, too early", d)
	}
}

// A fresh stream starts one output latency before the audio is due, and the
// static delay of the player postpones it, as in the Sendspin reference player.
func TestStartTiming(t *testing.T) {
	now := time.Now()
	ms := func(v int) time.Duration { return time.Duration(v) * time.Millisecond }
	tm := startTiming{outputLatency: ms(150)}
	if tm.due(now.Add(ms(400)), now) {
		t.Error("audio due in 400 ms sent already")
	}
	if !tm.due(now.Add(ms(150)), now) || !tm.due(now.Add(-ms(20)), now) {
		t.Error("audio due within the output latency not sent")
	}
	tm.staticDelay = ms(100)
	if tm.due(now.Add(ms(100)), now) {
		t.Error("static delay ignored: audio heard at +200 ms sent 50 ms early")
	}
	if !tm.due(now.Add(ms(50)), now) {
		t.Error("audio heard at +150 ms with static delay not sent")
	}
	// Late audio: what can no longer be heard on time, minus the tolerance.
	if got, want := tm.lateBefore(now, 0), now.Add(ms(50)); !got.Equal(want) {
		t.Errorf("late before %v, want %v", got.Sub(now), want.Sub(now))
	}
	if got, want := tm.lateBefore(now, 5*time.Second), now.Add(ms(50)-5*time.Second); !got.Equal(want) {
		t.Errorf("late before %v with tolerance, want %v", got.Sub(now), want.Sub(now))
	}
}

func TestLDACABRFixedQuality(t *testing.T) {
	p := ldacPacketizer(t, ldac.HQ)
	start := time.Now()
	a := ldacABR{enabled: false, settle: start, quietFrom: start}
	var busy uint32
	if q, d, changed := abrRun(&a, p, start, 500*time.Millisecond, 5*time.Second, &busy, 10); changed {
		t.Fatalf("fixed quality changed to %v at %v", q, d)
	}
}
