package node

import (
	"testing"
	"time"
)

func TestDecodeBattery(t *testing.T) {
	p := []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 80, byte(BatteryApple), batteryChargingKnown | batteryCharging, 90, 0, 0, 0}
	msg, err := decode(MsgBattery, p)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := msg.(Battery)
	if !ok {
		t.Fatalf("decoded %T, want Battery", msg)
	}
	if b.Addr.String() != "12:34:56:78:9A:BC" || b.Percent != 80 || b.Source != BatteryApple || b.Age != 90*time.Second {
		t.Errorf("decoded %+v", b)
	}
	if b.Charging == nil || !*b.Charging {
		t.Errorf("charging %v, want true", b.Charging)
	}

	p[8] = 0 // charging state unknown
	msg, _ = decode(MsgBattery, p)
	if b := msg.(Battery); b.Charging != nil {
		t.Errorf("charging %v, want unknown", *b.Charging)
	}
	if _, err := decode(MsgBattery, p[:12]); err == nil {
		t.Error("short battery message accepted")
	}
}

func TestDecodeAdvCompany(t *testing.T) {
	p := []byte{0x70, 0x81, 0x94, 0x0d, 0xfb, 0xaa, 1, 0xC8, 0, 2, 'h', 'i', 0x2D, 0x01}
	msg, err := decode(MsgAdv, p)
	if err != nil {
		t.Fatal(err)
	}
	if a := msg.(Adv); a.Name != "hi" || a.Company != 0x012D || a.RSSI != -56 {
		t.Errorf("decoded %+v", a)
	}
	// Older nodes: no company ID.
	msg, _ = decode(MsgAdv, p[:12])
	if a := msg.(Adv); a.Company != -1 {
		t.Errorf("company %d without the field", a.Company)
	}
	msg, _ = decode(MsgAdv, append(p[:12:12], 0xFF, 0xFF))
	if a := msg.(Adv); a.Company != -1 {
		t.Errorf("company %d for 0xFFFF", a.Company)
	}
}

func TestDecodeBLEPair(t *testing.T) {
	p := make([]byte, 31)
	copy(p, []byte{0x70, 0x81, 0x94, 0x0d, 0xfb, 0xaa, BLEPairOK, 0, 0, 0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC})
	irk, _ := ParseIRK("ec0234a357c8ad05341010a60a397d9b")
	copy(p[15:], irk[:])
	msg, err := decode(MsgBLEPair, p)
	if err != nil {
		t.Fatal(err)
	}
	r := msg.(BLEPair)
	if r.Status != BLEPairOK || r.Identity.String() != "12:34:56:78:9A:BC" || r.IRK != irk || !r.IRK.Resolves(r.Addr) {
		t.Errorf("decoded %+v", r)
	}
	if _, err := decode(MsgBLEPair, p[:30]); err == nil {
		t.Error("short message accepted")
	}
}

func TestEncodeScanIRK(t *testing.T) {
	irk, _ := ParseIRK("ec0234a357c8ad05341010a60a397d9b")
	a := Addr{1, 2, 3, 4, 5, 6}
	p := encodeScan(ScanWatched, true, 100, 30, []ScanFilter{{Addr: &a}, {IRK: &irk}, {NamePrefix: "LE_"}})
	if p[6] != 3 {
		t.Fatalf("count %d", p[6])
	}
	if off := 7 + 2 + 6; p[off] != 2 || p[off+1] != 16 || IRK(p[off+2:off+18]) != irk {
		t.Errorf("IRK filter % x", p[off:off+18])
	}
}

// A newer node may send messages this bridge does not know; they must not end
// the session.
func TestDecodeUnknown(t *testing.T) {
	msg, err := decode(0x7E, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := msg.(Unknown); !ok || u.Type != 0x7E {
		t.Errorf("decoded %#v, want Unknown{0x7E}", msg)
	}
}
