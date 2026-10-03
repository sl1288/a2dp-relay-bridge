// Package sendspin implements a Sendspin player and controller client.
//
// It targets the unencrypted wire that Music Assistant 2.10 accepts from
// "legacy" clients: the client dials the server, announces itself as player
// and controller, receives timestamped PCM chunks and can send transport
// commands (next, previous, play, pause, ...) back to its own group.
package sendspin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	ssync "github.com/Sendspin/sendspin-go/pkg/sync"
	"github.com/coder/websocket"
)

// Config describes one Sendspin client, i.e. one player in Music Assistant.
type Config struct {
	// URL of the server endpoint, e.g. ws://music-assistant.local:8927/sendspin.
	URL string
	// ClientID must be stable and unique. Do not use a MAC address: Music
	// Assistant would merge the player with other players of that host.
	ClientID string
	// Name is the player name shown in Music Assistant.
	Name            string
	ProductName     string
	Manufacturer    string
	SoftwareVersion string
	// Formats lists the accepted stream formats in priority order.
	Formats []Format
	// BufferCapacity is the number of bytes of audio the client can queue.
	BufferCapacity int
	// RequiredLeadTimeMs and MinBufferMs tell the server how far ahead to send.
	RequiredLeadTimeMs int
	MinBufferMs        int
	// Initial player state.
	Volume        int
	Muted         bool
	StaticDelayMs int
}

// Handler receives everything the server sends. Methods are called from the
// connection's read goroutine and must not block for long.
type Handler interface {
	ConnectionChanged(connected bool, serverName string)
	StreamStarted(f Format)
	StreamCleared()
	StreamEnded()
	// Audio delivers one chunk; playAt is the local time at which its first
	// frame should leave the headphone, static delay already subtracted.
	Audio(playAt time.Time, data []byte)
	VolumeChanged(volume int, muted bool)
	StaticDelayChanged(ms int)
	MetadataChanged(m Metadata)
	PlaybackStateChanged(playing bool)
	ControllerStateChanged(s ControllerState)
}

// ErrNotConnected is returned by commands while no session is established.
var ErrNotConnected = errors.New("sendspin: not connected")

type timeSample struct {
	resp serverTime
	t4   int64
}

// Client is a reconnecting Sendspin client.
type Client struct {
	cfg   Config
	h     Handler
	log   *slog.Logger
	epoch time.Time

	filter *ssync.TimeFilter
	times  chan timeSample

	mu         sync.Mutex
	conn       *websocket.Conn
	available  bool
	volume     int
	muted      bool
	delayMs    int
	streaming  bool
	format     Format
	controller ControllerState
	metadata   map[string]json.RawMessage
	unsynced   bool // logged that audio was dropped before clock sync
}

// New creates a client; call Run to connect.
func New(cfg Config, h Handler, log *slog.Logger) *Client {
	if cfg.BufferCapacity <= 0 {
		cfg.BufferCapacity = 1 << 20
	}
	return &Client{
		cfg:       cfg,
		h:         h,
		log:       log.With("player", cfg.Name),
		epoch:     time.Now(),
		filter:    ssync.NewTimeFilter(ssync.DefaultTimeFilterConfig()),
		times:     make(chan timeSample, 8),
		available: true,
		volume:    cfg.Volume,
		muted:     cfg.Muted,
		delayMs:   cfg.StaticDelayMs,
	}
}

// nowUs is the client clock: monotonic microseconds since the client was created.
func (c *Client) nowUs() int64 { return time.Since(c.epoch).Microseconds() }

func (c *Client) localTime(us int64) time.Time {
	return c.epoch.Add(time.Duration(us) * time.Microsecond)
}

// Run keeps a session to the server alive until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.log.Warn("sendspin session ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, c.cfg.URL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(4 << 20)
	defer conn.CloseNow()

	sessCtx, stop := context.WithCancel(ctx)
	defer stop()

	if err := c.write(sessCtx, conn, "client/hello", c.hello()); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	hello, err := c.awaitServerHello(sessCtx, conn)
	if err != nil {
		return err
	}
	c.log.Info("connected to sendspin server", "server", hello.Name, "roles", hello.ActiveRoles)

	c.filter.Reset()
	c.mu.Lock()
	c.conn = conn
	c.metadata = map[string]json.RawMessage{}
	c.unsynced = false
	c.mu.Unlock()
	defer c.dropConn(conn)

	if err := c.sendState(sessCtx); err != nil {
		return fmt.Errorf("initial state: %w", err)
	}
	c.h.ConnectionChanged(true, hello.Name)

	go c.syncClock(sessCtx, conn)
	err = c.readLoop(sessCtx, conn)
	if ctx.Err() != nil {
		// Shutting down on purpose: say goodbye while the connection is still open.
		bye, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if werr := c.write(bye, conn, "client/goodbye", clientGoodbye{Reason: "shutdown"}); werr == nil {
			conn.Close(websocket.StatusNormalClosure, "shutdown")
		}
		cancel()
	}
	return err
}

func (c *Client) dropConn(conn *websocket.Conn) {
	c.mu.Lock()
	wasStreaming := c.streaming
	if c.conn == conn {
		c.conn = nil
	}
	c.streaming = false
	c.mu.Unlock()
	if wasStreaming {
		c.h.StreamEnded()
	}
	c.h.ConnectionChanged(false, "")
}

func (c *Client) hello() clientHello {
	roles := []string{"player@v1", "controller@v1", "metadata@v1"}
	return clientHello{
		ClientID: c.cfg.ClientID,
		Name:     c.cfg.Name,
		DeviceInfo: &deviceInfo{
			ProductName:     c.cfg.ProductName,
			Manufacturer:    c.cfg.Manufacturer,
			SoftwareVersion: c.cfg.SoftwareVersion,
		},
		Version:        coreVersion,
		SupportedRoles: roles,
		PlayerSupport: &playerSupport{
			SupportedFormats:  c.cfg.Formats,
			BufferCapacity:    c.cfg.BufferCapacity,
			SupportedCommands: []string{"volume", "mute"},
		},
	}
}

func (c *Client) awaitServerHello(ctx context.Context, conn *websocket.Conn) (serverHello, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return serverHello{}, fmt.Errorf("waiting for server/hello: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return serverHello{}, fmt.Errorf("decode: %w", err)
		}
		if env.Type != "server/hello" {
			c.log.Debug("ignoring message before server/hello", "type", env.Type)
			continue
		}
		var h serverHello
		if err := json.Unmarshal(env.Payload, &h); err != nil {
			return serverHello{}, fmt.Errorf("decode server/hello: %w", err)
		}
		if !slices.Contains(h.ActiveRoles, "player@v1") {
			return h, fmt.Errorf("server did not activate player@v1 (active: %v)", h.ActiveRoles)
		}
		return h, nil
	}
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b, err := json.Marshal(envelope{Type: typ, Payload: p})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

// send writes on the current connection, if any.
func (c *Client) send(ctx context.Context, typ string, payload any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return c.write(ctx, conn, typ, payload)
}

func (c *Client) sendState(ctx context.Context) error {
	c.mu.Lock()
	st := clientState{
		Available: c.available,
		Player: &playerState{
			Volume:             c.volume,
			Muted:              c.muted,
			StaticDelayMs:      c.delayMs,
			RequiredLeadTimeMs: c.cfg.RequiredLeadTimeMs,
			MinBufferMs:        c.cfg.MinBufferMs,
			SupportedCommands:  []string{"set_static_delay"},
		},
	}
	c.mu.Unlock()
	return c.send(ctx, "client/state", st)
}

// syncClock runs bursts of time exchanges and feeds the best sample of each
// burst (lowest uncertainty) into the Kalman filter.
func (c *Client) syncClock(ctx context.Context, conn *websocket.Conn) {
	for burst := 0; ; burst++ {
		if s, ok := c.measureBurst(ctx, conn, 8); ok {
			offset := ((s.resp.ServerReceived - s.resp.ClientTransmitted) + (s.resp.ServerTransmitted - s.t4)) / 2
			maxErr := ((s.t4 - s.resp.ClientTransmitted) - (s.resp.ServerTransmitted - s.resp.ServerReceived)) / 2
			c.filter.Update(offset, maxErr, s.t4)
		}
		interval := 10 * time.Second
		if burst < 4 {
			interval = time.Second // converge quickly after connecting
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (c *Client) measureBurst(ctx context.Context, conn *websocket.Conn, n int) (timeSample, bool) {
	var best timeSample
	bestErr := int64(-1)
	for range n {
		t1 := c.nowUs()
		if err := c.write(ctx, conn, "client/time", clientTime{ClientTransmitted: t1}); err != nil {
			return best, false
		}
		timeout := time.After(time.Second)
	wait:
		for {
			select {
			case <-ctx.Done():
				return best, false
			case <-timeout:
				break wait
			case s := <-c.times:
				if s.resp.ClientTransmitted != t1 {
					continue // late answer to an earlier request
				}
				e := (s.t4 - t1) - (s.resp.ServerTransmitted - s.resp.ServerReceived)
				if bestErr < 0 || e < bestErr {
					best, bestErr = s, e
				}
				break wait
			}
		}
	}
	return best, bestErr >= 0
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageBinary {
			c.handleBinary(data)
			continue
		}
		t4 := c.nowUs()
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.log.Warn("undecodable message", "err", err)
			continue
		}
		if err := c.handleText(ctx, env, t4); err != nil {
			c.log.Warn("bad message", "type", env.Type, "err", err)
		}
	}
}

func (c *Client) handleBinary(data []byte) {
	if len(data) < audioHeaderLength || data[0] != binaryAudioChunk {
		return
	}
	c.mu.Lock()
	streaming, delay := c.streaming, c.delayMs
	c.mu.Unlock()
	if !streaming {
		return // the spec says to reject chunks without an active stream
	}
	if !c.filter.Synced() {
		c.mu.Lock()
		if !c.unsynced {
			c.unsynced = true
			c.log.Info("dropping audio until the clock is synchronized")
		}
		c.mu.Unlock()
		return
	}
	ts := int64(binary.BigEndian.Uint64(data[1:audioHeaderLength]))
	local := c.filter.ComputeClientTime(ts) - int64(delay)*1000
	c.h.Audio(c.localTime(local), data[audioHeaderLength:])
}

func (c *Client) handleText(ctx context.Context, env envelope, t4 int64) error {
	switch env.Type {
	case "server/time":
		var st serverTime
		if err := json.Unmarshal(env.Payload, &st); err != nil {
			return err
		}
		select {
		case c.times <- timeSample{resp: st, t4: t4}:
		default: // nobody waiting: stale answer
		}
	case "stream/start":
		var ss streamStart
		if err := json.Unmarshal(env.Payload, &ss); err != nil {
			return err
		}
		if ss.Player == nil {
			return nil
		}
		c.mu.Lock()
		c.streaming = true
		c.format = ss.Player.Format
		c.mu.Unlock()
		c.log.Info("stream started", "format", ss.Player.Format)
		c.h.StreamStarted(ss.Player.Format)
	case "stream/clear":
		var sr streamRoles
		if err := json.Unmarshal(env.Payload, &sr); err != nil {
			return err
		}
		if sr.affects("player") {
			c.h.StreamCleared()
		}
	case "stream/end":
		var sr streamRoles
		if err := json.Unmarshal(env.Payload, &sr); err != nil {
			return err
		}
		if sr.affects("player") {
			c.mu.Lock()
			was := c.streaming
			c.streaming = false
			c.mu.Unlock()
			if was {
				c.log.Info("stream ended")
				c.h.StreamEnded()
			}
		}
	case "server/command":
		return c.handleServerCommand(ctx, env.Payload)
	case "server/state":
		return c.handleServerState(env.Payload)
	case "group/update":
		var gu groupUpdate
		if err := json.Unmarshal(env.Payload, &gu); err != nil {
			return err
		}
		if gu.PlaybackState != nil {
			c.h.PlaybackStateChanged(*gu.PlaybackState == "playing")
		}
	default:
		c.log.Debug("unhandled message", "type", env.Type)
	}
	return nil
}

func (c *Client) handleServerCommand(ctx context.Context, payload json.RawMessage) error {
	var sc serverCommand
	if err := json.Unmarshal(payload, &sc); err != nil {
		return err
	}
	if sc.Player == nil {
		return nil
	}
	p := sc.Player
	c.mu.Lock()
	switch p.Command {
	case "volume":
		if p.Volume != nil {
			c.volume = max(0, min(100, *p.Volume))
		}
	case "mute":
		if p.Mute != nil {
			c.muted = *p.Mute
		}
	case "set_static_delay":
		if p.StaticDelayMs != nil {
			c.delayMs = max(0, min(5000, *p.StaticDelayMs))
		}
	default:
		c.mu.Unlock()
		return fmt.Errorf("unsupported player command %q", p.Command)
	}
	volume, muted, delay := c.volume, c.muted, c.delayMs
	c.mu.Unlock()

	if p.Command == "set_static_delay" {
		c.h.StaticDelayChanged(delay)
	} else {
		c.h.VolumeChanged(volume, muted)
	}
	// The spec requires a state update after every change.
	return c.sendState(ctx)
}

func (c *Client) handleServerState(payload json.RawMessage) error {
	var ss serverState
	if err := json.Unmarshal(payload, &ss); err != nil {
		return err
	}
	if ss.Controller != nil {
		c.mu.Lock()
		c.controller = *ss.Controller
		c.mu.Unlock()
		c.h.ControllerStateChanged(*ss.Controller)
	}
	if len(ss.Metadata) > 0 && string(ss.Metadata) != "null" {
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(ss.Metadata, &delta); err != nil {
			return err
		}
		c.mu.Lock()
		for k, v := range delta {
			if string(v) == "null" {
				delete(c.metadata, k) // null clears a field
			} else {
				c.metadata[k] = v
			}
		}
		merged, err := json.Marshal(c.metadata)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		var m Metadata
		if err := json.Unmarshal(merged, &m); err != nil {
			return err
		}
		c.h.MetadataChanged(m)
	}
	return nil
}

// Command sends a controller command such as "next", "previous", "play",
// "pause" or "stop" for the client's own group.
func (c *Client) Command(ctx context.Context, command string) error {
	c.mu.Lock()
	supported := slices.Contains(c.controller.SupportedCommands, command)
	c.mu.Unlock()
	if !supported {
		return fmt.Errorf("sendspin: command %q not offered by the server", command)
	}
	return c.send(ctx, "client/command", clientCommand{Controller: &controllerCommand{Command: command}})
}

// TogglePlayback sends "pause" while the group plays and "play" otherwise.
// Sendspin has no toggle command, so the decision uses the last known state.
func (c *Client) TogglePlayback(ctx context.Context, playing bool) error {
	if playing {
		return c.Command(ctx, "pause")
	}
	return c.Command(ctx, "play")
}

// RequestFormat asks the server to switch the stream to another format. The
// server answers with a new stream/start.
func (c *Client) RequestFormat(ctx context.Context, f Format) error {
	return c.send(ctx, "stream/request-format", map[string]Format{"player": f})
}

// Format returns the format of the current stream.
func (c *Client) Format() (Format, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.format, c.streaming
}

// Connected reports whether a session to the server is established.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// ReportVolume reports a volume change made on the headphone.
func (c *Client) ReportVolume(ctx context.Context, volume int, muted bool) error {
	c.mu.Lock()
	c.volume = max(0, min(100, volume))
	c.muted = muted
	c.mu.Unlock()
	return c.sendState(ctx)
}

// SetAvailable marks the player as available or in use elsewhere, e.g. when
// the headphone is connected to a phone instead of a relay node.
func (c *Client) SetAvailable(ctx context.Context, available bool) error {
	c.mu.Lock()
	changed := c.available != available
	c.available = available
	c.mu.Unlock()
	if !changed {
		return nil
	}
	return c.sendState(ctx)
}

// ServerTimeUs returns the current server clock, or false before clock sync.
func (c *Client) ServerTimeUs() (int64, bool) {
	if !c.filter.Synced() {
		return 0, false
	}
	return c.filter.ComputeServerTime(c.nowUs()), true
}
