package wyoming

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu         sync.Mutex
	transcript string
	audio      int
	format     AudioFormat
	stopped    chan struct{}
}

func (r *recorder) VoiceStopped()             {}
func (r *recorder) Transcript(text string)    { r.mu.Lock(); r.transcript = text; r.mu.Unlock() }
func (r *recorder) AudioStart(f AudioFormat)  { r.mu.Lock(); r.format = f; r.mu.Unlock() }
func (r *recorder) Audio(pcm []byte)          { r.mu.Lock(); r.audio += len(pcm); r.mu.Unlock() }
func (r *recorder) AudioStop()                { close(r.stopped) }
func (r *recorder) PipelineError(text string) {}

func send(t *testing.T, c net.Conn, ev Event) {
	t.Helper()
	b, err := ev.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func expect(t *testing.T, r *bufio.Reader, typ string) Event {
	t.Helper()
	for {
		ev, err := readEvent(r)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if ev.Type == typ {
			return ev
		}
	}
}

// Plays Home Assistant's side of a pipeline run.
func TestSatellitePipeline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	rec := &recorder{stopped: make(chan struct{})}
	mic := AudioFormat{Rate: 16000, Width: 2, Channels: 1}
	sat := New(Config{Name: "Test", Listen: addr, Mic: mic, Version: "test"}, rec,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sat.Run(ctx) }()

	var c net.Conn
	for range 50 {
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)

	send(t, c, Event{Type: "describe"})
	info := expect(t, r, "info")
	satInfo, ok := info.Data["satellite"].(map[string]any)
	if !ok || satInfo["name"] != "Test" {
		t.Fatalf("info without satellite: %v", info.Data)
	}
	send(t, c, Event{Type: "ping", Data: map[string]any{"text": "x"}})
	if pong := expect(t, r, "pong"); pong.Data["text"] != "x" {
		t.Errorf("pong %v", pong.Data)
	}
	send(t, c, Event{Type: "run-satellite"})
	for range 50 {
		if sat.Ready() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sat.Ready() {
		t.Fatal("satellite not ready after run-satellite")
	}
	// Home Assistant probes the info on a second connection every 30 s; that
	// must not disturb the running one.
	probe, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	send(t, probe, Event{Type: "describe"})
	expect(t, bufio.NewReader(probe), "info")
	probe.Close()
	time.Sleep(50 * time.Millisecond)
	if !sat.Ready() {
		t.Fatal("a probe connection stopped the satellite")
	}

	if err := sat.StartPipeline(); err != nil {
		t.Fatal(err)
	}
	if rp := expect(t, r, "run-pipeline"); rp.Data["start_stage"] != "asr" || rp.Data["end_stage"] != "tts" {
		t.Errorf("run-pipeline %v", rp.Data)
	}
	if as := expect(t, r, "audio-start"); formatOf(as.Data) != mic {
		t.Errorf("audio-start %v", as.Data)
	}
	if err := sat.SendAudio(make([]byte, 3200)); err != nil { // 100 ms
		t.Fatal(err)
	}
	if ch := expect(t, r, "audio-chunk"); len(ch.Payload) != 3200 {
		t.Errorf("chunk of %d bytes", len(ch.Payload))
	}
	_ = sat.StopAudio()
	expect(t, r, "audio-stop")

	send(t, c, Event{Type: "transcript", Data: map[string]any{"text": "Licht an"}})
	send(t, c, Event{Type: "audio-start", Data: map[string]any{"rate": 22050, "width": 2, "channels": 1}})
	send(t, c, Event{Type: "audio-chunk", Data: map[string]any{"rate": 22050, "width": 2, "channels": 1}, Payload: make([]byte, 2048)})
	send(t, c, Event{Type: "audio-stop"})
	select {
	case <-rec.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("no audio-stop")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.transcript != "Licht an" || rec.audio != 2048 || rec.format.Rate != 22050 {
		t.Errorf("got transcript %q, %d bytes, format %+v", rec.transcript, rec.audio, rec.format)
	}
	if err := sat.Played(); err != nil {
		t.Fatal(err)
	}
	expect(t, r, "played")
}
