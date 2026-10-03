package bridge

import (
	"math"
	"slices"
	"sort"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// Status is a snapshot of the whole bridge for the web interface.
type Status struct {
	Version      string            `json:"version"`
	SendspinURL  string            `json:"sendspin_url"`
	CodecChoices []string          `json:"codec_choices"`
	Nodes        []NodeStatus      `json:"nodes"`
	Headphones   []HeadphoneStatus `json:"headphones"`
	Pairing      bool              `json:"pairing"`
	Time         time.Time         `json:"time"`
}

// NodeStatus describes one relay node.
type NodeStatus struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Online      bool      `json:"online"`
	Since       time.Time `json:"since"`
	LastError   string    `json:"last_error,omitempty"`
	BTAddr      string    `json:"bt_addr"`
	Codecs      []string  `json:"codecs"`
	Link        string    `json:"link"`
	Peer        string    `json:"peer,omitempty"`
	PeerName    string    `json:"peer_name,omitempty"`
	MTU         int       `json:"mtu"`
	Codec       string    `json:"codec,omitempty"`
	AssignedTo  string    `json:"assigned_to,omitempty"`
	QueueMs     int       `json:"queue_ms"`
	Sent        uint32    `json:"sent"`
	Underruns   uint32    `json:"underruns"`
	Dropped     uint32    `json:"dropped"`
	FreeHeap    uint32    `json:"free_heap"`
	MinFreeHeap uint32    `json:"min_free_heap"`
	UptimeS     uint32    `json:"uptime_s"`
	Inquiring   bool      `json:"inquiring"`
	Found       []Found   `json:"found"`
	BLE         []SeenBLE `json:"ble"`
	BLEScanning bool      `json:"ble_scanning"`
	Bonds       []string  `json:"bonds"`
}

// NodeRSSI is the reception of a headphone at one node.
type NodeRSSI struct {
	Node     string  `json:"node"`
	NodeName string  `json:"node_name"`
	RSSI     int     `json:"rssi"`
	Smoothed float64 `json:"smoothed"`
	AgeS     float64 `json:"age_s"`
	Active   bool    `json:"active"`
}

// HeadphoneStatus describes one paired headphone.
type HeadphoneStatus struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Addr             string       `json:"addr"`
	DeviceName       string       `json:"device_name"`
	BLEName          string       `json:"ble_name"`
	BLEAddr          string       `json:"ble_addr"`
	BLEIRK           bool         `json:"ble_irk"` // identity key known (the key itself stays in the bridge)
	BLEIdentity      string       `json:"ble_identity,omitempty"`
	BLEIRKUnverified bool         `json:"ble_irk_unverified,omitempty"`
	PairedNodes      []string     `json:"paired_nodes"`
	State            string       `json:"state"` // absent, available, connected, streaming
	PlayerOnMA       bool         `json:"player_on_ma"`
	LastSeen         time.Time    `json:"last_seen"`
	Node             string       `json:"node,omitempty"`
	NodeName         string       `json:"node_name,omitempty"`
	RSSI             []NodeRSSI   `json:"rssi"`
	Playing          bool         `json:"playing"`
	Title            string       `json:"title,omitempty"`
	Artist           string       `json:"artist,omitempty"`
	Album            string       `json:"album,omitempty"`
	ArtworkURL       string       `json:"artwork_url,omitempty"`
	Volume           int          `json:"volume"`
	AbsVolume        bool         `json:"abs_volume"`
	Stream           *StreamStats `json:"stream,omitempty"`
	Error            string       `json:"error,omitempty"`
	// While connected but idle: seconds until another headphone may take the
	// node and until the Bluetooth connection is released (nil otherwise).
	YieldIn      *int           `json:"yield_in,omitempty"`
	DisconnectIn *int           `json:"disconnect_in,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"` // translated by the web interface
	ErrorArgs    []string       `json:"error_args,omitempty"`
	CodecPref    string         `json:"codec_pref"`
	LDACQuality  string         `json:"ldac_quality"`
	Offered      []string       `json:"offered_codecs,omitempty"` // codecs the headphone offered
	Battery      *BatteryStatus `json:"battery,omitempty"`
	// Voice assistant: Wyoming satellite port and Home Assistant connection.
	WyomingPort  int  `json:"wyoming_port,omitempty"`
	WyomingReady bool `json:"wyoming_ready"`
	VoiceActive  bool `json:"voice_active"`
}

// BatteryStatus is the last battery level a headphone reported.
type BatteryStatus struct {
	Percent  int       `json:"percent"`
	Charging *bool     `json:"charging,omitempty"`
	Source   string    `json:"source"` // apple, hfp or xevent
	At       time.Time `json:"at"`
}

func codecNames(mask uint16) []string {
	var out []string
	for _, c := range []struct {
		bit  uint16
		name string
	}{{node.CodecSBC, "SBC"}, {node.CodecAAC, "AAC"}, {node.CodecAptX, "aptX"}, {node.CodecAptXHD, "aptX HD"}, {node.CodecLDAC, "LDAC"}} {
		if mask&c.bit != 0 {
			out = append(out, c.name)
		}
	}
	return out
}

// Snapshot returns the current status.
func (b *Bridge) Snapshot() Status {
	now := time.Now()
	st := Status{Version: Version, SendspinURL: b.cfg.MusicAssistant.SendspinURL, Time: now, CodecChoices: CodecChoices}
	b.mu.Lock()
	st.Pairing = b.pairing != nil
	b.mu.Unlock()

	nodeNames := map[string]string{}
	btToName := map[string]string{}
	for _, n := range b.nodes {
		nodeNames[n.id()] = n.name()
	}
	for _, n := range b.nodes {
		ns := NodeStatus{ID: n.id(), Name: n.name()}
		n.mu.Lock()
		ns.Online = n.conn != nil
		ns.Since = n.connected
		ns.LastError = n.lastErr
		ns.BTAddr = n.hello.BTAddr.String()
		ns.Codecs = codecNames(n.hello.Codecs)
		ns.Link = n.link.State.String()
		if !n.link.Peer.IsZero() {
			ns.Peer = n.link.Peer.String()
		}
		ns.MTU = n.link.MTU
		if n.link.Codec != nil {
			ns.Codec = n.link.Codec.String()
		}
		ns.AssignedTo = n.assignedTo
		ns.QueueMs = int(n.status.QueuedUs / 1000)
		ns.Sent, ns.Underruns, ns.Dropped = n.status.SentPackets, n.status.Underruns, n.status.Dropped
		ns.FreeHeap, ns.MinFreeHeap, ns.UptimeS = n.status.FreeHeap, n.status.MinFreeHeap, n.status.UptimeS
		ns.Inquiring = n.inquiring
		ns.BLEScanning = now.Before(n.bleAllEnd)
		for _, a := range n.bonds {
			ns.Bonds = append(ns.Bonds, a.String())
		}
		n.mu.Unlock()
		ns.Found = n.foundDevices()
		ns.BLE = n.seenBLE()
		b.attributeBLE(ns.BLE)
		if !ns.Online {
			ns.Found, ns.BLE = nil, nil
		}
		btToName[ns.BTAddr] = ns.Name
		st.Nodes = append(st.Nodes, ns)
	}

	for _, h := range b.allHeadphones() {
		h.mu.Lock()
		hs := HeadphoneStatus{
			ID: h.cfg.ID, Name: h.cfg.Name, Addr: h.cfg.Addr, DeviceName: h.cfg.DeviceName,
			BLEName: h.cfg.BLEName, BLEAddr: h.cfg.BLEAddr, PlayerOnMA: h.connected, LastSeen: h.lastSeen,
			Playing: h.playing, Title: h.meta.Title, Artist: h.meta.Artist, Album: h.meta.Album,
			ArtworkURL: h.meta.ArtworkURL, Volume: h.cfg.Volume, AbsVolume: h.absVolume, Error: h.streamErr,
			CodecPref: h.cfg.Codec, LDACQuality: h.cfg.LDACQuality,
			BLEIRK: h.irk != nil, BLEIdentity: h.cfg.BLEIdentity, BLEIRKUnverified: h.cfg.BLEIRKUnverified,
		}
		if hs.CodecPref == "" {
			hs.CodecPref = "auto"
		}
		if hs.LDACQuality == "" {
			hs.LDACQuality = "auto"
		}
		addr := h.addr
		if h.lastVolume >= 0 {
			hs.Volume = h.lastVolume
		}
		if h.battery != nil {
			bat := *h.battery
			hs.Battery = &bat
		}
		hs.WyomingPort = h.cfg.WyomingPort
		if h.node != nil && !h.streaming && h.streamer == nil && !h.idleSince.IsZero() {
			idle := now.Sub(h.idleSince)
			y := max(0, int((b.cfg.Streaming.YieldAfter - idle).Seconds()))
			d := max(0, int((b.cfg.Streaming.IdleDisconnect - idle).Seconds()))
			hs.YieldIn, hs.DisconnectIn = &y, &d
		}
		hs.ErrorCode, hs.ErrorArgs = h.errCode, slices.Clone(h.errArgs)
		sat := h.sat
		for _, bt := range h.cfg.PairedNodes {
			if name, ok := btToName[bt]; ok {
				hs.PairedNodes = append(hs.PairedNodes, name)
			} else {
				hs.PairedNodes = append(hs.PairedNodes, bt)
			}
		}
		est := h.engine.Estimates(now)
		active := h.engine.Active()
		for id, s := range h.rssi {
			r := NodeRSSI{Node: id, NodeName: nodeNames[id], RSSI: s.RSSI, AgeS: math.Round(now.Sub(s.At).Seconds()*10) / 10,
				Active: id == active}
			if v, ok := est[id]; ok {
				r.Smoothed = math.Round(v*10) / 10
			}
			hs.RSSI = append(hs.RSSI, r)
		}
		sort.Slice(hs.RSSI, func(i, j int) bool { return hs.RSSI[i].Smoothed > hs.RSSI[j].Smoothed })
		n := h.node
		s := h.streamer
		hasBLE := h.bleAddr != nil || h.cfg.BLEName != "" || h.irk != nil
		present := !hasBLE || now.Sub(h.lastSeen) < b.cfg.Streaming.AbsentAfter
		h.mu.Unlock()
		hs.WyomingReady = sat != nil && sat.Ready()
		hs.VoiceActive = h.voice() != nil

		hs.State = "absent"
		if present {
			hs.State = "available"
		}
		if n != nil {
			hs.Node, hs.NodeName = n.id(), n.name()
			l := n.currentLink()
			if l.Peer.String() == hs.Addr && l.State >= node.LinkConnected {
				hs.State = "connected"
				if l.State == node.LinkStreaming && s != nil {
					hs.State = "streaming"
				}
			}
		}
		if s != nil {
			ss := s.snapshot()
			hs.Stream = &ss
		}
		for _, nn := range b.nodes {
			for _, c := range nn.offeredCodecs(addr) {
				if !slices.Contains(hs.Offered, c) {
					hs.Offered = append(hs.Offered, c)
				}
			}
		}
		st.Headphones = append(st.Headphones, hs)
	}
	for i := range st.Nodes {
		if h := b.headphoneByAddrString(st.Nodes[i].Peer); h != "" {
			st.Nodes[i].PeerName = h
		}
	}
	return st
}

// attributeBLE marks the scanned advertisers that belong to a headphone, so
// the web interface shows which entry an IRK, name or address matches.
func (b *Bridge) attributeBLE(list []SeenBLE) {
	hps := b.allHeadphones()
	for i := range list {
		a, err := node.ParseAddr(list[i].Addr)
		if err != nil {
			continue
		}
		adv := node.Adv{Addr: a, AddrType: list[i].AddrType, Name: list[i].Name}
		for _, h := range hps {
			if via := h.identify(adv); via != "" {
				list[i].Headphone, list[i].Via = h.id(), via
				break
			}
		}
	}
}

func (b *Bridge) headphoneByAddrString(s string) string {
	a, err := node.ParseAddr(s)
	if err != nil {
		return ""
	}
	if h := b.headphoneByAddr(a); h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cfg.Name
	}
	return ""
}
