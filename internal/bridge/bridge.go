// Package bridge ties Music Assistant (Sendspin), the relay nodes and the
// headphones together: it publishes one player per paired headphone, streams
// its audio through the node with the best reception and forwards headphone
// buttons and volume back to Music Assistant.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/handover"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// Version is the bridge version, reported to Music Assistant.
var Version = "0.1.0-dev"

// Bridge is the running relay bridge.
type Bridge struct {
	cfg   config.Config
	store *config.Store
	log   *slog.Logger
	ctx   context.Context

	nodes []*nodeSession

	mu         sync.Mutex
	headphones map[string]*headphone
	order      []string // headphone IDs in display order
	pairing    *pairingJob
	blePairing *blePairJob
	subs       map[chan struct{}]struct{}

	voiceMu sync.Mutex
	voice   map[*nodeSession]*voiceSession // running voice assistant sessions
}

// New creates a bridge; call Run to start it.
func New(cfg config.Config, store *config.Store, log *slog.Logger) *Bridge {
	b := &Bridge{
		cfg:        cfg,
		store:      store,
		log:        log,
		headphones: map[string]*headphone{},
		subs:       map[chan struct{}]struct{}{},
		voice:      map[*nodeSession]*voiceSession{},
	}
	for _, nc := range cfg.Nodes {
		b.nodes = append(b.nodes, newNodeSession(b, nc))
	}
	for _, hc := range store.Headphones() {
		if hc.WyomingPort == 0 {
			hc = b.assignWyomingPort(hc)
		}
		b.headphones[hc.ID] = newHeadphone(b, hc)
		b.order = append(b.order, hc.ID)
	}
	return b
}

// wyomingBasePort is the first port of the headphones' voice satellites.
const wyomingBasePort = 10700

// assignWyomingPort gives a headphone the lowest free satellite port and
// stores it, so that Home Assistant finds it at the same port after restarts.
func (b *Bridge) assignWyomingPort(hc config.Headphone) config.Headphone {
	used := map[int]bool{}
	for _, o := range b.store.Headphones() {
		used[o.WyomingPort] = true
	}
	port := wyomingBasePort
	for used[port] {
		port++
	}
	if updated, err := b.store.Update(hc.ID, false, func(c *config.Headphone) { c.WyomingPort = port }); err == nil {
		return updated
	}
	hc.WyomingPort = port
	return hc
}

func (b *Bridge) handoverConfig() handover.Config {
	h := b.cfg.Handover
	offsets := map[string]float64{}
	for _, n := range b.cfg.Nodes {
		offsets[n.Address] = n.RSSIOffset
	}
	return handover.Config{TimeConstant: h.TimeConstant, Hysteresis: h.HysteresisDB, Hold: h.Hold,
		MinDwell: h.MinDwell, StaleAfter: h.StaleAfter, MinRSSI: h.MinRSSI, Offsets: offsets}
}

// Run starts all node sessions and the control loop; it returns when ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	b.ctx = ctx
	var wg sync.WaitGroup
	for _, h := range b.allHeadphones() {
		h.startSatellite(ctx)
	}
	for _, n := range b.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.run(ctx)
		}()
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, h := range b.allHeadphones() {
				b.stopStreamer(h)
				h.stopClient()
			}
			wg.Wait()
			return ctx.Err()
		case <-t.C:
			b.tick()
		}
	}
}

func (b *Bridge) allHeadphones() []*headphone {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*headphone, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, b.headphones[id])
	}
	return out
}

func (b *Bridge) headphoneByID(id string) *headphone {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.headphones[id]
}

func (b *Bridge) headphoneByAddr(a node.Addr) *headphone {
	for _, h := range b.allHeadphones() {
		h.mu.Lock()
		match := h.addr == a
		h.mu.Unlock()
		if match {
			return h
		}
	}
	return nil
}

func (b *Bridge) nodeByID(id string) *nodeSession {
	for _, n := range b.nodes {
		if n.id() == id {
			return n
		}
	}
	return nil
}

// notify wakes web interface subscribers.
func (b *Bridge) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a channel that is signalled on state changes.
func (b *Bridge) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// scanFilters is the union of the BLE identities of all headphones.
func (b *Bridge) scanFilters() []node.ScanFilter {
	var f []node.ScanFilter
	for _, h := range b.allHeadphones() {
		f = append(f, h.scanFilters()...)
	}
	return f
}

func (b *Bridge) refreshScans() {
	for _, n := range b.nodes {
		if n.online() {
			if err := n.applyScan(); err != nil {
				n.log.Warn("updating BLE scan failed", "err", err)
			}
		}
	}
}

// --- events from nodes ------------------------------------------------------

func (b *Bridge) observeAdv(n *nodeSession, a node.Adv) {
	now := time.Now()
	for _, h := range b.allHeadphones() {
		if !h.matchesAdv(a) {
			continue
		}
		if v := h.takeIRKFound(); v != 0 {
			go b.confirmIRK(h, v == 2)
		}
		h.mu.Lock()
		h.engine.Observe(n.id(), float64(a.RSSI), now)
		h.rssi[n.id()] = rssiSample{RSSI: int(a.RSSI), At: now}
		h.lastSeen = now
		started := h.client != nil
		unavailable := h.unavailable
		h.mu.Unlock()
		if !started {
			h.startClient(b.ctx)
			b.notify()
		}
		if unavailable {
			go b.setUnavailable(h, false) // it is back
		}
	}
}

func (b *Bridge) nodeOffline(n *nodeSession) {
	for _, h := range b.allHeadphones() {
		h.mu.Lock()
		h.engine.Remove(n.id())
		delete(h.rssi, n.id())
		h.mu.Unlock()
	}
	b.notify()
}

func (b *Bridge) onLinkEvent(n *nodeSession, e node.A2DPEvent) {
	switch e.Event {
	case node.EvAudioStarted, node.EvAudioSuspended:
		go func() { _ = n.applyScan() }() // scan duty cycle depends on streaming
	case node.EvConnected:
		// A headphone may connect on its own (e.g. when switched on). Adopt
		// the node for it unless it is busy.
		if h := b.headphoneByAddr(e.Addr); h != nil {
			b.assign(h, n, false)
			b.rememberBond(h, n)
			go b.setUnavailable(h, false)
			// Music Assistant still plays to it, e.g. after a manual reconnect.
			// Failed streams are restarted by their own retry instead.
			h.mu.Lock()
			resume := h.streaming && h.streamer == nil && h.failures == 0
			h.mu.Unlock()
			if resume {
				h.log.Info("headphone connected during playback, resuming")
				go b.startStreaming(h, true)
			}
		}
	case node.EvDisconnected:
		for _, h := range b.allHeadphones() {
			h.mu.Lock()
			if h.node == n && h.streamer == nil {
				h.node = nil
			}
			if h.addr == e.Addr && h.ldacLink == n {
				h.ldacLink = nil // a new link starts at the default quality
			}
			h.mu.Unlock()
		}
		if !b.isStreamingOn(n) {
			n.mu.Lock()
			n.assignedTo = ""
			n.mu.Unlock()
		}
	}
	b.notify()
}

// isStreamingOn reports whether some headphone streams through n.
func (b *Bridge) isStreamingOn(n *nodeSession) bool {
	for _, h := range b.allHeadphones() {
		h.mu.Lock()
		busy := h.node == n && h.streamer != nil
		h.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

func (b *Bridge) headphoneOnNode(n *nodeSession) *headphone {
	l := n.currentLink()
	if h := b.headphoneByAddr(l.Peer); h != nil && !l.Peer.IsZero() {
		return h
	}
	n.mu.Lock()
	id := n.assignedTo
	n.mu.Unlock()
	return b.headphoneByID(id)
}

func (b *Bridge) onKey(n *nodeSession, k node.AVRCPKey) {
	h := b.headphoneOnNode(n)
	if h == nil {
		return
	}
	c := h.sendspin()
	if c == nil {
		return
	}
	h.mu.Lock()
	playing := h.playing
	vol := h.lastVolume
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
	defer cancel()
	var err error
	switch k.Key {
	case node.KeyPlay:
		err = c.Command(ctx, "play")
	case node.KeyPause:
		err = c.Command(ctx, "pause")
	case node.KeyStop:
		err = c.Command(ctx, "stop")
	case node.KeyForward:
		err = c.Command(ctx, "next")
	case node.KeyBackward:
		err = c.Command(ctx, "previous")
	case node.KeyVolumeUp, node.KeyVolumeDown:
		// Only headphones without absolute volume send volume keys.
		if vol < 0 {
			vol = 40
		}
		step := 6
		if k.Key == node.KeyVolumeDown {
			step = -6
		}
		vol = max(0, min(100, vol+step))
		h.mu.Lock()
		h.lastVolume = vol
		h.mu.Unlock()
		err = c.ReportVolume(ctx, vol, false)
	default:
		return
	}
	h.log.Info("headphone button", "key", node.KeyName(k.Key), "playing", playing, "err", err)
}

func (b *Bridge) onVolume(n *nodeSession, v node.Volume) {
	h := b.headphoneOnNode(n)
	if h == nil {
		return
	}
	vol := int(math.Round(float64(v.Volume) * 100 / 127))
	h.mu.Lock()
	h.absVolume = true
	changed := vol != h.lastVolume
	h.lastVolume = vol
	c := h.client
	h.mu.Unlock()
	if !changed || c == nil || v.Origin != 0 {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
	defer cancel()
	if err := c.ReportVolume(ctx, vol, false); err != nil {
		h.log.Debug("reporting volume failed", "err", err)
	}
	if _, err := b.store.Update(h.id(), false, func(c *config.Headphone) { c.Volume = vol }); err != nil {
		h.log.Warn("saving volume failed", "err", err)
	}
}

// onBattery records a battery report. Nodes replay their last report when the
// bridge connects, so an older report never replaces a newer one.
func (b *Bridge) onBattery(n *nodeSession, r node.Battery) {
	h := b.headphoneByAddr(r.Addr)
	if h == nil {
		return
	}
	at := time.Now().Add(-r.Age)
	h.mu.Lock()
	prev := h.battery
	if prev != nil && prev.At.After(at) {
		h.mu.Unlock()
		return
	}
	h.battery = &BatteryStatus{Percent: r.Percent, Charging: r.Charging, Source: r.Source.String(), At: at}
	h.mu.Unlock()
	if prev == nil || prev.Percent != r.Percent || !equalPtr(prev.Charging, r.Charging) {
		args := []any{"percent", r.Percent, "source", r.Source.String(), "node", n.name()}
		if r.Charging != nil {
			args = append(args, "charging", *r.Charging)
		}
		h.log.Info("battery level", args...)
	}
	b.notify()
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// setHeadphoneVolume applies a volume set in Music Assistant (0..100).
func (b *Bridge) setHeadphoneVolume(h *headphone, vol int) {
	h.mu.Lock()
	n := h.node
	changed := vol != h.lastVolume
	h.lastVolume = vol
	h.mu.Unlock()
	if n == nil || !changed {
		return
	}
	if c, err := n.getConn(); err == nil {
		_ = c.SetVolume(uint8(math.Round(float64(vol) * 127 / 100)))
	}
}

func (b *Bridge) rememberBond(h *headphone, n *nodeSession) {
	bt := n.btAddr()
	if bt.IsZero() {
		return
	}
	cfg, err := b.store.Update(h.id(), false, func(c *config.Headphone) {
		if !slices.Contains(c.PairedNodes, bt.String()) {
			c.PairedNodes = append(c.PairedNodes, bt.String())
		}
	})
	if err == nil {
		h.setConfig(cfg)
	}
}

// --- node assignment and streaming -----------------------------------------

// assign makes n the node of h if it is free (or force is set).
func (b *Bridge) assign(h *headphone, n *nodeSession, force bool) bool {
	id := h.id()
	n.mu.Lock()
	other := n.assignedTo
	n.mu.Unlock()
	if other != "" && other != id && !force {
		// A node serves one headphone. One that has been idle for a while
		// gives it up to a headphone that wants to play.
		o := b.headphoneByID(other)
		if o != nil && !b.idleLongEnough(o) {
			return false
		}
		if o != nil {
			b.evict(n, o, h)
		}
	}
	n.mu.Lock()
	n.assignedTo = id
	n.mu.Unlock()
	h.mu.Lock()
	old := h.node
	h.node = n
	h.mu.Unlock()
	if old != nil && old != n {
		b.release(old, id)
	}
	return true
}

// Error codes shown translated in the web interface.
const (
	errNodeBusy = "node_busy" // args: node, headphone using it, seconds until it is free ("" while it plays)
	errNoNode   = "no_node"   // no online node holds a bond with the headphone
)

// noNodeReason explains why pickNode found no node for h.
func (b *Bridge) noNodeReason(h *headphone) (string, []string) {
	h.mu.Lock()
	paired := slices.Clone(h.cfg.PairedNodes)
	id := h.cfg.ID
	h.mu.Unlock()
	for _, n := range b.nodes {
		if !n.online() || !slices.Contains(paired, n.btAddr().String()) {
			continue
		}
		n.mu.Lock()
		other := n.assignedTo
		n.mu.Unlock()
		o := b.headphoneByID(other)
		if other == "" || other == id || o == nil {
			continue
		}
		wait := ""
		o.mu.Lock()
		if !o.streaming && o.streamer == nil && !o.idleSince.IsZero() {
			wait = fmt.Sprint(max(0, int((b.cfg.Streaming.YieldAfter - time.Since(o.idleSince)).Seconds())))
		}
		name := o.cfg.Name
		o.mu.Unlock()
		return errNodeBusy, []string{n.name(), name, wait}
	}
	return errNoNode, nil
}

// idleLongEnough reports whether headphone o has played nothing for
// streaming.yield_after and may give up its node.
func (b *Bridge) idleLongEnough(o *headphone) bool {
	if o.voice() != nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.streaming && o.streamer == nil && !o.idleSince.IsZero() &&
		time.Since(o.idleSince) > b.cfg.Streaming.YieldAfter
}

// evict disconnects idle headphone o from node n so that h can use it.
func (b *Bridge) evict(n *nodeSession, o, h *headphone) {
	o.mu.Lock()
	addr := o.addr
	if o.node == n {
		o.node = nil
		o.idleSince = time.Time{}
	}
	o.mu.Unlock()
	b.release(n, o.id())
	o.log.Info("giving up the node to another headphone", "node", n.name(), "to", h.id())
	if l := n.currentLink(); l.Peer == addr && l.State != node.LinkIdle {
		if c, err := n.getConn(); err == nil {
			_ = c.Disconnect(addr)
			_, _ = n.waitLink(b.ctx, 6*time.Second, func(l linkInfo) bool { return l.State == node.LinkIdle })
		}
	}
}

func (b *Bridge) release(n *nodeSession, id string) {
	n.mu.Lock()
	if n.assignedTo == id {
		n.assignedTo = ""
	}
	n.mu.Unlock()
}

// pickNode chooses the node for a new stream: the current one, else the one
// with the best reception, else any free online node that holds a bond.
func (b *Bridge) pickNode(h *headphone) *nodeSession {
	h.mu.Lock()
	cur := h.node
	d := h.engine.Decide(time.Now())
	paired := slices.Clone(h.cfg.PairedNodes)
	addr := h.addr
	id := h.cfg.ID
	h.mu.Unlock()

	pairing := b.pairingNode()
	free := func(n *nodeSession) bool {
		if n == pairing {
			return false // a pairing owns this node right now
		}
		n.mu.Lock()
		online, other := n.conn != nil, n.assignedTo
		n.mu.Unlock()
		if !online {
			return false
		}
		if other == "" || other == id {
			return true
		}
		o := b.headphoneByID(other)
		return o == nil || b.idleLongEnough(o)
	}
	if cur != nil && free(cur) && (d.Active == "" || d.Active == cur.id()) {
		return cur
	}
	if n := b.nodeByID(d.Active); n != nil && free(n) {
		return n
	}
	for _, n := range b.nodes {
		if free(n) && n.currentLink().Peer == addr {
			return n
		}
	}
	for _, n := range b.nodes {
		if free(n) && slices.Contains(paired, n.btAddr().String()) {
			return n
		}
	}
	return nil
}

func (b *Bridge) streamStarted(h *headphone) { b.startStreaming(h, true) }

func (b *Bridge) streamCleared(h *headphone) {
	h.mu.Lock()
	active := h.streamer != nil
	h.mu.Unlock()
	if active {
		b.startStreaming(h, true)
	}
}

func (b *Bridge) streamEnded(h *headphone) {
	b.stopStreamer(h)
	h.mu.Lock()
	n := h.node
	h.mu.Unlock()
	if n != nil {
		if c, err := n.getConn(); err == nil {
			_ = c.Flush()
		}
	}
	b.notify()
}

func (b *Bridge) stopStreamer(h *headphone) {
	h.mu.Lock()
	s := h.streamer
	h.streamer = nil
	h.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

// startStreaming (re)starts the streamer of h on the best node.
func (b *Bridge) startStreaming(h *headphone, fresh bool) {
	h.op.Lock()
	defer h.op.Unlock()
	b.startStreamingLocked(h, fresh)
}

// startStreamingLocked is startStreaming with h.op held.
func (b *Bridge) startStreamingLocked(h *headphone, fresh bool) {
	b.stopStreamer(h)
	h.mu.Lock()
	streaming := h.streaming
	h.mu.Unlock()
	if !streaming || b.ctx.Err() != nil {
		return
	}
	n := b.pickNode(h)
	if n == nil {
		code, args := b.noNodeReason(h)
		h.mu.Lock()
		repeated := h.errCode == code
		h.streamErr, h.errCode, h.errArgs = "", code, args
		h.mu.Unlock()
		if repeated {
			// Retried while waiting: already logged.
		} else if code == errNodeBusy {
			h.log.Info("waiting for a free node", "node", args[0], "used_by", args[1])
		} else {
			h.log.Warn("cannot stream: no online node holds a bond with this headphone")
		}
		b.notify()
		return
	}
	b.assign(h, n, false)

	ctx, cancel := context.WithCancel(b.ctx)
	s := &streamer{h: h, n: n, cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	h.streamer = s
	h.streamErr, h.errCode, h.errArgs = "", "", nil
	h.mu.Unlock()
	go func() {
		err := s.run(ctx, fresh)
		close(s.done)
		if ctx.Err() != nil {
			return // stopped on purpose
		}
		h.mu.Lock()
		current := h.streamer == s
		if current {
			h.streamer = nil
			if err != nil {
				h.streamErr, h.errCode, h.errArgs = err.Error(), "", nil
			}
		}
		h.mu.Unlock()
		if !current {
			return
		}
		if errors.Is(err, errFormatRequested) || errors.Is(err, errReset) {
			return // a new stream/start follows and restarts streaming
		}
		if errors.Is(err, errLinkLost) {
			h.log.Warn("Bluetooth link lost, failing over", "node", n.name())
			h.mu.Lock()
			h.engine.ActiveLost()
			if h.node == n {
				h.node = nil
			}
			h.mu.Unlock()
			b.release(n, h.id())
		} else {
			h.log.Warn("streaming failed", "node", n.name(), "err", err)
		}
		h.mu.Lock()
		h.failures++
		failures := h.failures
		if failures == 1 {
			h.failingSince = time.Now()
		}
		failingFor := time.Since(h.failingSince)
		hasBLE := h.bleAddr != nil || h.cfg.BLEName != ""
		unseen := hasBLE && time.Since(h.lastSeen) > retryUnseen
		h.mu.Unlock()
		// After an abrupt loss a headphone may refuse connections for a minute,
		// so keep trying for a while - unless it is clearly gone (switched off,
		// back in its case, out of range: no BLE advertisements either).
		if failingFor > retryWindow || (failures >= 3 && unseen) {
			h.log.Warn("headphone unreachable, stopping playback", "tries", failures,
				"for", failingFor.Round(time.Second), "ble_unseen", unseen)
			b.setUnavailable(h, true)
			b.notify()
			return
		}
		time.Sleep(min(time.Duration(failures)*2*time.Second, 10*time.Second))
		h.mu.Lock()
		restarted := h.streamer != nil // the headphone reconnected and resumed meanwhile
		h.mu.Unlock()
		if !restarted {
			b.startStreaming(h, false)
		}
		b.notify()
	}()
	b.notify()
}

// setUnavailable tells Music Assistant whether the headphone can play. An
// unavailable player is treated as in use elsewhere and its playback stops.
func (b *Bridge) setUnavailable(h *headphone, unavailable bool) {
	h.mu.Lock()
	changed := h.unavailable != unavailable
	h.unavailable = unavailable
	if !unavailable {
		h.failures = 0
	}
	c := h.client
	h.mu.Unlock()
	if !changed || c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
	defer cancel()
	if err := c.SetAvailable(ctx, !unavailable); err != nil {
		h.log.Warn("reporting availability failed", "err", err)
	}
	h.log.Info("availability changed", "available", !unavailable)
}

// handover moves a running stream of h to node to.
func (b *Bridge) handover(h *headphone, to *nodeSession, reason string) {
	h.op.Lock()
	defer h.op.Unlock()
	h.mu.Lock()
	from := h.node
	addr := h.addr
	h.mu.Unlock()
	if from == to {
		return
	}
	h.log.Info("handover", "from", nodeName(from), "to", to.name(), "reason", reason)
	b.stopStreamer(h)
	if from != nil {
		// Most headphones accept only one source: release it first.
		if c, err := from.getConn(); err == nil {
			_ = c.Disconnect(addr)
			_, _ = from.waitLink(b.ctx, 5*time.Second, func(l linkInfo) bool { return l.State == node.LinkIdle })
		}
		b.release(from, h.id())
	}
	if !b.assign(h, to, false) {
		h.log.Warn("handover target busy", "node", to.name())
		return
	}
	b.startStreamingLocked(h, false)
}

func nodeName(n *nodeSession) string {
	if n == nil {
		return "-"
	}
	return n.name()
}

// tick runs once per second: presence, handover, suspend and idle release.
func (b *Bridge) tick() {
	now := time.Now()
	for _, h := range b.allHeadphones() {
		h.mu.Lock()
		hasBLE := h.bleAddr != nil || h.cfg.BLEName != ""
		absent := hasBLE && now.Sub(h.lastSeen) > b.cfg.Streaming.AbsentAfter
		streaming, playingStream := h.streamer != nil, h.streaming
		// A stream that waits for a busy node: try again every few seconds,
		// the other headphone may have become idle long enough.
		retry := playingStream && !streaming && h.errCode == errNodeBusy && now.Sub(h.lastRetry) > 5*time.Second
		if retry {
			h.lastRetry = now
		}
		n := h.node
		if n != nil && !playingStream && h.idleSince.IsZero() {
			h.idleSince = now // e.g. the headphone connected on its own
		}
		idleFor := time.Duration(0)
		if !h.idleSince.IsZero() {
			idleFor = now.Sub(h.idleSince)
		}
		client := h.client
		h.mu.Unlock()

		// Presence: publish the player while the headphone is around.
		switch {
		case !hasBLE && client == nil:
			h.startClient(b.ctx)
		case absent && client != nil && !playingStream && n == nil:
			h.stopClient()
		}

		if retry {
			go b.startStreaming(h, true)
		}

		// Handover while streaming.
		if streaming {
			h.mu.Lock()
			d := h.engine.Decide(now)
			h.mu.Unlock()
			if d.Switch && n != nil && d.Active != n.id() {
				if to := b.nodeByID(d.Active); to != nil {
					go b.handover(h, to, d.Reason)
				}
			}
			continue
		}

		// After playback stopped: suspend the audio stream, later release the headphone.
		if n != nil && !playingStream {
			l := n.currentLink()
			c, err := n.getConn()
			if err != nil {
				continue
			}
			if l.State == node.LinkStreaming && idleFor > b.cfg.Streaming.SuspendAfter {
				_ = c.MediaSuspend()
			}
			if l.State >= node.LinkConnected && idleFor > b.cfg.Streaming.IdleDisconnect {
				h.log.Info("releasing idle headphone", "node", n.name(), "idle", idleFor.Round(time.Second))
				_ = c.Disconnect(node.Addr{})
				b.release(n, h.id())
				h.mu.Lock()
				h.node = nil
				h.idleSince = time.Time{}
				h.mu.Unlock()
			}
		}
	}
	b.notify()
}

// --- pairing ----------------------------------------------------------------

type pairingJob struct {
	node   *nodeSession
	addr   node.Addr
	result chan node.Auth
}

func (b *Bridge) onAuth(n *nodeSession, a node.Auth) {
	b.mu.Lock()
	job := b.pairing
	b.mu.Unlock()
	if job != nil && job.node == n && job.addr == a.Addr {
		select {
		case job.result <- a:
		default:
		}
	}
	if a.Status == 0 {
		if h := b.headphoneByAddr(a.Addr); h != nil {
			b.rememberBond(h, n)
		}
	}
	b.notify()
}

// pairingNode returns the node a pairing is running on, if any.
func (b *Bridge) pairingNode() *nodeSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pairing == nil {
		return nil
	}
	return b.pairing.node
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "headphone"
	}
	return s
}

// Pair pairs node nodeID with the headphone at addr (which must be in pairing
// mode) and registers the headphone. name may be empty.
func (b *Bridge) Pair(ctx context.Context, nodeID, addrStr, name string) (config.Headphone, error) {
	n := b.nodeByID(nodeID)
	if n == nil {
		return config.Headphone{}, fmt.Errorf("unknown node %q", nodeID)
	}
	addr, err := node.ParseAddr(addrStr)
	if err != nil {
		return config.Headphone{}, err
	}
	c, err := n.getConn()
	if err != nil {
		return config.Headphone{}, err
	}
	job := &pairingJob{node: n, addr: addr, result: make(chan node.Auth, 1)}
	b.mu.Lock()
	if b.pairing != nil {
		b.mu.Unlock()
		return config.Headphone{}, errors.New("another pairing is in progress")
	}
	b.pairing = job
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.pairing = nil
		b.mu.Unlock()
	}()

	// A node holds one A2DP link: release another headphone first.
	if l := n.currentLink(); l.State != node.LinkIdle && l.Peer != addr {
		if other := b.headphoneByAddr(l.Peer); other != nil {
			b.stopStreamer(other)
			b.release(n, other.id())
			other.mu.Lock()
			if other.node == n {
				other.node = nil
			}
			other.mu.Unlock()
		}
		b.log.Info("disconnecting headphone for pairing", "peer", l.Peer, "node", n.name())
		_ = c.Disconnect(l.Peer)
		if _, err := n.waitLink(ctx, 6*time.Second, func(l linkInfo) bool { return l.State == node.LinkIdle }); err != nil {
			return config.Headphone{}, fmt.Errorf("could not release the connected headphone: %w", err)
		}
	}
	if err := c.SetCodecs(b.codecPrefs(config.Headphone{})); err != nil {
		return config.Headphone{}, err
	}
	if err := c.Connect(addr); err != nil {
		return config.Headphone{}, err
	}
	var deviceName string
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	select {
	case a := <-job.result:
		if a.Status != 0 {
			return config.Headphone{}, fmt.Errorf("pairing failed (status %d); is the headphone in pairing mode?", a.Status)
		}
		deviceName = a.Name
	case <-ctx.Done():
		// An existing bond connects without a new pairing event.
		if l := n.currentLink(); l.Peer != addr || l.State < node.LinkConnected {
			return config.Headphone{}, errors.New("no answer from the headphone; is it in pairing mode?")
		}
	}
	if name == "" {
		name = deviceName
	}
	if name == "" {
		name = addr.String()
	}

	existing := b.headphoneByAddr(addr)
	id := slug(name)
	if existing != nil {
		id = existing.id()
	} else if b.headphoneByID(id) != nil {
		id = fmt.Sprintf("%s-%d", id, time.Now().Unix()%10000)
	}
	cfg, err := b.store.Update(id, true, func(h *config.Headphone) {
		if h.Name == "" {
			h.Name = name
		}
		h.Addr = addr.String()
		if deviceName != "" {
			h.DeviceName = deviceName
		}
	})
	if err != nil {
		return cfg, err
	}
	if cfg.WyomingPort == 0 {
		cfg = b.assignWyomingPort(cfg)
	}
	b.mu.Lock()
	h := b.headphones[id]
	created := h == nil
	if created {
		h = newHeadphone(b, cfg)
		b.headphones[id] = h
		b.order = append(b.order, id)
	}
	b.mu.Unlock()
	h.setConfig(cfg)
	if created {
		h.startSatellite(b.ctx)
	}
	b.rememberBond(h, n)
	b.assign(h, n, false)
	b.log.Info("paired headphone", "name", name, "addr", addr, "node", n.name())

	// Find the headphone's BLE advertisements for reception measurement.
	if !h.hasBLEIdentity() {
		go b.learnBLEIdentity(h, n)
	}
	b.notify()
	return cfg, nil
}

// learnBLEIdentity scans all advertisers on n for a while and picks the
// strongest one whose name resembles the headphone's Classic name.
func (b *Bridge) learnBLEIdentity(h *headphone, n *nodeSession) {
	if err := n.startBLEDiscovery(12 * time.Second); err != nil {
		return
	}
	select {
	case <-b.ctx.Done():
		return
	case <-time.After(12 * time.Second):
	}
	h.mu.Lock()
	dev := strings.ToLower(h.cfg.DeviceName)
	h.mu.Unlock()
	if dev == "" {
		return
	}
	var best *SeenBLE
	for _, s := range n.seenBLE() {
		name := strings.ToLower(s.Name)
		if name == "" || !(strings.Contains(name, dev) || strings.Contains(dev, name)) {
			continue
		}
		if best == nil || s.RSSI > best.RSSI {
			sc := s
			best = &sc
		}
	}
	if best == nil {
		h.log.Info("no BLE advertisements found; reception cannot be measured until a BLE identity is set")
		return
	}
	if err := b.SetBLEIdentity(h.id(), best.Name, bleAddrIfStable(*best)); err != nil {
		h.log.Warn("saving BLE identity failed", "err", err)
		return
	}
	h.log.Info("learned BLE identity", "name", best.Name, "addr", best.Addr)
}

// bleAddrIfStable returns the address if it is public or static random;
// resolvable private addresses rotate and are useless as an identity.
func bleAddrIfStable(s SeenBLE) string {
	a, err := node.ParseAddr(s.Addr)
	if err != nil {
		return ""
	}
	if s.AddrType == 0 || (s.AddrType == 1 && a[0]&0xC0 == 0xC0) {
		return s.Addr
	}
	return ""
}

// SetBLEIdentity sets how a headphone's BLE advertisements are recognized.
func (b *Bridge) SetBLEIdentity(id, name, addr string) error {
	if addr != "" {
		if _, err := node.ParseAddr(addr); err != nil {
			return err
		}
	}
	cfg, err := b.store.Update(id, false, func(h *config.Headphone) {
		h.BLEName, h.BLEAddr = name, addr
	})
	if err != nil {
		return err
	}
	if h := b.headphoneByID(id); h != nil {
		h.setConfig(cfg)
	}
	b.refreshScans()
	b.notify()
	return nil
}

// Rename changes the player name shown in Music Assistant.
func (b *Bridge) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	cfg, err := b.store.Update(id, false, func(h *config.Headphone) { h.Name = name })
	if err != nil {
		return err
	}
	h := b.headphoneByID(id)
	if h == nil {
		return nil
	}
	h.setConfig(cfg)
	// The name is sent in the Sendspin hello: reconnect the player.
	h.mu.Lock()
	running := h.client != nil
	h.mu.Unlock()
	if running {
		b.stopStreamer(h)
		h.stopClient()
		h.startClient(b.ctx)
	}
	b.notify()
	return nil
}

// SetCodec sets the codec preference and LDAC quality of a headphone. It takes
// effect on the next connection the bridge initiates.
func (b *Bridge) SetCodec(id, codec, quality string) error {
	if !slices.Contains(CodecChoices, codec) {
		return fmt.Errorf("unknown codec %q", codec)
	}
	if !slices.Contains([]string{"auto", "hq", "sq", "mq"}, quality) {
		return fmt.Errorf("unknown LDAC quality %q", quality)
	}
	cfg, err := b.store.Update(id, false, func(h *config.Headphone) { h.Codec, h.LDACQuality = codec, quality })
	if err != nil {
		return err
	}
	if h := b.headphoneByID(id); h != nil {
		h.setConfig(cfg)
	}
	b.notify()
	return nil
}

// Forget unpairs a headphone from all nodes and deletes it.
func (b *Bridge) Forget(id string) error {
	h := b.headphoneByID(id)
	if h == nil {
		return fmt.Errorf("unknown headphone %q", id)
	}
	b.stopStreamer(h)
	h.stopClient()
	h.stopSatellite()
	h.mu.Lock()
	addr := h.addr
	bleID := h.bleID
	h.mu.Unlock()
	for _, n := range b.nodes {
		if c, err := n.getConn(); err == nil {
			if n.currentLink().Peer == addr {
				_ = c.Disconnect(addr)
			}
			_ = c.RemoveBond(addr)
			if bleID != nil && *bleID != addr {
				_ = c.RemoveBond(*bleID) // BLE bond from learning the identity key
			}
			_ = c.GetBonds()
		}
		b.release(n, id)
	}
	if err := b.store.Delete(id); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.headphones, id)
	b.order = slices.DeleteFunc(b.order, func(s string) bool { return s == id })
	b.mu.Unlock()
	b.refreshScans()
	b.notify()
	return nil
}

// Connect connects a headphone through a specific node (manual control).
func (b *Bridge) Connect(id, nodeID string) error {
	h, n := b.headphoneByID(id), b.nodeByID(nodeID)
	if h == nil || n == nil {
		return errors.New("unknown headphone or node")
	}
	if !b.assign(h, n, false) {
		return fmt.Errorf("node %s is busy", n.name())
	}
	c, err := n.getConn()
	if err != nil {
		return err
	}
	h.mu.Lock()
	addr, cfg := h.addr, h.cfg
	h.mu.Unlock()
	if err := c.SetCodecs(b.codecPrefs(cfg)); err != nil {
		return err
	}
	return c.Connect(addr)
}

// ConnectBest connects a headphone through the node the bridge would pick
// for streaming (best reception, current node, or a node with a bond).
func (b *Bridge) ConnectBest(id string) error {
	h := b.headphoneByID(id)
	if h == nil {
		return fmt.Errorf("unknown headphone %q", id)
	}
	n := b.pickNode(h)
	if n == nil {
		return errors.New("no free node with a bond is online")
	}
	return b.Connect(id, n.id())
}

// Disconnect releases a headphone from its node.
func (b *Bridge) Disconnect(id string) error {
	h := b.headphoneByID(id)
	if h == nil {
		return fmt.Errorf("unknown headphone %q", id)
	}
	b.stopStreamer(h)
	h.mu.Lock()
	n, addr := h.node, h.addr
	h.node = nil
	h.mu.Unlock()
	if n == nil {
		return nil
	}
	b.release(n, id)
	c, err := n.getConn()
	if err != nil {
		return err
	}
	return c.Disconnect(addr)
}

// StartInquiry starts a Classic discovery on a node.
func (b *Bridge) StartInquiry(nodeID string, seconds int) error {
	n := b.nodeByID(nodeID)
	if n == nil {
		return fmt.Errorf("unknown node %q", nodeID)
	}
	return n.startInquiry(seconds)
}

// StartBLEDiscovery reports all BLE advertisers on a node for a while.
func (b *Bridge) StartBLEDiscovery(nodeID string, d time.Duration) error {
	n := b.nodeByID(nodeID)
	if n == nil {
		return fmt.Errorf("unknown node %q", nodeID)
	}
	return n.startBLEDiscovery(d)
}
