// Package hamqtt publishes the bridge to Home Assistant over MQTT with
// discovery: one device per headphone and per node, with sensors, selects
// and buttons. State is published as one retained JSON document per device;
// the entities pick their values with templates.
package hamqtt

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/sl1288/a2dp-relay-bridge/internal/bridge"
	"github.com/sl1288/a2dp-relay-bridge/internal/config"
)

// Bridge is what this package needs from the bridge.
type Bridge interface {
	Snapshot() bridge.Status
	Subscribe() (<-chan struct{}, func())
	SetCodec(id, codec, quality string) error
	ConnectBest(id string) error
	Disconnect(id string) error
}

// Publisher keeps Home Assistant up to date.
type Publisher struct {
	cfg config.MQTT
	b   Bridge
	log *slog.Logger
	c   paho.Client

	mu         sync.Mutex
	discovered map[string]bool   // device keys with published discovery
	last       map[string]string // topic -> last published payload
}

// New creates a publisher; call Run.
func New(cfg config.MQTT, b Bridge, log *slog.Logger) *Publisher {
	return &Publisher{cfg: cfg, b: b, log: log.With("component", "mqtt"),
		discovered: map[string]bool{}, last: map[string]string{}}
}

func (p *Publisher) base() string { return strings.TrimRight(p.cfg.BaseTopic, "/") }

func (p *Publisher) availability() string { return p.base() + "/status" }

// Run connects and publishes until ctx ends.
func (p *Publisher) Run(ctx context.Context) error {
	opts := paho.NewClientOptions().
		AddBroker(p.cfg.Broker).
		SetClientID(p.cfg.ClientID).
		SetUsername(p.cfg.Username).
		SetPassword(p.cfg.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10*time.Second).
		SetConnectionAttemptHandler(func(broker *url.URL, tc *tls.Config) *tls.Config {
			p.log.Debug("connecting to MQTT broker", "broker", broker.String())
			return tc
		}).
		SetKeepAlive(30*time.Second).
		SetWill(p.availability(), "offline", 1, true).
		SetOnConnectHandler(func(c paho.Client) {
			p.log.Info("connected to MQTT broker", "broker", p.cfg.Broker)
			p.mu.Lock()
			p.discovered = map[string]bool{} // republish after a broker restart
			p.last = map[string]string{}
			p.mu.Unlock()
			c.Publish(p.availability(), 1, true, "online")
			c.Subscribe(p.base()+"/headphone/+/set/+", 1, p.onCommand)
			// Home Assistant restarted: send discovery and state again.
			c.Subscribe(p.cfg.DiscoveryPrefix+"/status", 1, func(paho.Client, paho.Message) {
				p.mu.Lock()
				p.discovered, p.last = map[string]bool{}, map[string]string{}
				p.mu.Unlock()
				p.publish()
			})
			p.publish()
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			p.log.Warn("MQTT connection lost", "err", err)
		})
	p.c = paho.NewClient(opts)
	// The first attempt reports errors; later ones retry quietly.
	if t := p.c.Connect(); t.WaitTimeout(10*time.Second) && t.Error() != nil {
		p.log.Warn("MQTT connection failed, retrying", "broker", p.cfg.Broker, "err", t.Error())
	} else if !p.c.IsConnected() {
		p.log.Warn("MQTT broker not reachable yet, retrying", "broker", p.cfg.Broker)
	}

	changes, unsubscribe := p.b.Subscribe()
	defer unsubscribe()
	tick := time.NewTicker(15 * time.Second) // RSSI and counters change without events
	defer tick.Stop()
	var pending bool
	limit := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			if p.c.IsConnected() {
				p.c.Publish(p.availability(), 1, true, "offline").WaitTimeout(2 * time.Second)
			}
			p.c.Disconnect(500)
			return ctx.Err()
		case <-changes:
			pending = true
		case <-tick.C:
			pending = true
		case <-limit.C:
			if pending && p.c.IsConnected() {
				pending = false
				p.publish()
			}
			limit.Reset(time.Second) // at most one update per second
		}
	}
}

func (p *Publisher) send(topic string, retained bool, v any) {
	var payload string
	switch x := v.(type) {
	case string:
		payload = x
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		payload = string(b)
	}
	p.mu.Lock()
	if p.last[topic] == payload {
		p.mu.Unlock()
		return
	}
	p.last[topic] = payload
	p.mu.Unlock()
	p.c.Publish(topic, 1, retained, payload)
}

// --- state ------------------------------------------------------------------------

type headphoneState struct {
	State       string   `json:"state"`
	Battery     *int     `json:"battery"`
	Charging    *bool    `json:"charging"`
	Node        string   `json:"node"`
	Codec       string   `json:"codec"`
	Bitrate     *int     `json:"bitrate"`
	RSSI        *float64 `json:"rssi"`
	Volume      int      `json:"volume"`
	Playing     bool     `json:"playing"`
	Title       string   `json:"title"`
	CodecPref   string   `json:"codec_pref"`
	LDACQuality string   `json:"ldac_quality"`
	Voice       string   `json:"voice"`
	NodeQueueMs *int     `json:"node_queue_ms"`
}

type nodeState struct {
	Online    bool   `json:"online"`
	Link      string `json:"link"`
	Peer      string `json:"peer"`
	FreeHeap  uint32 `json:"free_heap"`
	MinHeap   uint32 `json:"min_free_heap"`
	Underruns uint32 `json:"underruns"`
	Dropped   uint32 `json:"dropped"`
	UptimeS   uint32 `json:"uptime_s"`
}

func (p *Publisher) publish() {
	st := p.b.Snapshot()
	for _, h := range st.Headphones {
		p.discoverHeadphone(h, st)
		s := headphoneState{State: h.State, Node: h.NodeName, Volume: h.Volume, Playing: h.Playing, Title: h.Title,
			CodecPref: h.CodecPref, LDACQuality: h.LDACQuality, Voice: "unavailable"}
		if h.Battery != nil {
			s.Battery, s.Charging = &h.Battery.Percent, h.Battery.Charging
		}
		if h.Stream != nil {
			s.Codec, s.NodeQueueMs = h.Stream.Codec, &h.Stream.NodeQueueMs
			kbit := h.Stream.Bitrate / 1000
			s.Bitrate = &kbit
		}
		best := math.Inf(-1)
		for _, r := range h.RSSI {
			v := r.Smoothed
			if v == 0 {
				v = float64(r.RSSI)
			}
			if r.AgeS < 30 && v > best {
				best = v
			}
		}
		if !math.IsInf(best, -1) {
			s.RSSI = &best
		}
		switch {
		case h.VoiceActive:
			s.Voice = "active"
		case h.WyomingReady:
			s.Voice = "ready"
		}
		p.send(p.base()+"/headphone/"+h.ID+"/state", true, s)
	}
	for _, n := range st.Nodes {
		p.discoverNode(n)
		p.send(p.base()+"/node/"+slug(n.ID)+"/state", true, nodeState{Online: n.Online, Link: n.Link,
			Peer: n.PeerName, FreeHeap: n.FreeHeap, MinHeap: n.MinFreeHeap, Underruns: n.Underruns,
			Dropped: n.Dropped, UptimeS: n.UptimeS})
	}
}

// --- discovery ---------------------------------------------------------------------

func slug(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '_'
	}, s)
}

func (p *Publisher) entity(component, objectID string, device map[string]any, stateTopic string, cfg map[string]any) {
	cfg["unique_id"] = objectID
	cfg["object_id"] = objectID
	cfg["device"] = device
	cfg["availability_topic"] = p.availability()
	if stateTopic != "" {
		cfg["state_topic"] = stateTopic
	}
	p.send(fmt.Sprintf("%s/%s/%s/config", p.cfg.DiscoveryPrefix, component, objectID), true, cfg)
}

func (p *Publisher) discoverHeadphone(h bridge.HeadphoneStatus, st bridge.Status) {
	key := "h/" + h.ID + "/" + h.Name
	p.mu.Lock()
	done := p.discovered[key]
	p.discovered[key] = true
	p.mu.Unlock()
	if done {
		return
	}
	id := "a2dp_relay_" + slug(h.ID)
	dev := map[string]any{"identifiers": []string{id}, "name": h.Name, "manufacturer": "a2dp-relay",
		"model": h.DeviceName}
	topic := p.base() + "/headphone/" + h.ID + "/state"
	val := func(field string) string { return "{{ value_json." + field + " }}" }
	sensor := func(key, name string, extra map[string]any) {
		cfg := map[string]any{"name": name, "value_template": val(key)}
		for k, v := range extra {
			cfg[k] = v
		}
		p.entity("sensor", id+"_"+key, dev, topic, cfg)
	}
	sensor("battery", "Akku", map[string]any{"device_class": "battery", "unit_of_measurement": "%",
		"state_class": "measurement"})
	sensor("state", "Zustand", map[string]any{"device_class": "enum",
		"options": []string{"absent", "available", "connected", "streaming"}, "icon": "mdi:headphones"})
	sensor("node", "Knoten", map[string]any{"icon": "mdi:access-point"})
	sensor("codec", "Codec", map[string]any{"icon": "mdi:waveform"})
	sensor("bitrate", "Bitrate", map[string]any{"unit_of_measurement": "kbit/s", "state_class": "measurement",
		"icon": "mdi:speedometer", "entity_category": "diagnostic"})
	sensor("rssi", "Empfang", map[string]any{"device_class": "signal_strength", "unit_of_measurement": "dBm",
		"state_class": "measurement", "entity_category": "diagnostic"})
	sensor("voice", "Sprachassistent", map[string]any{"device_class": "enum",
		"options": []string{"unavailable", "ready", "active"}, "icon": "mdi:microphone"})
	sensor("node_queue_ms", "Knotenpuffer", map[string]any{"unit_of_measurement": "ms", "state_class": "measurement",
		"entity_category": "diagnostic"})
	p.entity("binary_sensor", id+"_charging", dev, topic, map[string]any{"name": "Lädt",
		"device_class": "battery_charging", "value_template": "{{ 'ON' if value_json.charging else 'OFF' }}"})

	set := p.base() + "/headphone/" + h.ID + "/set/"
	p.entity("select", id+"_codec", dev, topic, map[string]any{"name": "Codec-Vorgabe",
		"options": st.CodecChoices, "value_template": val("codec_pref"), "command_topic": set + "codec",
		"icon": "mdi:tune", "entity_category": "config"})
	p.entity("select", id+"_ldac_quality", dev, topic, map[string]any{"name": "LDAC-Qualität",
		"options": []string{"auto", "hq", "sq", "mq"}, "value_template": val("ldac_quality"),
		"command_topic": set + "ldac_quality", "icon": "mdi:quality-high", "entity_category": "config"})
	p.entity("button", id+"_connect", dev, "", map[string]any{"name": "Verbinden", "command_topic": set + "connect",
		"icon": "mdi:bluetooth-connect"})
	p.entity("button", id+"_disconnect", dev, "", map[string]any{"name": "Trennen", "command_topic": set + "disconnect",
		"icon": "mdi:bluetooth-off"})
}

func (p *Publisher) discoverNode(n bridge.NodeStatus) {
	key := "n/" + n.ID + "/" + n.Name
	p.mu.Lock()
	done := p.discovered[key]
	p.discovered[key] = true
	p.mu.Unlock()
	if done {
		return
	}
	id := "a2dp_relay_node_" + slug(n.ID)
	dev := map[string]any{"identifiers": []string{id}, "name": "Relay-Knoten " + n.Name, "manufacturer": "a2dp-relay",
		"model": "ESP32 node"}
	topic := p.base() + "/node/" + slug(n.ID) + "/state"
	p.entity("binary_sensor", id+"_online", dev, topic, map[string]any{"name": "Online",
		"device_class": "connectivity", "value_template": "{{ 'ON' if value_json.online else 'OFF' }}"})
	for _, s := range []struct {
		key, name string
		extra     map[string]any
	}{
		{"link", "Verbindung", map[string]any{"icon": "mdi:bluetooth-audio"}},
		{"peer", "Kopfhörer", map[string]any{"icon": "mdi:headphones"}},
		{"free_heap", "Freier Speicher", map[string]any{"unit_of_measurement": "B", "state_class": "measurement",
			"entity_category": "diagnostic"}},
		{"min_free_heap", "Freier Speicher (min.)", map[string]any{"unit_of_measurement": "B",
			"state_class": "measurement", "entity_category": "diagnostic"}},
		{"underruns", "Unterläufe", map[string]any{"state_class": "total_increasing", "entity_category": "diagnostic"}},
		{"dropped", "Verworfene Pakete", map[string]any{"state_class": "total_increasing", "entity_category": "diagnostic"}},
		{"uptime_s", "Laufzeit", map[string]any{"device_class": "duration", "unit_of_measurement": "s",
			"entity_category": "diagnostic"}},
	} {
		cfg := map[string]any{"name": s.name, "value_template": "{{ value_json." + s.key + " }}"}
		for k, v := range s.extra {
			cfg[k] = v
		}
		p.entity("sensor", id+"_"+s.key, dev, topic, cfg)
	}
}

// --- commands ----------------------------------------------------------------------

func (p *Publisher) onCommand(_ paho.Client, m paho.Message) {
	// <base>/headphone/<id>/set/<what>
	parts := strings.Split(strings.TrimPrefix(m.Topic(), p.base()+"/"), "/")
	if len(parts) != 4 || parts[0] != "headphone" || parts[2] != "set" {
		return
	}
	id, what, value := parts[1], parts[3], strings.TrimSpace(string(m.Payload()))
	var err error
	switch what {
	case "codec", "ldac_quality":
		var cur bridge.HeadphoneStatus
		for _, h := range p.b.Snapshot().Headphones {
			if h.ID == id {
				cur = h
			}
		}
		codec, quality := cur.CodecPref, cur.LDACQuality
		if what == "codec" {
			codec = value
		} else {
			quality = value
		}
		err = p.b.SetCodec(id, codec, quality)
	case "connect":
		err = p.b.ConnectBest(id)
	case "disconnect":
		err = p.b.Disconnect(id)
	default:
		return
	}
	if err != nil {
		p.log.Warn("MQTT command failed", "headphone", id, "command", what, "value", value, "err", err)
	} else {
		p.log.Info("MQTT command", "headphone", id, "command", what, "value", value)
	}
}
