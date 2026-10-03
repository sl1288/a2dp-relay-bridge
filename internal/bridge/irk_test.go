package bridge

import (
	"io"
	"log/slog"
	"testing"

	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// Sample of Core Vol 3 Part H Appendix D.7: this IRK resolves this address.
const (
	sampleIRK = "ec0234a357c8ad05341010a60a397d9b"
)

var sampleRPA = node.Addr{0x70, 0x81, 0x94, 0x0d, 0xfb, 0xaa}

func testHeadphone(cfg config.Headphone) *headphone {
	b := &Bridge{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return newHeadphone(b, cfg)
}

func TestMatchesAdvByIRK(t *testing.T) {
	h := testHeadphone(config.Headphone{ID: "x", BLEIRK: sampleIRK})
	if !h.matchesAdv(node.Adv{Addr: sampleRPA, AddrType: 1}) {
		t.Error("RPA not matched")
	}
	if h.matchesAdv(node.Adv{Addr: sampleRPA, AddrType: 0}) {
		t.Error("public address matched as RPA")
	}
	if h.takeIRKFound() != 0 {
		t.Error("verified IRK reported as found")
	}
	if n := len(h.scanFilters()); n != 1 {
		t.Errorf("%d filters, want 1", n)
	}
}

// A key copied from another system may be stored in reverse byte order: while
// unverified, both orders are tried and the matching one is reported.
func TestUnverifiedIRKReversed(t *testing.T) {
	k, _ := node.ParseIRK(sampleIRK)
	h := testHeadphone(config.Headphone{ID: "x", BLEIRK: k.Reversed().String(), BLEIRKUnverified: true})
	if n := len(h.scanFilters()); n != 2 {
		t.Errorf("%d filters while unverified, want 2", n)
	}
	if !h.matchesAdv(node.Adv{Addr: sampleRPA, AddrType: 1}) {
		t.Fatal("reversed key not tried")
	}
	if v := h.takeIRKFound(); v != 2 {
		t.Errorf("found %d, want 2 (reversed)", v)
	}
	if h.takeIRKFound() != 0 {
		t.Error("found reported twice")
	}
}
