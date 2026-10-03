// Package handover decides which relay node streams to a headphone.
//
// Every node reports the RSSI at which it receives the headphone. The engine
// smooths these reports per node and moves the stream to another node only if
// that node is clearly and persistently better, so that the stream does not
// bounce between nodes with similar reception.
package handover

import (
	"math"
	"time"
)

// Config tunes the handover decision.
type Config struct {
	// TimeConstant of the exponential moving average applied to RSSI samples.
	TimeConstant time.Duration
	// Hysteresis is the margin in dB by which a candidate must beat the active node.
	Hysteresis float64
	// Hold is how long a candidate must stay better before the stream moves.
	Hold time.Duration
	// MinDwell is the minimum time a node keeps the stream after taking it over.
	MinDwell time.Duration
	// StaleAfter discards a node's estimate when no sample arrived for this long.
	StaleAfter time.Duration
	// MinRSSI is the weakest smoothed RSSI at which a node may take the stream.
	MinRSSI float64
	// Offsets corrects per-node antenna differences in dB (added to every sample).
	Offsets map[string]float64
}

// DefaultConfig returns conservative values: a switch interrupts the audio for
// a few seconds, so it must only happen when it clearly pays off.
func DefaultConfig() Config {
	return Config{
		TimeConstant: 3 * time.Second,
		Hysteresis:   6,
		Hold:         5 * time.Second,
		MinDwell:     20 * time.Second,
		StaleAfter:   10 * time.Second,
		MinRSSI:      -90,
	}
}

// Decision is the outcome of one evaluation.
type Decision struct {
	// Active is the node that should stream, empty if no node is eligible.
	Active string
	// Switch is true if Active differs from the previous decision.
	Switch bool
	// Reason explains a switch for logging.
	Reason string
}

type estimate struct {
	value    float64
	lastSeen time.Time
}

// Engine tracks the RSSI estimates of all nodes for one headphone.
// It is not safe for concurrent use.
type Engine struct {
	cfg       Config
	nodes     map[string]*estimate
	active    string
	activeAt  time.Time
	candidate string
	candSince time.Time
}

// New creates an engine with the given configuration.
func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, nodes: make(map[string]*estimate)}
}

// Active returns the node that currently holds the stream.
func (e *Engine) Active() string { return e.active }

// Observe feeds one RSSI sample in dBm reported by node at time at.
func (e *Engine) Observe(node string, rssi float64, at time.Time) {
	rssi += e.cfg.Offsets[node]
	est, ok := e.nodes[node]
	if !ok || at.Sub(est.lastSeen) > e.cfg.StaleAfter {
		e.nodes[node] = &estimate{value: rssi, lastSeen: at}
		return
	}
	dt := at.Sub(est.lastSeen)
	if dt < 0 {
		return // out of order, the estimate already contains newer data
	}
	alpha := 1 - math.Exp(-float64(dt)/float64(e.cfg.TimeConstant))
	est.value += alpha * (rssi - est.value)
	est.lastSeen = at
}

// Estimates returns the smoothed RSSI of every node with a fresh estimate.
func (e *Engine) Estimates(now time.Time) map[string]float64 {
	out := make(map[string]float64, len(e.nodes))
	for node := range e.nodes {
		if v, ok := e.value(node, now); ok {
			out[node] = v
		}
	}
	return out
}

// Remove forgets a node, e.g. because it went offline.
func (e *Engine) Remove(node string) {
	delete(e.nodes, node)
	if e.candidate == node {
		e.candidate = ""
	}
}

// ActiveLost reports that the active node lost its link to the headphone.
// Its estimate is dropped so that the next decision fails over immediately.
func (e *Engine) ActiveLost() {
	if e.active != "" {
		delete(e.nodes, e.active)
	}
}

// Release clears the active node, e.g. after the headphone was switched off.
func (e *Engine) Release() {
	e.active = ""
	e.candidate = ""
}

func (e *Engine) value(node string, now time.Time) (float64, bool) {
	est, ok := e.nodes[node]
	if !ok || now.Sub(est.lastSeen) > e.cfg.StaleAfter {
		return 0, false
	}
	return est.value, true
}

func (e *Engine) best(now time.Time) (string, float64) {
	bestNode, bestVal := "", math.Inf(-1)
	for node := range e.nodes {
		v, ok := e.value(node, now)
		if !ok || v < e.cfg.MinRSSI {
			continue
		}
		// Ties are broken by name so that decisions are deterministic.
		if v > bestVal || (v == bestVal && node < bestNode) {
			bestNode, bestVal = node, v
		}
	}
	return bestNode, bestVal
}

func (e *Engine) switchTo(node string, now time.Time, reason string) Decision {
	e.active = node
	e.activeAt = now
	e.candidate = ""
	return Decision{Active: node, Switch: true, Reason: reason}
}

// Decide evaluates the current estimates and returns which node should stream.
func (e *Engine) Decide(now time.Time) Decision {
	best, bestVal := e.best(now)
	if e.active == "" {
		if best == "" {
			return Decision{}
		}
		return e.switchTo(best, now, "initial selection")
	}
	keep := Decision{Active: e.active}

	activeVal, fresh := e.value(e.active, now)
	if !fresh {
		// The active node no longer sees the headphone or lost its link:
		// fail over without waiting for dwell or hold times.
		if best != "" && best != e.active {
			return e.switchTo(best, now, "active node lost the headphone")
		}
		return keep
	}
	if best == "" || best == e.active || bestVal < activeVal+e.cfg.Hysteresis {
		e.candidate = ""
		return keep
	}
	if e.candidate != best {
		e.candidate = best
		e.candSince = now
	}
	if now.Sub(e.activeAt) < e.cfg.MinDwell || now.Sub(e.candSince) < e.cfg.Hold {
		return keep
	}
	return e.switchTo(best, now, "better reception")
}
