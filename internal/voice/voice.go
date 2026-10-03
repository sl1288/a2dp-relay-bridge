// Package voice handles hands-free voice packets: mSBC frames in their H2
// synchronisation header (wide band speech) and CVSD as linear PCM.
package voice

import (
	"encoding/binary"
	"os"
)

// H2 sequence bytes for the four packet numbers (HFP 1.7, 5.7.1).
var h2Seq = [4]byte{0x08, 0x38, 0xC8, 0xF8}

const (
	h2Sync   = 0x01
	msbcSync = 0xAD
	// MSBCPacket is the eSCO packet size for mSBC: H2 header, frame, one pad byte.
	MSBCPacket = 2 + 57 + 1
)

func validSeq(b byte) bool {
	for _, s := range h2Seq {
		if b == s {
			return true
		}
	}
	return false
}

// Unpacker extracts 57-byte mSBC frames from a stream of eSCO payloads that
// need not be aligned with the frames.
type Unpacker struct {
	buf []byte
}

// Push adds received data and returns the complete frames found.
func (u *Unpacker) Push(data []byte) [][]byte {
	u.buf = append(u.buf, data...)
	var frames [][]byte
	i := 0
	for i+2+57 <= len(u.buf) {
		if u.buf[i] == h2Sync && validSeq(u.buf[i+1]) && u.buf[i+2] == msbcSync {
			frames = append(frames, append([]byte(nil), u.buf[i+2:i+2+57]...))
			i += 2 + 57
			continue
		}
		i++
	}
	u.buf = append(u.buf[:0], u.buf[i:]...)
	return frames
}

// Packer wraps mSBC frames into eSCO packets with a running sequence number.
type Packer struct {
	seq int
}

// Pack returns one 60-byte packet for a 57-byte frame.
func (p *Packer) Pack(frame []byte) []byte {
	pkt := make([]byte, MSBCPacket)
	pkt[0] = h2Sync
	pkt[1] = h2Seq[p.seq&3]
	p.seq++
	copy(pkt[2:], frame)
	return pkt
}

// WriteWAV stores mono 16-bit PCM as a WAV file.
func WriteWAV(path string, sampleRate int, pcm []byte) error {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return os.WriteFile(path, append(h, pcm...), 0o644)
}
