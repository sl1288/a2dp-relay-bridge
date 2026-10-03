package voice

import (
	"bytes"
	"testing"
)

// Packets survive a round trip even when the receiver gets them split at
// arbitrary points, and garbage between them is skipped.
func TestPackUnpack(t *testing.T) {
	var p Packer
	var stream []byte
	var want [][]byte
	for i := range 6 {
		frame := bytes.Repeat([]byte{byte(i)}, 57)
		frame[0] = 0xAD
		want = append(want, frame)
		stream = append(stream, p.Pack(frame)...)
		if i == 2 {
			stream = append(stream, 0x55, 0x01, 0x99) // noise
		}
	}
	var u Unpacker
	var got [][]byte
	for len(stream) > 0 {
		n := min(23, len(stream))
		got = append(got, u.Push(stream[:n])...)
		stream = stream[n:]
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d differs", i)
		}
	}
}
