package sendspin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recorder is a Handler that records calls for assertions.
type recorder struct {
	mu        sync.Mutex
	connected bool
	formats   []Format
	clears    int
	ends      int
	audio     []time.Time
	volume    int
	muted     bool
	meta      Metadata
	ctrl      ControllerState
	gotAudio  chan struct{}
	gotVolume chan struct{}
	gotCtrl   chan struct{}
}

func newRecorder() *recorder {
	return &recorder{gotAudio: make(chan struct{}, 16), gotVolume: make(chan struct{}, 4), gotCtrl: make(chan struct{}, 4)}
}

func (r *recorder) ConnectionChanged(c bool, _ string) { r.mu.Lock(); r.connected = c; r.mu.Unlock() }
func (r *recorder) StreamStarted(f Format) {
	r.mu.Lock()
	r.formats = append(r.formats, f)
	r.mu.Unlock()
}
func (r *recorder) StreamCleared() { r.mu.Lock(); r.clears++; r.mu.Unlock() }
func (r *recorder) StreamEnded()   { r.mu.Lock(); r.ends++; r.mu.Unlock() }
func (r *recorder) Audio(at time.Time, _ []byte) {
	r.mu.Lock()
	r.audio = append(r.audio, at)
	r.mu.Unlock()
	r.gotAudio <- struct{}{}
}
func (r *recorder) VolumeChanged(v int, m bool) {
	r.mu.Lock()
	r.volume, r.muted = v, m
	r.mu.Unlock()
	r.gotVolume <- struct{}{}
}
func (r *recorder) StaticDelayChanged(int)     {}
func (r *recorder) MetadataChanged(m Metadata) { r.mu.Lock(); r.meta = m; r.mu.Unlock() }
func (r *recorder) PlaybackStateChanged(bool)  {}
func (r *recorder) ControllerStateChanged(s ControllerState) {
	r.mu.Lock()
	r.ctrl = s
	r.mu.Unlock()
	r.gotCtrl <- struct{}{}
}

// fakeServer emulates the parts of the Music Assistant Sendspin server the
// client relies on. Its clock runs offsetUs ahead of the wall clock.
type fakeServer struct {
	t        *testing.T
	offsetUs int64
	conn     chan *websocket.Conn
	hello    chan clientHello
	states   chan clientState
	commands chan clientCommand
}

func (s *fakeServer) nowUs() int64 { return time.Now().UnixMicro() + s.offsetUs }

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.t.Errorf("accept: %v", err)
		return
	}
	ctx := r.Context()
	s.conn <- c
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		received := s.nowUs()
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.t.Errorf("client sent invalid JSON: %v", err)
			return
		}
		switch env.Type {
		case "client/hello":
			var h clientHello
			mustUnmarshal(s.t, env.Payload, &h)
			s.hello <- h
			s.send(ctx, c, "server/hello", serverHello{ServerID: "srv", Name: "Fake", Version: 1,
				ActiveRoles: []string{"player@v1", "controller@v1", "metadata@v1"}, ConnectionReason: "discovery"})
		case "client/state":
			var st clientState
			mustUnmarshal(s.t, env.Payload, &st)
			s.states <- st
		case "client/time":
			var ct clientTime
			mustUnmarshal(s.t, env.Payload, &ct)
			s.send(ctx, c, "server/time", serverTime{ClientTransmitted: ct.ClientTransmitted,
				ServerReceived: received, ServerTransmitted: s.nowUs()})
		case "client/command":
			var cc clientCommand
			mustUnmarshal(s.t, env.Payload, &cc)
			s.commands <- cc
		}
	}
}

func (s *fakeServer) send(ctx context.Context, c *websocket.Conn, typ string, payload any) {
	p, _ := json.Marshal(payload)
	b, _ := json.Marshal(envelope{Type: typ, Payload: p})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		s.t.Logf("server write %s: %v", typ, err)
	}
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("unmarshal %s: %v", data, err)
	}
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
	var zero T
	return zero
}

func TestClientAgainstFakeServer(t *testing.T) {
	srv := &fakeServer{
		t:        t,
		offsetUs: 3_600_000_000, // server clock one hour ahead
		conn:     make(chan *websocket.Conn, 1),
		hello:    make(chan clientHello, 1),
		states:   make(chan clientState, 8),
		commands: make(chan clientCommand, 4),
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()

	rec := newRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(Config{
		URL:                "ws" + strings.TrimPrefix(hs.URL, "http"),
		ClientID:           "test-client",
		Name:               "Test Headphones",
		Formats:            []Format{{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16}},
		RequiredLeadTimeMs: 300,
		MinBufferMs:        500,
		Volume:             40,
	}, rec, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	conn := waitFor(t, srv.conn, "connection")
	hello := waitFor(t, srv.hello, "client/hello")
	if hello.ClientID != "test-client" || hello.Version != 1 || hello.PlayerSupport == nil ||
		len(hello.PlayerSupport.SupportedFormats) != 1 || hello.PlayerSupport.BufferCapacity <= 0 {
		t.Fatalf("unexpected hello: %+v", hello)
	}
	st := waitFor(t, srv.states, "initial client/state")
	if !st.Available || st.Player == nil || st.Player.Volume != 40 || st.Player.MinBufferMs != 500 {
		t.Fatalf("unexpected initial state: %+v", st)
	}

	// Let the first clock-sync burst complete.
	deadline := time.Now().Add(3 * time.Second)
	for !c.filter.Synced() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !c.filter.Synced() {
		t.Fatal("clock never synchronized")
	}

	sctx := context.Background()
	srv.send(sctx, conn, "stream/start", map[string]any{"player": Format{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16}})
	want := time.Now().Add(500 * time.Millisecond)
	chunk := make([]byte, audioHeaderLength+4*1200)
	chunk[0] = binaryAudioChunk
	binary.BigEndian.PutUint64(chunk[1:], uint64(srv.nowUs()+500_000))
	if err := conn.Write(sctx, websocket.MessageBinary, chunk); err != nil {
		t.Fatal(err)
	}
	waitFor(t, rec.gotAudio, "audio chunk")
	rec.mu.Lock()
	got := rec.audio[0]
	rec.mu.Unlock()
	if d := got.Sub(want); d < -50*time.Millisecond || d > 50*time.Millisecond {
		t.Fatalf("chunk scheduled %v off the expected local time", d)
	}

	// Volume set by the server must be applied and echoed in client/state.
	srv.send(sctx, conn, "server/command", map[string]any{"player": map[string]any{"command": "volume", "volume": 30}})
	waitFor(t, rec.gotVolume, "volume change")
	echo := waitFor(t, srv.states, "client/state after volume")
	if echo.Player == nil || echo.Player.Volume != 30 {
		t.Fatalf("volume not echoed: %+v", echo)
	}

	// Commands are only sent once the server offered them.
	if err := c.Command(sctx, "next"); err == nil {
		t.Fatal("command accepted before the server offered it")
	}
	srv.send(sctx, conn, "server/state", map[string]any{
		"controller": ControllerState{SupportedCommands: []string{"next", "previous", "play", "pause"}},
		"metadata":   map[string]any{"timestamp": 1, "title": "Song", "artist": "Band"},
	})
	waitFor(t, rec.gotCtrl, "controller state")
	if err := c.Command(sctx, "next"); err != nil {
		t.Fatal(err)
	}
	cmd := waitFor(t, srv.commands, "client/command")
	if cmd.Controller == nil || cmd.Controller.Command != "next" {
		t.Fatalf("unexpected command: %+v", cmd)
	}
	rec.mu.Lock()
	if rec.meta.Title != "Song" || rec.meta.Artist != "Band" {
		t.Errorf("metadata not merged: %+v", rec.meta)
	}
	rec.mu.Unlock()

	// A metadata delta with null clears the field.
	srv.send(sctx, conn, "server/state", map[string]any{"metadata": map[string]any{"artist": nil}})
	srv.send(sctx, conn, "stream/end", map[string]any{})
	time.Sleep(100 * time.Millisecond)
	rec.mu.Lock()
	if rec.meta.Artist != "" || rec.meta.Title != "Song" {
		t.Errorf("null did not clear only the artist: %+v", rec.meta)
	}
	if rec.ends != 1 {
		t.Errorf("stream/end not delivered (ends=%d)", rec.ends)
	}
	rec.mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
