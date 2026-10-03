package node

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Conn is a connection to one relay node. Read must be called from a single
// goroutine; the command methods may be called concurrently.
type Conn struct {
	nc  net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

// Dial connects to a node ("host:port") and announces the protocol version.
func Dial(ctx context.Context, address string) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if tcp, ok := nc.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(10 * time.Second)
	}
	c := &Conn{nc: nc, r: bufio.NewReaderSize(nc, 16<<10)}
	if err := c.send(CmdHello, []byte{ProtocolVersion}); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.nc.Close() }

// RemoteAddr returns the node's network address.
func (c *Conn) RemoteAddr() string { return c.nc.RemoteAddr().String() }

// Read returns the next message (Hello, Status, Adv, ...).
func (c *Conn) Read() (any, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint16(hdr[1:])
	if n > maxPayload {
		return nil, fmt.Errorf("node: oversized message (%d bytes)", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return nil, err
	}
	return decode(hdr[0], p)
}

// SetReadDeadline bounds the next Read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }

func (c *Conn) send(typ byte, payload []byte) error {
	if len(payload) > maxPayload {
		return fmt.Errorf("node: payload too large (%d bytes)", len(payload))
	}
	buf := make([]byte, headerLen+len(payload))
	buf[0] = typ
	binary.LittleEndian.PutUint16(buf[1:], uint16(len(payload)))
	copy(buf[headerLen:], payload)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := c.nc.Write(buf)
	return err
}

// Scan configures BLE scanning on the node.
func (c *Conn) Scan(mode ScanMode, active bool, interval, window time.Duration, filters []ScanFilter) error {
	return c.send(CmdScan, encodeScan(mode, active, uint16(interval.Milliseconds()), uint16(window.Milliseconds()), filters))
}

// Inquiry starts a Classic discovery for about the given number of seconds (0 cancels).
func (c *Conn) Inquiry(seconds int) error {
	return c.send(CmdInquiry, []byte{byte(min(max(seconds, 0), 60))})
}

// Connect opens an A2DP connection to a headphone, pairing if necessary.
func (c *Conn) Connect(a Addr) error { return c.send(CmdConnect, a[:]) }

// Disconnect closes the A2DP connection (zero address: current peer).
func (c *Conn) Disconnect(a Addr) error { return c.send(CmdDisconnect, a[:]) }

// MediaStart starts the audio stream on the connected headphone.
func (c *Conn) MediaStart() error { return c.send(CmdMediaStart, nil) }

// MediaSuspend suspends the audio stream and drops queued audio.
func (c *Conn) MediaSuspend() error { return c.send(CmdMediaSuspend, nil) }

// Media queues one encoded media packet.
func (c *Conn) Media(timestamp, durationUs uint32, frames uint16, data []byte) error {
	return c.send(CmdMedia, encodeMedia(timestamp, durationUs, frames, data))
}

// SetVolume sets the headphone's absolute volume (0..127).
func (c *Conn) SetVolume(v uint8) error { return c.send(CmdSetVolume, []byte{min(v, 127)}) }

// GetBonds requests the list of paired headphones.
func (c *Conn) GetBonds() error { return c.send(CmdGetBonds, nil) }

// RemoveBond unpairs a headphone (Classic, and a BLE bond with this address).
func (c *Conn) RemoveBond(a Addr) error { return c.send(CmdRemoveBond, a[:]) }

// BLEPair pairs with the BLE advertiser at a to learn its identity key; the
// node answers with a BLEPair message.
func (c *Conn) BLEPair(a Addr, addrType uint8) error {
	return c.send(CmdBLEPair, append(a[:], addrType))
}

// Flush drops all queued media.
func (c *Conn) Flush() error { return c.send(CmdFlush, nil) }

// SetSBCCaps changes the SBC capabilities offered on the next connection.
func (c *Conn) SetSBCCaps(s SBCCaps) error {
	return c.send(CmdSetSBCCaps, []byte{s.SampleRates, s.ChannelModes, s.BlockLengths, s.Subbands, s.Allocation, s.MinBitpool, s.MaxBitpool})
}

// Ping asks the node to answer with a Pong carrying token.
func (c *Conn) Ping(token uint32) error {
	return c.send(CmdPing, binary.LittleEndian.AppendUint32(nil, token))
}

// VoiceStart opens the voice connection to the hands-free headphone.
func (c *Conn) VoiceStart() error { return c.send(CmdVoice, []byte{1}) }

// VoiceStop closes the voice connection and ends voice recognition.
func (c *Conn) VoiceStop() error { return c.send(CmdVoice, []byte{0}) }

// VoiceAudio queues one speaker packet (same format as VoiceAudio.Data).
func (c *Conn) VoiceAudio(data []byte) error { return c.send(CmdVoiceAudio, data) }

// SetCodecs sets the codec preference for the next connection the node initiates.
func (c *Conn) SetCodecs(p CodecPrefs) error { return c.send(CmdSetCodecs, encodeCodecs(p)) }

// SetScanMode controls Classic page/inquiry scan (connectable, discoverable).
func (c *Conn) SetScanMode(connectable, discoverable bool) error {
	b := func(v bool) byte {
		if v {
			return 1
		}
		return 0
	}
	return c.send(CmdSetScanMode, []byte{b(connectable), b(discoverable)})
}
