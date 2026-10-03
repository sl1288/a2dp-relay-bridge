// Package wyoming implements a Wyoming protocol satellite (Home Assistant's
// voice protocol): Home Assistant connects over TCP, the satellite starts
// pipeline runs, streams microphone audio and plays the spoken answer.
//
// A message is one JSON line {"type", "data_length", "payload_length"},
// followed by data_length bytes of JSON data and payload_length bytes of
// binary payload (audio).
package wyoming

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// protocolVersion is the Wyoming version this satellite speaks.
const protocolVersion = "1.10.2"

// Event is one Wyoming message.
type Event struct {
	Type    string
	Data    map[string]any
	Payload []byte
}

type header struct {
	Type          string         `json:"type"`
	Data          map[string]any `json:"data,omitempty"`
	DataLength    int            `json:"data_length,omitempty"`
	PayloadLength int            `json:"payload_length,omitempty"`
	Version       string         `json:"version,omitempty"`
}

// readEvent reads one event.
func readEvent(r *bufio.Reader) (Event, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return Event{}, err
	}
	var h header
	if err := json.Unmarshal(line, &h); err != nil {
		return Event{}, fmt.Errorf("wyoming: bad header: %w", err)
	}
	ev := Event{Type: h.Type, Data: h.Data}
	if ev.Data == nil {
		ev.Data = map[string]any{}
	}
	if h.DataLength > 0 {
		b := make([]byte, h.DataLength)
		if _, err := io.ReadFull(r, b); err != nil {
			return Event{}, err
		}
		if err := json.Unmarshal(b, &ev.Data); err != nil {
			return Event{}, fmt.Errorf("wyoming: bad data: %w", err)
		}
	}
	if h.PayloadLength > 0 {
		ev.Payload = make([]byte, h.PayloadLength)
		if _, err := io.ReadFull(r, ev.Payload); err != nil {
			return Event{}, err
		}
	}
	return ev, nil
}

// encode serialises an event.
func (ev Event) encode() ([]byte, error) {
	h := header{Type: ev.Type, Version: protocolVersion, PayloadLength: len(ev.Payload)}
	var data []byte
	if len(ev.Data) > 0 {
		var err error
		if data, err = json.Marshal(ev.Data); err != nil {
			return nil, err
		}
		h.DataLength = len(data)
	}
	line, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	out := append(line, '\n')
	out = append(out, data...)
	return append(out, ev.Payload...), nil
}

// AudioFormat describes PCM audio.
type AudioFormat struct {
	Rate     int
	Width    int // bytes per sample
	Channels int
}

func (f AudioFormat) data(extra map[string]any) map[string]any {
	d := map[string]any{"rate": f.Rate, "width": f.Width, "channels": f.Channels}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func formatOf(d map[string]any) AudioFormat {
	n := func(k string) int {
		if v, ok := d[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	return AudioFormat{Rate: n("rate"), Width: n("width"), Channels: n("channels")}
}

func str(d map[string]any, k string) string {
	s, _ := d[k].(string)
	return s
}
