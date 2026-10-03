package stream

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/codec/aac"
	"github.com/sl1288/a2dp-relay-bridge/internal/codec/aptx"
	"github.com/sl1288/a2dp-relay-bridge/internal/codec/ldac"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// A typical audio MTU reported by headphones (882 bytes).
const testMTU = 882

func sine(rate int, seconds float64) []byte {
	n := int(float64(rate) * seconds)
	pcm := make([]byte, n*4)
	for i := range n {
		l := 0.3 * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
		r := 0.3 * math.Sin(2*math.Pi*1000*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(pcm[i*4:], uint16(int16(l*32767)))
		binary.LittleEndian.PutUint16(pcm[i*4+2:], uint16(int16(r*32767)))
	}
	return pcm
}

func mustConfig(t *testing.T, info []byte) node.CodecConfig {
	t.Helper()
	c, err := node.ParseCodecConfig(info)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// run feeds one second of audio in 10 ms chunks and checks the packets.
func run(t *testing.T, p Packetizer, rate int) []Packet {
	t.Helper()
	pcm := sine(rate, 1)
	chunk := rate / 100 * 4
	var pkts []Packet
	for off := 0; off+chunk <= len(pcm); off += chunk {
		out, err := p.Write(pcm[off : off+chunk])
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, out...)
	}
	var total time.Duration
	for i, pk := range pkts {
		if len(pk.Data) > testMTU {
			t.Fatalf("packet %d: %d bytes exceed the MTU", i, len(pk.Data))
		}
		if i > 0 && pk.Timestamp <= pkts[i-1].Timestamp {
			t.Fatalf("packet %d: timestamp %d not increasing", i, pk.Timestamp)
		}
		total += pk.Duration
	}
	// Encoder look-ahead and partial packets may hold back a little audio.
	if total < 900*time.Millisecond || total > time.Second {
		t.Fatalf("packets cover %v of 1 s", total)
	}
	t.Logf("%s: %d packets, %d kbit/s, %d..%d bytes", p.Describe(), len(pkts), p.Bitrate()/1000, minLen(pkts), maxLen(pkts))
	return pkts
}

func minLen(p []Packet) int {
	m := math.MaxInt
	for _, x := range p {
		m = min(m, len(x.Data))
	}
	return m
}

func maxLen(p []Packet) int {
	m := 0
	for _, x := range p {
		m = max(m, len(x.Data))
	}
	return m
}

func TestSBCPackets(t *testing.T) {
	p, err := New(mustConfig(t, []byte{6, 0x00, 0x00, 0x11, 0x15, 2, 53}), testMTU, Options{SBCMaxBitpool: 53})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pkts := run(t, p, 48000)
	if pkts[0].Frames != 7 {
		t.Errorf("%d SBC frames per packet, want 7", pkts[0].Frames)
	}
}

func TestLDACPackets(t *testing.T) {
	for _, q := range []ldac.Quality{ldac.HQ, ldac.SQ, ldac.MQ} {
		p, err := New(mustConfig(t, []byte{10, 0x00, 0xFF, 0x2D, 0x01, 0x00, 0x00, 0xAA, 0x00, 0x10, 0x01}),
			testMTU, Options{LDACQuality: q})
		if err != nil {
			t.Fatal(err)
		}
		pkts := run(t, p, 48000)
		for i, pk := range pkts {
			if int(pk.Data[0]) != int(pk.Frames) || pk.Frames == 0 || pk.Frames > 15 {
				t.Fatalf("%v packet %d: header %d, frames %d", q, i, pk.Data[0], pk.Frames)
			}
			if pk.Data[1] != 0xAA {
				t.Fatalf("%v packet %d: LDAC frame does not start with the sync byte", q, i)
			}
		}
		p.Close()
	}
}

func TestAptXRoundTrip(t *testing.T) {
	for _, hd := range []bool{false, true} {
		info := []byte{9, 0x00, 0xFF, 0x4F, 0x00, 0x00, 0x00, 0x01, 0x00, 0x12}
		if hd {
			info = []byte{13, 0x00, 0xFF, 0xD7, 0x00, 0x00, 0x00, 0x24, 0x00, 0x12, 0, 0, 0, 0}
		}
		p, err := New(mustConfig(t, info), testMTU, Options{})
		if err != nil {
			t.Fatal(err)
		}
		pkts := run(t, p, 48000)
		p.Close()

		dec, err := aptx.NewDecoder(hd)
		if err != nil {
			t.Fatal(err)
		}
		var out []int32
		for _, pk := range pkts {
			s24, err := dec.Decode(pk.Data)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+2 < len(s24); i += 3 {
				out = append(out, int32(uint32(s24[i])|uint32(s24[i+1])<<8|uint32(s24[i+2])<<16)<<8>>8)
			}
		}
		dec.Close()
		// The decoded left channel must be a 440 Hz sine at -10.5 dBFS: check
		// its RMS against the expected value after the codec delay.
		var sum float64
		n := 0
		for i := 20000; i+1 < len(out); i += 2 {
			v := float64(out[i]) / (1 << 23)
			sum += v * v
			n++
		}
		rms := math.Sqrt(sum / float64(n))
		want := 0.3 / math.Sqrt2
		if math.Abs(rms-want) > 0.02 {
			t.Errorf("hd=%v: decoded RMS %.3f, want %.3f", hd, rms, want)
		}
	}
}

func TestAACRoundTrip(t *testing.T) {
	cfg := mustConfig(t, []byte{8, 0x00, 0x02, 0x80, 0x00, 0x84, 0x04, 0xE2, 0x00})
	p, err := New(cfg, testMTU, Options{AACMaxBitrate: 320000})
	if err != nil {
		t.Fatal(err)
	}
	pkts := run(t, p, 48000)
	p.Close()

	dec, err := aac.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	decoded := 0
	for _, pk := range pkts {
		s, err := dec.Decode(pk.Data)
		if err != nil {
			t.Fatal(err)
		}
		decoded += len(s)
	}
	rate, ch := dec.StreamInfo()
	if rate != 48000 || ch != 2 {
		t.Fatalf("decoder sees %d Hz, %d channels", rate, ch)
	}
	if decoded < len(pkts)*1024*2/2 {
		t.Fatalf("decoded only %d samples from %d packets", decoded, len(pkts))
	}
}
