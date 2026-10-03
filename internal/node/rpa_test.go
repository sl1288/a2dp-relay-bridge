package node

import "testing"

// Sample data of the ah function, Core Vol 3 Part H Appendix D.7.
func TestIRKResolvesSpecSample(t *testing.T) {
	k, err := ParseIRK("ec0234a357c8ad05341010a60a397d9b")
	if err != nil {
		t.Fatal(err)
	}
	rpa := Addr{0x70, 0x81, 0x94, 0x0d, 0xfb, 0xaa}
	if !k.Resolves(rpa) {
		t.Fatal("spec sample does not resolve")
	}
	other := rpa
	other[5] ^= 1
	if k.Resolves(other) {
		t.Error("wrong hash resolves")
	}
	static := Addr{0xc0 | 0x30, 0x81, 0x94, 0x0d, 0xfb, 0xaa}
	if k.Resolves(static) {
		t.Error("non-RPA resolves")
	}
	if !IsRPA(rpa, 1) || IsRPA(rpa, 0) || IsRPA(static, 1) {
		t.Error("IsRPA")
	}
	if _, err := ParseIRK("12"); err == nil {
		t.Error("short IRK accepted")
	}
}
