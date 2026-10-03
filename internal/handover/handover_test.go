package handover

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

// feed reports constant RSSI values for all nodes once per second in [from, to).
func feed(e *Engine, from, to float64, rssi map[string]float64) {
	for s := from; s < to; s++ {
		for node, v := range rssi {
			e.Observe(node, v, at(s))
		}
	}
}

func TestInitialSelectionPicksStrongest(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -70, "b": -60})
	d := e.Decide(at(3))
	if d.Active != "b" || !d.Switch {
		t.Fatalf("got %+v, want switch to b", d)
	}
}

func TestNoNodeBelowMinRSSI(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -95})
	if d := e.Decide(at(3)); d.Active != "" {
		t.Fatalf("got %+v, want no active node", d)
	}
}

func TestSmallAdvantageDoesNotSwitch(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -60, "b": -70})
	e.Decide(at(3))
	// b becomes better, but by less than the hysteresis.
	feed(e, 3, 120, map[string]float64{"a": -65, "b": -61})
	if d := e.Decide(at(120)); d.Active != "a" || d.Switch {
		t.Fatalf("got %+v, want a to keep the stream", d)
	}
}

func TestClearAdvantageSwitchesAfterDwellAndHold(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -60, "b": -80})
	e.Decide(at(3)) // a active at t=3

	// From t=5 b is far better. The smoothed values cross the hysteresis
	// after a few seconds, but MinDwell (20 s after t=3) must pass first.
	switched := -1.0
	for s := 5.0; s < 60; s++ {
		e.Observe("a", -80, at(s))
		e.Observe("b", -55, at(s))
		if d := e.Decide(at(s)); d.Switch {
			if d.Active != "b" {
				t.Fatalf("switched to %q, want b", d.Active)
			}
			switched = s
			break
		}
	}
	if switched < 23 {
		t.Fatalf("switched at t=%v, before MinDwell expired", switched)
	}
	if switched > 30 {
		t.Fatalf("switched at t=%v, much later than expected", switched)
	}
}

func TestShortSpikeDoesNotSwitch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinDwell = 0
	e := New(cfg)
	feed(e, 0, 30, map[string]float64{"a": -60, "b": -75})
	e.Decide(at(30))
	// b spikes for 3 s (shorter than Hold), then falls back.
	for s := 30.0; s < 33; s++ {
		e.Observe("a", -60, at(s))
		e.Observe("b", -30, at(s))
		if d := e.Decide(at(s)); d.Switch {
			t.Fatalf("switched on a short spike at t=%v", s)
		}
	}
	for s := 33.0; s < 60; s++ {
		e.Observe("a", -60, at(s))
		e.Observe("b", -75, at(s))
		if d := e.Decide(at(s)); d.Switch {
			t.Fatalf("switched after the spike at t=%v", s)
		}
	}
}

func TestStaleActiveFailsOverImmediately(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -60, "b": -75})
	e.Decide(at(3))
	// a stops reporting, b keeps reporting.
	feed(e, 3, 15, map[string]float64{"b": -75})
	d := e.Decide(at(15))
	if d.Active != "b" || !d.Switch {
		t.Fatalf("got %+v, want failover to b", d)
	}
}

func TestActiveLostFailsOverWithoutDwell(t *testing.T) {
	e := New(DefaultConfig())
	feed(e, 0, 3, map[string]float64{"a": -60, "b": -75})
	e.Decide(at(3))
	e.ActiveLost()
	e.Observe("b", -75, at(4))
	d := e.Decide(at(4))
	if d.Active != "b" || !d.Switch {
		t.Fatalf("got %+v, want immediate failover to b", d)
	}
}

func TestOffsetsCompensateAntennas(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Offsets = map[string]float64{"b": 10}
	e := New(cfg)
	feed(e, 0, 3, map[string]float64{"a": -60, "b": -65})
	if d := e.Decide(at(3)); d.Active != "b" {
		t.Fatalf("got %+v, want b thanks to its offset", d)
	}
}
