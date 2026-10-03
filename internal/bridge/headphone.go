package bridge

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/handover"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/sendspin"
	"github.com/sl1288/a2dp-relay-bridge/internal/wyoming"
)

// headphone is the runtime state of one paired headphone.
type headphone struct {
	b   *Bridge
	log *slog.Logger
	buf *pcmBuffer
	op  sync.Mutex // serializes stream start, restart and handover

	mu        sync.Mutex
	cfg       config.Headphone
	addr      node.Addr
	bleAddr   *node.Addr
	irk       *node.IRK  // resolves the headphone's rotating BLE addresses
	irkAlt    *node.IRK  // irk reversed, tried while the byte order is unverified
	irkFound  int        // unverified IRK resolved an address: 1 as stored, 2 reversed
	bleID     *node.Addr // identity address learned with the IRK
	engine    *handover.Engine
	lastSeen  time.Time // last BLE advertisement from any node
	rssi      map[string]rssiSample
	client    *sendspin.Client
	stopCli   context.CancelFunc
	connected bool // Sendspin session established

	// Stream state reported by Music Assistant.
	streaming  bool
	format     sendspin.Format
	playing    bool
	meta       sendspin.Metadata
	controller sendspin.ControllerState
	idleSince  time.Time // when the headphone last had a node but no stream; zero while streaming
	played     bool      // audio of the current stream reached the headphone

	// Bluetooth side.
	node         *nodeSession // node currently assigned to this headphone
	streamer     *streamer
	streamErr    string
	errCode      string // translatable reason instead of streamErr (errNodeBusy, errNoNode)
	errArgs      []string
	lastRetry    time.Time // last attempt to start a stream that waits for a node
	failures     int       // consecutive failed stream starts
	failingSince time.Time // first of these failures
	unavailable  bool      // reported as unavailable to Music Assistant
	absVolume    bool      // headphone reported absolute volume
	lastVolume   int       // last volume sent to or received from the headphone (0..100)
	battery      *BatteryStatus
	ldacQ        ldac.Quality // adaptive LDAC quality last reached on ldacLink
	ldacLink     *nodeSession // nil: no quality learned on the current link
	codecRetry   time.Time    // last reconnect to get the preferred codec

	sat     *wyoming.Satellite // voice assistant satellite for Home Assistant
	stopSat context.CancelFunc
}

// Retries after a failed stream: keep trying for retryWindow, but give up
// early once no node has seen the headphone's BLE advertisements for retryUnseen.
const (
	retryWindow = 3 * time.Minute // some headphones refuse connections for ~2 min after an abrupt loss
	retryUnseen = 30 * time.Second
)

// codecRetryInterval limits reconnects for the preferred codec, in case the
// headphone keeps choosing another one.
const codecRetryInterval = 5 * time.Minute

// mayRetryCodec reports whether reconnecting can get codec want: the
// headphone must offer it (if its codecs are known), and the last attempt must
// be a while ago. A true result counts as an attempt.
func (h *headphone) mayRetryCodec(offered []string, want string) bool {
	if len(offered) > 0 && !slices.Contains(offered, want) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.codecRetry.IsZero() && time.Since(h.codecRetry) < codecRetryInterval {
		return false
	}
	h.codecRetry = time.Now()
	return true
}

type rssiSample struct {
	RSSI int       `json:"rssi"`
	At   time.Time `json:"at"`
}

func newHeadphone(b *Bridge, cfg config.Headphone) *headphone {
	h := &headphone{
		b:          b,
		log:        b.log.With("headphone", cfg.Name),
		buf:        newPCMBuffer(),
		engine:     handover.New(b.handoverConfig()),
		rssi:       map[string]rssiSample{},
		lastVolume: -1,
	}
	h.setConfig(cfg)
	return h
}

func (h *headphone) setConfig(cfg config.Headphone) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	h.addr, _ = node.ParseAddr(cfg.Addr)
	h.bleAddr = nil
	if a, err := node.ParseAddr(cfg.BLEAddr); err == nil {
		h.bleAddr = &a
	}
	h.irk, h.irkAlt, h.irkFound, h.bleID = nil, nil, 0, nil
	if k, err := node.ParseIRK(cfg.BLEIRK); err == nil {
		h.irk = &k
		if cfg.BLEIRKUnverified {
			r := k.Reversed()
			h.irkAlt = &r
		}
	}
	if a, err := node.ParseAddr(cfg.BLEIdentity); err == nil {
		h.bleID = &a
	}
}

func (h *headphone) id() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.ID
}

// How an advertisement was attributed to a headphone (identifyLocked).
const (
	viaAddr     = "addr"     // configured BLE address
	viaIdentity = "identity" // identity address learned with the IRK
	viaIRK      = "irk"      // rotating address resolved by the IRK
	viaName     = "name"     // name prefix
)

// identifyLocked tells whether and how a BLE advertisement belongs to this
// headphone ("" if not); found is set when an unverified IRK resolved it
// (1 as stored, 2 reversed). Caller holds h.mu.
func (h *headphone) identifyLocked(a node.Adv) (via string, found int) {
	if h.bleAddr != nil && *h.bleAddr == a.Addr {
		return viaAddr, 0
	}
	// A node bonded over BLE reports the headphone under its identity
	// address; the others see rotating addresses that the IRK resolves.
	if h.bleID != nil && *h.bleID == a.Addr {
		return viaIdentity, 0
	}
	if h.irk != nil && node.IsRPA(a.Addr, a.AddrType) {
		if h.irk.Resolves(a.Addr) {
			if h.irkAlt != nil {
				found = 1
			}
			return viaIRK, found
		}
		if h.irkAlt != nil && h.irkAlt.Resolves(a.Addr) {
			return viaIRK, 2
		}
	}
	if h.cfg.BLEName != "" && a.Name != "" && strings.HasPrefix(a.Name, h.cfg.BLEName) {
		return viaName, 0
	}
	return "", 0
}

// matchesAdv reports whether a BLE advertisement belongs to this headphone.
func (h *headphone) matchesAdv(a node.Adv) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	via, found := h.identifyLocked(a)
	if found != 0 {
		h.irkFound = found
	}
	return via != ""
}

// identify tells how an advertisement belongs to this headphone ("" if not),
// without side effects (status display).
func (h *headphone) identify(a node.Adv) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	via, _ := h.identifyLocked(a)
	return via
}

// takeIRKFound returns how an unverified IRK resolved an address (0: not yet).
func (h *headphone) takeIRKFound() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.irkFound
	h.irkFound = 0
	if v != 0 {
		h.irkAlt = nil // settled; confirmIRK stores the right order
	}
	return v
}

// hasBLEIdentity reports whether reception can be measured at all.
func (h *headphone) hasBLEIdentity() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bleAddr != nil || h.cfg.BLEName != "" || h.irk != nil
}

func (h *headphone) scanFilters() []node.ScanFilter {
	h.mu.Lock()
	defer h.mu.Unlock()
	var f []node.ScanFilter
	if h.bleAddr != nil {
		a := *h.bleAddr
		f = append(f, node.ScanFilter{Addr: &a})
	}
	if h.irk != nil {
		k := *h.irk
		f = append(f, node.ScanFilter{IRK: &k})
	}
	if h.irkAlt != nil {
		k := *h.irkAlt
		f = append(f, node.ScanFilter{IRK: &k})
	}
	if h.bleID != nil {
		a := *h.bleID
		f = append(f, node.ScanFilter{Addr: &a})
	}
	if h.cfg.BLEName != "" {
		f = append(f, node.ScanFilter{NamePrefix: h.cfg.BLEName})
	}
	return f
}

// --- Sendspin client lifecycle ---------------------------------------------

func (h *headphone) startClient(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	h.stopCli = cancel
	h.client = sendspin.New(sendspin.Config{
		URL:             h.b.cfg.MusicAssistant.SendspinURL,
		ClientID:        h.cfg.ClientID,
		Name:            h.cfg.Name,
		ProductName:     "A2DP relay headphone",
		Manufacturer:    "a2dp-relay",
		SoftwareVersion: Version,
		Formats: []sendspin.Format{
			{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16},
			{Codec: "pcm", Channels: 2, SampleRate: 44100, BitDepth: 16},
		},
		BufferCapacity:     2 << 20,
		RequiredLeadTimeMs: 800,
		MinBufferMs:        800,
		Volume:             h.cfg.Volume,
		StaticDelayMs:      h.cfg.StaticDelayMs,
	}, h, h.log)
	c := h.client
	go func() { _ = c.Run(cctx) }()
	h.log.Info("player published in Music Assistant")
}

func (h *headphone) stopClient() {
	h.mu.Lock()
	stop := h.stopCli
	h.client, h.stopCli = nil, nil
	h.connected = false
	h.mu.Unlock()
	if stop != nil {
		stop()
		h.log.Info("player withdrawn from Music Assistant")
	}
}

func (h *headphone) sendspin() *sendspin.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client
}

// --- sendspin.Handler --------------------------------------------------------

func (h *headphone) ConnectionChanged(connected bool, _ string) {
	h.mu.Lock()
	h.connected = connected
	h.mu.Unlock()
	h.b.notify()
}

func (h *headphone) StreamStarted(f sendspin.Format) {
	h.mu.Lock()
	h.streaming, h.format = true, f
	h.idleSince = time.Time{}
	h.played = false
	h.mu.Unlock()
	h.buf.reset(f.BytesPerFrame(), f.SampleRate)
	h.b.streamStarted(h)
}

func (h *headphone) StreamCleared() {
	h.mu.Lock()
	f := h.format
	h.mu.Unlock()
	h.buf.reset(f.BytesPerFrame(), f.SampleRate)
	h.b.streamCleared(h)
}

func (h *headphone) StreamEnded() {
	h.mu.Lock()
	h.streaming = false
	h.idleSince = time.Now()
	h.mu.Unlock()
	h.buf.reset(0, 0)
	h.b.streamEnded(h)
}

func (h *headphone) Audio(playAt time.Time, data []byte) { h.buf.push(playAt, data) }

func (h *headphone) VolumeChanged(volume int, muted bool) {
	v := volume
	if muted {
		v = 0
	}
	if _, err := h.b.store.Update(h.id(), false, func(c *config.Headphone) { c.Volume = volume }); err != nil {
		h.log.Warn("saving volume failed", "err", err)
	}
	h.b.setHeadphoneVolume(h, v)
}

func (h *headphone) StaticDelayChanged(ms int) {
	if _, err := h.b.store.Update(h.id(), false, func(c *config.Headphone) { c.StaticDelayMs = ms }); err != nil {
		h.log.Warn("saving static delay failed", "err", err)
	}
}

func (h *headphone) MetadataChanged(m sendspin.Metadata) {
	h.mu.Lock()
	h.meta = m
	h.mu.Unlock()
	h.b.notify()
}

func (h *headphone) PlaybackStateChanged(playing bool) {
	h.mu.Lock()
	h.playing = playing
	h.mu.Unlock()
	h.b.notify()
}

func (h *headphone) ControllerStateChanged(s sendspin.ControllerState) {
	h.mu.Lock()
	h.controller = s
	h.mu.Unlock()
}
