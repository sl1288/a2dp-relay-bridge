package node

import (
	"crypto/aes"
	"encoding/hex"
	"fmt"
)

// IRK is an identity resolving key, most significant byte first.
type IRK [16]byte

// ParseIRK parses 32 hex digits (spaces and colons are ignored).
func ParseIRK(s string) (IRK, error) {
	var k IRK
	clean := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		if c != ' ' && c != ':' && c != '-' {
			clean = append(clean, c)
		}
	}
	b, err := hex.DecodeString(string(clean))
	if err != nil || len(b) != len(k) {
		return k, fmt.Errorf("invalid IRK %q: need 32 hex digits", s)
	}
	copy(k[:], b)
	return k, nil
}

func (k IRK) String() string { return hex.EncodeToString(k[:]) }

// Reversed returns the key with the byte order swapped. Systems disagree on
// the order they store keys in; only resolving an address tells which is right.
func (k IRK) Reversed() IRK {
	var r IRK
	for i := range k {
		r[i] = k[len(k)-1-i]
	}
	return r
}

// IsRPA reports whether a is a resolvable private address (random address
// whose two most significant bits are 01).
func IsRPA(a Addr, addrType uint8) bool { return addrType == 1 && a[0]&0xC0 == 0x40 }

// Resolves reports whether the resolvable private address a was generated
// from k: its low 24 bits are ah(k, prand) = e(k, 0^104 || prand) mod 2^24
// (Core Vol 3 Part H 2.2.2), prand being the high 24 bits.
func (k IRK) Resolves(a Addr) bool {
	if a[0]&0xC0 != 0x40 {
		return false
	}
	c, err := aes.NewCipher(k[:])
	if err != nil {
		return false
	}
	var in, out [16]byte
	copy(in[13:], a[:3])
	c.Encrypt(out[:], in[:])
	return out[13] == a[3] && out[14] == a[4] && out[15] == a[5]
}
