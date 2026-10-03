// Package config loads the bridge configuration (YAML, edited by hand) and
// keeps the bridge state (JSON, written by the bridge: paired headphones,
// player IDs, volumes).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the hand-written configuration file.
type Config struct {
	MusicAssistant MusicAssistant `yaml:"music_assistant"`
	Web            Web            `yaml:"web"`
	StateFile      string         `yaml:"state_file"`
	Nodes          []Node         `yaml:"nodes"`
	Handover       Handover       `yaml:"handover"`
	Streaming      Streaming      `yaml:"streaming"`
	Voice          Voice          `yaml:"voice"`
	MQTT           MQTT           `yaml:"mqtt"`
}

// MQTT publishes the bridge to Home Assistant (MQTT discovery). Off without a broker.
type MQTT struct {
	Broker          string `yaml:"broker"` // e.g. tcp://mqtt.local:1883
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	ClientID        string `yaml:"client_id"`
	BaseTopic       string `yaml:"base_topic"`
	DiscoveryPrefix string `yaml:"discovery_prefix"`
}

// Voice tunes the voice assistant.
type Voice struct {
	// Record saves the microphone audio of every session as a WAV file next
	// to the state file (diagnostics). Without Home Assistant it is always
	// recorded.
	Record bool `yaml:"record"`
}

// MusicAssistant locates the Sendspin server.
type MusicAssistant struct {
	SendspinURL string `yaml:"sendspin_url"`
}

// Web configures the web interface.
type Web struct {
	Listen string  `yaml:"listen"`
	Auth   WebAuth `yaml:"auth"`
}

// WebAuth protects the web interface with a Home Assistant login.
type WebAuth struct {
	// HomeAssistant is the Home Assistant URL users log in at, as their
	// browser reaches it (empty: no login).
	HomeAssistant string `yaml:"home_assistant"`
	// AdminOnly admits only Home Assistant administrators.
	AdminOnly bool `yaml:"admin_only"`
}

// Node is one relay node.
type Node struct {
	Address string `yaml:"address"` // host:port
	Name    string `yaml:"name"`    // optional; defaults to the node's own name
	// RSSIOffset corrects antenna differences in dB (added to every reading).
	RSSIOffset float64 `yaml:"rssi_offset"`
}

// Handover tunes when the stream moves to another node.
type Handover struct {
	HysteresisDB float64       `yaml:"hysteresis_db"`
	Hold         time.Duration `yaml:"hold"`
	MinDwell     time.Duration `yaml:"min_dwell"`
	StaleAfter   time.Duration `yaml:"stale_after"`
	TimeConstant time.Duration `yaml:"time_constant"`
	MinRSSI      float64       `yaml:"min_rssi"`
}

// Streaming tunes the audio path.
type Streaming struct {
	// NodeBuffer is the audio kept queued on the node.
	NodeBuffer time.Duration `yaml:"node_buffer"`
	// OutputLatency is the time from the node handing audio to its Bluetooth
	// stack until the headphone plays it (radio, headphone buffer, decoding).
	// Audio is sent this long before the time Music Assistant set for it; the
	// static delay of the player in Music Assistant shifts it further.
	OutputLatency time.Duration `yaml:"output_latency"`
	// SBCMaxBitpool caps the SBC bitpool (53 = "high quality" at 48 kHz joint stereo).
	SBCMaxBitpool int `yaml:"sbc_max_bitpool"`
	// AACMaxBitrate caps the AAC bit rate (bit/s).
	AACMaxBitrate int `yaml:"aac_max_bitrate"`
	// SuspendAfter suspends the Bluetooth audio stream after playback stopped.
	SuspendAfter time.Duration `yaml:"suspend_after"`
	// YieldAfter lets a headphone that played nothing for this long give up
	// its node to another headphone that wants to play.
	YieldAfter time.Duration `yaml:"yield_after"`
	// IdleDisconnect releases the headphone after playback stopped this long.
	IdleDisconnect time.Duration `yaml:"idle_disconnect"`
	// AbsentAfter removes the player from Music Assistant when no node has
	// seen the headphone for this long.
	AbsentAfter time.Duration `yaml:"absent_after"`
}

// Default returns the configuration used for omitted fields.
func Default() Config {
	return Config{
		MusicAssistant: MusicAssistant{SendspinURL: "ws://127.0.0.1:8927/sendspin"},
		Web:            Web{Listen: ":8099", Auth: WebAuth{AdminOnly: true}},
		StateFile:      "state.json",
		Handover: Handover{
			HysteresisDB: 8,
			Hold:         5 * time.Second,
			MinDwell:     30 * time.Second,
			StaleAfter:   15 * time.Second,
			TimeConstant: 3 * time.Second,
			MinRSSI:      -95,
		},
		MQTT: MQTT{ClientID: "a2dp-relay", BaseTopic: "a2dp-relay", DiscoveryPrefix: "homeassistant"},
		Streaming: Streaming{
			NodeBuffer:     200 * time.Millisecond,
			OutputLatency:  150 * time.Millisecond,
			SBCMaxBitpool:  53,
			AACMaxBitrate:  320000,
			SuspendAfter:   5 * time.Second,
			YieldAfter:     2 * time.Minute,
			IdleDisconnect: 15 * time.Minute,
			AbsentAfter:    2 * time.Minute,
		},
	}
}

// Load reads a configuration file; omitted fields keep their defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Nodes) == 0 {
		return cfg, fmt.Errorf("%s: no nodes configured", path)
	}
	for i, n := range cfg.Nodes {
		if n.Address == "" {
			return cfg, fmt.Errorf("%s: node %d has no address", path, i+1)
		}
	}
	return cfg, nil
}
