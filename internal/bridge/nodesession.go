package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
	"github.com/sl1288/a2dp-relay-bridge/internal/stream"
)

// linkInfo is the A2DP link of a node as last reported.
type linkInfo struct {
	State node.LinkState
	Peer  node.Addr
	MTU   int
	Codec *node.CodecConfig // negotiated codec, nil until configured
}

// Found is a Classic device seen during discovery.
type Found struct {
	Addr     string    `json:"addr"`
	Name     string    `json:"name"`
	Class    uint32    `json:"class"`
	RSSI     int       `json:"rssi"`
	LastSeen time.Time `json:"last_seen"`
	IsAudio  bool      `json:"is_audio"`
}

// SeenBLE is a BLE advertiser seen while scanning everything.
type SeenBLE struct {
	Addr     string    `json:"addr"`
	AddrType uint8     `json:"addr_type"`
	Name     string    `json:"name"`
	Company  int       `json:"company"` // manufacturer (Bluetooth SIG company ID), -1: unknown
	RSSI     int       `json:"rssi"`
	LastSeen time.Time `json:"last_seen"`
	// Headphone this advertiser was attributed to, and how (filled in the status).
	Headphone string `json:"headphone,omitempty"`
	Via       string `json:"via,omitempty"`
}

type nodeSession struct {
	b   *Bridge
	cfg config.Node
	log *slog.Logger

	pacer     atomic.Pointer[stream.Pacer]
	stackBusy atomic.Uint32 // congestion counter from the last credit

	mu         sync.Mutex
	conn       *node.Conn
	hello      node.Hello
	status     node.Status
	link       linkInfo
	assignedTo string // headphone ID using this node, "" if free
	linkSignal chan struct{}
	inquiring  bool
	found      map[node.Addr]*Found
	bleAll     map[node.Addr]*SeenBLE
	bleAllEnd  time.Time
	bonds      []node.Addr
	lastErr    string
	connected  time.Time
	remoteCaps map[node.Addr][]string // codecs each headphone offered when connected
}

func newNodeSession(b *Bridge, cfg config.Node) *nodeSession {
	return &nodeSession{
		b:          b,
		cfg:        cfg,
		log:        b.log.With("node", cfg.Address),
		linkSignal: make(chan struct{}),
		found:      map[node.Addr]*Found{},
		bleAll:     map[node.Addr]*SeenBLE{},
		remoteCaps: map[node.Addr][]string{},
	}
}

// id identifies the node in the bridge (its configured address).
func (n *nodeSession) id() string { return n.cfg.Address }

func (n *nodeSession) name() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cfg.Name != "" {
		return n.cfg.Name
	}
	if n.hello.Name != "" {
		return n.hello.Name
	}
	return n.cfg.Address
}

func (n *nodeSession) btAddr() node.Addr {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hello.BTAddr
}

func (n *nodeSession) online() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn != nil
}

func (n *nodeSession) getConn() (*node.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn == nil {
		return nil, fmt.Errorf("node %s is offline", n.cfg.Address)
	}
	return n.conn, nil
}

func (n *nodeSession) currentLink() linkInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.link
	if l.Codec != nil {
		c := *l.Codec
		l.Codec = &c
	}
	return l
}

// offeredCodecs returns the codecs a headphone offered on this node, if known.
func (n *nodeSession) offeredCodecs(a node.Addr) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.remoteCaps[a]...)
}

// waitLink blocks until cond holds for the node's link or the timeout expires.
func (n *nodeSession) waitLink(ctx context.Context, timeout time.Duration, cond func(linkInfo) bool) (linkInfo, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		n.mu.Lock()
		l, sig := n.link, n.linkSignal
		online := n.conn != nil
		n.mu.Unlock()
		if cond(l) {
			return l, nil
		}
		if !online {
			return l, errors.New("node went offline")
		}
		select {
		case <-ctx.Done():
			return l, ctx.Err()
		case <-deadline.C:
			return l, errors.New("timeout")
		case <-sig:
		}
	}
}

// signalLink wakes everyone waiting for a link change. Caller holds n.mu.
func (n *nodeSession) signalLinkLocked() {
	close(n.linkSignal)
	n.linkSignal = make(chan struct{})
}

func (n *nodeSession) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := n.session(ctx)
		if ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		n.lastErr = err.Error()
		n.mu.Unlock()
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		n.log.Warn("node connection lost", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func (n *nodeSession) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	c, err := node.Dial(dctx, n.cfg.Address)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		c.Close()
	}()

	n.mu.Lock()
	n.conn = c
	n.link = linkInfo{}
	n.lastErr = ""
	n.connected = time.Now()
	n.signalLinkLocked()
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.conn = nil
		n.link = linkInfo{}
		n.signalLinkLocked()
		n.mu.Unlock()
		n.b.nodeOffline(n)
	}()

	n.log.Info("connected to node")
	if err := n.applyScan(); err != nil {
		return err
	}
	_ = c.GetBonds()

	// A node sends a status every second; silence means the link is dead.
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		msg, err := c.Read()
		if err != nil {
			return err
		}
		n.handle(msg)
	}
}

// applyScan sends the BLE scan configuration: either the watch list of all
// headphones, or everything while the web interface is looking for devices.
func (n *nodeSession) applyScan() error {
	c, err := n.getConn()
	if err != nil {
		return err
	}
	n.mu.Lock()
	all := time.Now().Before(n.bleAllEnd)
	streaming := n.link.State == node.LinkStreaming
	n.mu.Unlock()
	// While streaming, scan with a low duty cycle to leave airtime for audio.
	interval, window := 100*time.Millisecond, 30*time.Millisecond
	if streaming {
		interval, window = 300*time.Millisecond, 30*time.Millisecond
	}
	if all {
		return c.Scan(node.ScanAll, true, 100*time.Millisecond, 50*time.Millisecond, nil)
	}
	filters := n.b.scanFilters()
	if len(filters) == 0 {
		return c.Scan(node.ScanOff, false, interval, window, nil)
	}
	return c.Scan(node.ScanWatched, true, interval, window, filters)
}

func (n *nodeSession) handle(msg any) {
	switch m := msg.(type) {
	case node.Hello:
		n.mu.Lock()
		n.hello = m
		n.mu.Unlock()
		n.log.Info("node hello", "name", m.Name, "bt", m.BTAddr, "codecs", m.Codecs)
	case node.Status:
		n.mu.Lock()
		n.status = m
		changed := n.link.State != m.Link
		if changed {
			n.link.State = m.Link
			n.link.Peer = m.Peer
			if m.MTU > 0 {
				n.link.MTU = int(m.MTU)
			}
			n.signalLinkLocked()
		}
		n.mu.Unlock()
	case node.Credit:
		n.stackBusy.Store(m.StackBusy)
		if p := n.pacer.Load(); p != nil {
			p.Credit(m.QueuedUs, m.Underruns)
		}
	case node.RemoteCaps:
		var names []string
		for _, s := range m.SEPs {
			if name := node.CodecOf(s.Info); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		n.mu.Lock()
		n.remoteCaps[m.Addr] = names
		n.mu.Unlock()
		n.log.Info("headphone offers codecs", "peer", m.Addr, "codecs", names)
		n.b.notify()
	case node.A2DPEvent:
		n.handleA2DP(m)
	case node.Adv:
		n.mu.Lock()
		if time.Now().Before(n.bleAllEnd) {
			e := n.bleAll[m.Addr]
			if e == nil {
				e = &SeenBLE{Addr: m.Addr.String(), Company: -1}
				n.bleAll[m.Addr] = e
			}
			e.AddrType, e.RSSI, e.LastSeen = m.AddrType, int(m.RSSI), time.Now()
			if m.Name != "" {
				e.Name = m.Name
			}
			if m.Company >= 0 {
				e.Company = m.Company
			}
		}
		n.mu.Unlock()
		n.b.observeAdv(n, m)
	case node.InquiryResult:
		n.mu.Lock()
		e := n.found[m.Addr]
		if e == nil {
			e = &Found{Addr: m.Addr.String()}
			n.found[m.Addr] = e
		}
		e.Class, e.RSSI, e.LastSeen = m.Class, int(m.RSSI), time.Now()
		// Major device class 0x04: audio/video.
		e.IsAudio = (m.Class>>8)&0x1F == 0x04
		if m.Name != "" {
			e.Name = m.Name
		}
		n.mu.Unlock()
	case node.InquiryDone:
		n.mu.Lock()
		n.inquiring = false
		n.mu.Unlock()
		n.b.notify()
	case node.Auth:
		n.b.onAuth(n, m)
	case node.BLEPair:
		n.b.onBLEPair(n, m)
	case node.AVRCPKey:
		if !m.Released {
			n.b.onKey(n, m)
		}
	case node.Volume:
		n.b.onVolume(n, m)
	case node.Bonds:
		n.mu.Lock()
		n.bonds = append([]node.Addr(nil), m...)
		n.mu.Unlock()
		n.b.notify()
	case node.Battery:
		n.b.onBattery(n, m)
	case node.Voice:
		n.b.onVoice(n, m)
	case node.VoiceAudio:
		n.b.onVoiceAudio(n, m)
	case node.Unknown:
		n.log.Debug("ignoring unknown node message", "type", fmt.Sprintf("0x%02X", m.Type))
	}
}

func (n *nodeSession) handleA2DP(e node.A2DPEvent) {
	n.mu.Lock()
	if e.MTU > 0 {
		n.link.MTU = int(e.MTU)
	}
	switch e.Event {
	case node.EvConnecting:
		n.link.State = node.LinkConnecting
		n.link.Peer = e.Addr
	case node.EvConnected:
		if n.link.State != node.LinkStreaming {
			n.link.State = node.LinkConnected
		}
		n.link.Peer = e.Addr
	case node.EvAudioStarted:
		n.link.State = node.LinkStreaming
		n.link.Peer = e.Addr
	case node.EvAudioSuspended:
		if n.link.State == node.LinkStreaming {
			n.link.State = node.LinkConnected
		}
	case node.EvDisconnected:
		n.link = linkInfo{}
	case node.EvAudioConfig:
		n.link.Codec = nil
		if cfg, err := node.ParseCodecConfig(e.Info); err == nil {
			n.link.Codec = &cfg
		} else {
			n.log.Warn("unusable codec configuration", "err", err, "info", fmt.Sprintf("% x", e.Info))
		}
	}
	n.signalLinkLocked()
	n.mu.Unlock()
	if e.Event == node.EvAudioConfig && n.currentLink().Codec != nil {
		n.log.Info("a2dp", "event", node.EventName(e.Event), "peer", e.Addr, "codec", n.currentLink().Codec.String())
	} else if e.Event != node.EvMediaCtrlAck {
		n.log.Info("a2dp", "event", node.EventName(e.Event), "peer", e.Addr, "mtu", e.MTU)
	}
	n.b.onLinkEvent(n, e)
}

// startBLEDiscovery reports every BLE advertiser for d (web interface).
func (n *nodeSession) startBLEDiscovery(d time.Duration) error {
	n.mu.Lock()
	n.bleAll = map[node.Addr]*SeenBLE{}
	n.bleAllEnd = time.Now().Add(d)
	n.mu.Unlock()
	if err := n.applyScan(); err != nil {
		return err
	}
	time.AfterFunc(d+100*time.Millisecond, func() { _ = n.applyScan() })
	return nil
}

func (n *nodeSession) startInquiry(seconds int) error {
	c, err := n.getConn()
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.found = map[node.Addr]*Found{}
	n.inquiring = true
	n.mu.Unlock()
	return c.Inquiry(seconds)
}

func (n *nodeSession) foundDevices() []Found {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Found, 0, len(n.found))
	for _, f := range n.found {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RSSI > out[j].RSSI })
	return out
}

func (n *nodeSession) seenBLE() []SeenBLE {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]SeenBLE, 0, len(n.bleAll))
	for _, s := range n.bleAll {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RSSI > out[j].RSSI })
	return out
}
