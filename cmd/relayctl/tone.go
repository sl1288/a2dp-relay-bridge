package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/stream"
)

// tone streams a test tone (freq on the left, 1.5 * freq on the right) to the
// headphone the node is connected to, with whatever codec was negotiated.
func tone(ctx context.Context, c *node.Conn, events <-chan any, length time.Duration, freq float64) error {
	var cfg *node.CodecConfig
	mtu := 0
	waitFor := func(what string, timeout time.Duration, done func(any) bool) error {
		deadline := time.After(timeout)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline:
				return fmt.Errorf("timeout waiting for %s", what)
			case m := <-events:
				if e, ok := m.(node.A2DPEvent); ok && e.Event == node.EvAudioConfig {
					if parsed, err := node.ParseCodecConfig(e.Info); err == nil {
						cfg = &parsed
					}
				}
				if e, ok := m.(node.A2DPEvent); ok && e.MTU > 0 {
					mtu = int(e.MTU)
				}
				if done(m) {
					return nil
				}
			}
		}
	}

	// A freshly connected bridge gets the current link state replayed.
	err := waitFor("the headphone connection", 5*time.Second, func(m any) bool {
		e, ok := m.(node.A2DPEvent)
		return ok && (e.Event == node.EvConnected || e.Event == node.EvAudioStarted)
	})
	if err != nil {
		return fmt.Errorf("%w (is the node connected to a headphone?)", err)
	}
	if cfg == nil {
		return errors.New("node did not report a codec configuration")
	}
	p, err := stream.New(*cfg, mtu, stream.Options{SBCMaxBitpool: 53, AACMaxBitrate: 320000, LDACQuality: ldac.SQ})
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Printf("encoding %s at %d kbit/s, MTU %d\n", p.Describe(), p.Bitrate()/1000, mtu)

	if err := c.MediaStart(); err != nil {
		return err
	}
	if err := waitFor("audio start", 5*time.Second, func(m any) bool {
		e, ok := m.(node.A2DPEvent)
		return ok && e.Event == node.EvAudioStarted
	}); err != nil {
		return err
	}

	pacer := stream.NewPacer(200 * time.Millisecond)
	go func() {
		for m := range events {
			if cr, ok := m.(node.Credit); ok {
				pacer.Credit(cr.QueuedUs, cr.Underruns)
			}
		}
	}()

	rate := float64(cfg.SampleRate)
	chunk := cfg.SampleRate / 100 // 10 ms
	pcm := make([]byte, chunk*2*cfg.Channels)
	total := int(length.Seconds() * rate)
	start := time.Now()
	sent := 0
	for n := 0; n < total; n += chunk {
		for i := range chunk {
			t := float64(n+i) / rate
			// 50 ms fade in and out avoid clicks; -20 dBFS is audible but not loud.
			env := math.Max(0, math.Min(1, math.Min(t/0.05, (float64(total)/rate-t)/0.05)))
			l := 0.1 * env * math.Sin(2*math.Pi*freq*t)
			r := 0.1 * env * math.Sin(2*math.Pi*freq*1.5*t)
			if cfg.Channels == 1 {
				binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16((l+r)/2*32767)))
				continue
			}
			binary.LittleEndian.PutUint16(pcm[i*4:], uint16(int16(l*32767)))
			binary.LittleEndian.PutUint16(pcm[i*4+2:], uint16(int16(r*32767)))
		}
		packets, err := p.Write(pcm)
		if err != nil {
			return err
		}
		for _, pkt := range packets {
			if err := pacer.Wait(ctx, pkt.Duration); err != nil {
				return err
			}
			if err := c.Media(pkt.Timestamp, uint32(pkt.Duration.Microseconds()), pkt.Frames, pkt.Data); err != nil {
				return err
			}
			sent++
		}
	}
	// Let the queue drain before suspending.
	time.Sleep(pacer.Queued() + 100*time.Millisecond)
	fmt.Printf("sent %d packets in %v\n", sent, time.Since(start).Round(time.Millisecond))
	return c.MediaSuspend()
}
