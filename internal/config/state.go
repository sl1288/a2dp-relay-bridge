package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Headphone is a paired headphone as persisted by the bridge.
type Headphone struct {
	ID string `json:"id"`
	// Name is the player name in Music Assistant.
	Name string `json:"name"`
	// Addr is the Bluetooth Classic address (AA:BB:CC:DD:EE:FF).
	Addr string `json:"addr"`
	// DeviceName is the name the headphone reported when it was paired.
	DeviceName string `json:"device_name"`
	// BLEName and BLEAddr identify the headphone's BLE advertisements, which
	// every node uses to measure reception. Either may be empty.
	BLEName string `json:"ble_name,omitempty"`
	BLEAddr string `json:"ble_addr,omitempty"`
	// BLEIRK is the identity resolving key learned by BLE pairing (32 hex
	// digits, most significant byte first): it resolves the headphone's
	// rotating private addresses. BLEIdentity is its identity address.
	BLEIRK      string `json:"ble_irk,omitempty"`
	BLEIdentity string `json:"ble_identity,omitempty"`
	// BLEIRKUnverified marks an IRK entered by hand that has not resolved an
	// address yet; until it does, both byte orders are tried.
	BLEIRKUnverified bool `json:"ble_irk_unverified,omitempty"`
	// ClientID is the stable Sendspin client ID of this headphone's player.
	ClientID string `json:"client_id"`
	// PairedNodes lists the Bluetooth addresses of nodes that hold a bond.
	PairedNodes   []string `json:"paired_nodes"`
	Volume        int      `json:"volume"`
	StaticDelayMs int      `json:"static_delay_ms"`
	// Codec is the preferred codec: "" or "auto" for the best one both sides
	// support, otherwise a codec name (SBC is always the fallback).
	Codec string `json:"codec,omitempty"`
	// LDACQuality is "auto" (adaptive), "hq", "sq" or "mq".
	LDACQuality string `json:"ldac_quality,omitempty"`
	// WyomingPort is the TCP port of the headphone's voice assistant
	// satellite for Home Assistant (0: not assigned yet).
	WyomingPort int `json:"wyoming_port,omitempty"`
}

// State is everything the bridge persists.
type State struct {
	Headphones []Headphone `json:"headphones"`
}

// Store guards the state and writes it to disk on every change.
type Store struct {
	path string
	mu   sync.Mutex
	st   State
}

// OpenStore loads the state file, starting empty if it does not exist.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Headphones returns a copy of all headphones.
func (s *Store) Headphones() []Headphone {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Headphone, len(s.st.Headphones))
	for i, h := range s.st.Headphones {
		h.PairedNodes = slices.Clone(h.PairedNodes)
		out[i] = h
	}
	return out
}

// Headphone returns one headphone by ID.
func (s *Store) Headphone(id string) (Headphone, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.st.Headphones {
		if h.ID == id {
			h.PairedNodes = slices.Clone(h.PairedNodes)
			return h, true
		}
	}
	return Headphone{}, false
}

// Update applies fn to the headphone with the given ID, or adds a new one if
// it does not exist and create is set, then saves.
func (s *Store) Update(id string, create bool, fn func(*Headphone)) (Headphone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.st.Headphones, func(h Headphone) bool { return h.ID == id })
	if i < 0 {
		if !create {
			return Headphone{}, fmt.Errorf("unknown headphone %q", id)
		}
		s.st.Headphones = append(s.st.Headphones, Headphone{ID: id, ClientID: "a2dp-relay-" + randomHex(6), Volume: 40})
		i = len(s.st.Headphones) - 1
	}
	fn(&s.st.Headphones[i])
	h := s.st.Headphones[i]
	h.PairedNodes = slices.Clone(h.PairedNodes)
	return h, s.saveLocked()
}

// Delete removes a headphone and saves.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Headphones = slices.DeleteFunc(s.st.Headphones, func(h Headphone) bool { return h.ID == id })
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
